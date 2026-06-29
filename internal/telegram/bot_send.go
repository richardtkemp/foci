package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"foci/internal/platform"
	"foci/internal/session"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// Telegram send-side flood-control (429) retry bounds. A 429 carries a
// server-instructed Retry-After; rather than silently dropping the message
// (the old behaviour — #795), the send waits and retries up to maxFloodRetries.
const (
	maxFloodRetries = 3
	maxFloodWait    = 30 * time.Second
)

// floodSleep is the sleep used between flood-control retries; a package var so
// tests can stub it out and not actually block.
var floodSleep = time.Sleep

// retryOn429 runs send and, on a Telegram 429 flood-control error, waits the
// server-instructed Retry-After (padded 1s for clock/roundtrip slack, capped at
// maxFloodWait) and retries, up to maxFloodRetries times. Non-429 errors and
// success return immediately, so callers keep their existing handling (e.g. the
// HTML→plain-text fallback) for everything except rate limiting.
//
// send MUST be safe to invoke more than once: media callers reopen the file on
// each attempt (see sendMedia's contract). This mirrors the poll path's
// Retry-After honour (bot_poll.go) so a throttled send queues-and-retries
// instead of dropping the user's message.
func (b *Bot) retryOn429(op string, send func() error) error {
	for attempt := 0; ; attempt++ {
		err := send()
		if err == nil {
			return nil
		}
		var tgErr *gotgbot.TelegramError
		if !errors.As(err, &tgErr) || tgErr.ResponseParams == nil || tgErr.ResponseParams.RetryAfter <= 0 {
			return err // not flood control — let the caller handle it
		}
		if attempt >= maxFloodRetries {
			b.logger().Errorf("%s: still flood-limited after %d retries, giving up: %s", op, maxFloodRetries, b.sanitizeError(err))
			return err
		}
		wait := time.Duration(tgErr.ResponseParams.RetryAfter)*time.Second + time.Second
		if wait > maxFloodWait {
			wait = maxFloodWait
		}
		b.logger().Warnf("%s: Telegram 429 flood control, waiting %v then retrying (attempt %d/%d)", op, wait, attempt+1, maxFloodRetries)
		floodSleep(wait)
	}
}

// sendHTMLWithFallback sends html to chatID with HTML parse mode, retrying as a
// plain-text send if the HTML attempt errors. It logs the HTML-attempt failure
// so an ambiguous timeout — where Telegram may already have delivered the
// message, producing a duplicate (rendered HTML first, raw tags second) — is
// diagnosable. The client is passed explicitly so the streaming sink can supply
// its own (test-injectable) client. Returns the delivered message, or ok=false
// if both attempts fail.
func sendHTMLWithFallback(b *Bot, client botClient, chatID int64, html string) (*gotgbot.Message, bool) {
	var msg *gotgbot.Message
	err := b.retryOn429("send", func() error {
		var e error
		msg, e = client.SendMessage(chatID, html, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
		return e
	})
	if err != nil {
		b.logger().Warnf("send: HTML attempt failed (%s); retrying as plain text — if this was a timeout, Telegram may already have delivered, producing a duplicate", b.sanitizeError(err))
		err = b.retryOn429("send", func() error {
			var e error
			msg, e = client.SendMessage(chatID, html, nil)
			return e
		})
		if err != nil {
			b.logger().Errorf("send: plain-text fallback also failed: %s", b.sanitizeError(err))
			return nil, false
		}
	}
	return msg, true
}

// sendHTMLChunks sends pre-converted HTML to a chat, splitting into chunks
// and falling back to plain text if HTML parsing fails.
func (b *Bot) sendHTMLChunks(chatID int64, html string) {
	for _, chunk := range splitMessage(html, 4096) {
		sendHTMLWithFallback(b, b.client, chatID, chunk)
	}
	// Telegram auto-cancels typing when a message is sent.
	// Re-establish it immediately if a turn is still active.
	b.refreshTyping()
}

// sendReply sends a response back to the user, splitting long messages and
// falling back to plain text if HTML formatting fails.
func (b *Bot) sendReply(msg *gotgbot.Message, response string) {
	_ = b.SendTextToChat(msg.Chat.Id, response)
}

// SendNotification sends a plain text notification to the default chat.
// Used for system alerts (cache bust, etc.) — not an agent turn, no tokens spent.
// Silently skips empty or whitespace-only messages.
// If an agent turn is active, the notification is buffered and sent after the turn ends.
func (b *Bot) SendNotification(text string) {
	if strings.TrimSpace(text) == "" {
		b.logger().Debugf("skipping empty notification")
		return
	}

	// Buffer during active turns to avoid interrupting streaming output.
	if b.turnActive.Load() {
		b.pendingNotifsMu.Lock()
		b.pendingNotifs = append(b.pendingNotifs, text)
		b.pendingNotifsMu.Unlock()
		return
	}

	b.sendNotificationImmediate(text)
}

// SetTyping starts or stops the typing indicator. When true, sends an
// immediate typing action and starts a 4-second ticker (Telegram's typing
// expires after ~5s). When false, cancels the ticker goroutine.
//
// Typing lifecycle:
//
//  1. processAgentMessage starts typing and defers SetTyping(false) as a
//     safety net for errors/cancellation.
//  2. TypingFunc (delegated backend): the backend calls typingFunc(true) on
//     SendToPane and typingFunc(false) on turn completion or error. This is
//     the primary stop mechanism for delegated turns.
//  3. tryDispatchViaDispatcher: SetTyping(false) after command completion.
//  4. refreshTyping(): re-sends typing action after each message send to
//     counter Telegram's auto-cancel-on-message behaviour.
func (b *Bot) SetTyping(typing bool) {
	b.typingMu.Lock()
	defer b.typingMu.Unlock()

	if !typing {
		if b.typingCancel != nil {
			b.typingCancel()
			b.typingCancel = nil
		}
		return
	}

	// Already typing — don't start a second ticker.
	if b.typingCancel != nil {
		return
	}

	_, _ = b.client.SendChatAction(b.chatID, "typing", nil)
	ctx, cancel := context.WithCancel(context.Background())
	b.typingCancel = cancel
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_, _ = b.client.SendChatAction(b.chatID, "typing", nil)
			case <-ctx.Done():
				return
			}
		}
	}()
}

// refreshTyping re-sends the typing action immediately if a typing ticker
// is active. Telegram auto-cancels typing when any message is sent to the
// chat; this re-establishes it without waiting for the next ticker interval
// (up to 4 seconds). No-op if typing isn't active.
func (b *Bot) refreshTyping() {
	b.typingMu.Lock()
	active := b.typingCancel != nil
	b.typingMu.Unlock()
	if active {
		_, _ = b.client.SendChatAction(b.chatID, "typing", nil)
	}
}

// SendNotificationDirect sends a notification immediately, bypassing the
// turn-active buffer. Use for time-sensitive notifications (e.g. compaction start)
// that must arrive before the turn ends.
func (b *Bot) SendNotificationDirect(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return b.sendNotificationImmediate(text)
}

// sendNotificationImmediate sends a notification directly to the default chat.
// Returns the platform message ID as a string, or "" on failure.
func (b *Bot) sendNotificationImmediate(text string) string {
	return b.sendNotificationToChat(b.defaultChatIDOrLast(), text)
}

// defaultChatIDOrLast resolves the bot's default chat, falling back to the last
// known chat when no state store is configured (e.g. tests).
func (b *Bot) defaultChatIDOrLast() int64 {
	chatID := b.DefaultChatID()
	if chatID == 0 {
		b.chatMu.Lock()
		chatID = b.chatID
		b.chatMu.Unlock()
	}
	return chatID
}

// chatIDForSession resolves the destination chat for a session key. The Telegram
// chatID is embedded in the session key (clutch/c<chatID>/<instance>), so it is
// extracted directly — no chat_metadata round-trip. Falls back to the default
// chat when the key carries no chatID (unparseable / independent session) (#911).
func (b *Bot) chatIDForSession(sessionKey string) int64 {
	if id := session.ChatIDFromKey(sessionKey); id != 0 {
		return id
	}
	return b.defaultChatIDOrLast()
}

// sendNotificationToChat sends a notification to a specific chat. Returns the
// first chunk's message ID as the anchor (for later in-place edits), or "" on
// failure / no chat.
func (b *Bot) sendNotificationToChat(chatID int64, text string) string {
	if chatID == 0 {
		truncated := text
		if len(truncated) > 40 {
			truncated = truncated[:40] + "..."
		}
		b.logger().Warnf("no chat ID for notification: %s", truncated)
		return ""
	}

	// Chunk to respect Telegram's 4096-char cap — an over-length notification
	// (e.g. startup proactive-warnings) is otherwise rejected (#810). Returns the
	// first chunk's message ID as the anchor.
	var firstID string
	for _, chunk := range splitMessage(text, 4096) {
		msg, err := b.client.SendMessage(chatID, chunk, nil)
		if err != nil {
			b.logger().Errorf("send notification: %s", b.sanitizeError(err))
			continue
		}
		if firstID == "" {
			firstID = strconv.FormatInt(msg.MessageId, 10)
		}
	}
	return firstID
}

// SendNotificationToSession sends a notification to the chat that owns
// sessionKey, rather than the bot's default chat. This fixes multi-user
// misrouting: when two users DM the same primary bot, a per-session notice (e.g.
// a compaction notice for the second user) must land in THAT user's chat, not the
// default (first user's) chat (#911). Implements platform.SessionNotifier.
func (b *Bot) SendNotificationToSession(sessionKey, text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return b.sendNotificationToChat(b.chatIDForSession(sessionKey), text)
}

// drainPendingNotifications sends all buffered notifications to the default chat.
// Called after an agent turn ends. Atomically swaps the buffer to nil so new
// notifications arriving during drain go directly via sendNotificationImmediate.
func (b *Bot) drainPendingNotifications() {
	b.pendingNotifsMu.Lock()
	notifs := b.pendingNotifs
	b.pendingNotifs = nil
	b.pendingNotifsMu.Unlock()

	for _, text := range notifs {
		b.sendNotificationImmediate(text)
	}
}

// SendStartupNotification sends a startup notification to the last known chat.
// Skips silently if no chat ID is available (expected on first run or fresh state).
func (b *Bot) SendStartupNotification(agentID string) {
	b.SendStartupNotificationWithDiagnosis(agentID, nil)
}

// StartupDiagnosis is an interface for the diagnosis result from the startup package.
// Using an interface avoids importing the startup package (would create a cycle).
type StartupDiagnosis interface {
	FormatNotification() string
}

// SendStartupNotificationWithDiagnosis sends a startup notification with optional diagnosis info.
// If diagnosis is nil or produces no additional text, sends a simple restart message.
func (b *Bot) SendStartupNotificationWithDiagnosis(agentID string, diagnosis StartupDiagnosis) {
	b.chatMu.Lock()
	chatID := b.chatID
	b.chatMu.Unlock()

	if chatID == 0 {
		b.logger().Debugf("no chat ID for startup notification (no prior messages)")
		return
	}

	botName := b.Username()
	if botName == "" {
		botName = "foci"
	}
	text := fmt.Sprintf("%s restarted at %s", botName, time.Now().Format("15:04:05"))

	if diagnosis != nil {
		if extra := diagnosis.FormatNotification(); extra != "" {
			text = fmt.Sprintf("%s\n\n%s", text, extra)
		}
	}

	// Chunk so an over-length diagnosis body respects Telegram's 4096-char cap (#810).
	for _, chunk := range splitMessage(text, 4096) {
		if _, err := b.client.SendMessage(chatID, chunk, nil); err != nil {
			b.logger().Errorf("send startup notification: %s", b.sanitizeError(err))
		}
	}
}

// SendTextWithButtons sends a text message with inline keyboard buttons.
// callbackPrefix is prepended to each button's Data for callback routing.
// Returns the platform message ID (as string) for later editing.
func (b *Bot) SendTextWithButtons(text string, buttons []platform.ButtonChoice, callbackPrefix string) (string, error) {
	chatID := b.DefaultChatID()
	if chatID == 0 {
		b.chatMu.Lock()
		chatID = b.chatID
		b.chatMu.Unlock()
	}
	if chatID == 0 {
		return "", fmt.Errorf("no chat ID — no default chat configured")
	}

	rows := buildButtonRows(buttons, callbackPrefix)
	msg, err := b.client.SendMessage(chatID, ConvertToTelegramHTML(text, b.tableOpts()), &gotgbot.SendMessageOpts{
		ParseMode: "HTML",
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{
			InlineKeyboard: rows,
		},
	})
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(msg.MessageId, 10), nil
}

// EditMessageText edits an existing message's text and removes buttons.
func (b *Bot) EditMessageText(msgID string, text string) error {
	return b.editMessageInChat(b.defaultChatIDOrLast(), msgID, text)
}

// editMessageInChat edits a message in a specific chat (Telegram's
// editMessageText needs both chatID and msgID — a msgID alone is ambiguous
// across chats).
func (b *Bot) editMessageInChat(chatID int64, msgID, text string) error {
	id, _ := strconv.ParseInt(msgID, 10, 64)
	_, _, err := b.client.EditMessageText(
		ConvertToTelegramHTML(text, b.tableOpts()),
		&gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: id,
			ParseMode: "HTML",
		})
	return err
}

// EditNotificationInSession edits a message in the chat that owns sessionKey,
// rather than the default chat. The compaction flow sends a ⏳ start notice and
// edits it in place to ✅ — both must target the session's chat, else the edit
// hits the wrong chat (and Telegram rejects it, since the msgID belongs to the
// session's chat) (#911). Implements platform.SessionNotifier.
func (b *Bot) EditNotificationInSession(sessionKey, msgID, text string) error {
	return b.editMessageInChat(b.chatIDForSession(sessionKey), msgID, text)
}

// EditMessageWithButtons edits an existing message's text and replaces its buttons.
func (b *Bot) EditMessageWithButtons(msgID string, text string, buttons []platform.ButtonChoice, callbackPrefix string) error {
	chatID := b.DefaultChatID()
	if chatID == 0 {
		b.chatMu.Lock()
		chatID = b.chatID
		b.chatMu.Unlock()
	}
	id, _ := strconv.ParseInt(msgID, 10, 64)
	rows := buildButtonRows(buttons, callbackPrefix)
	_, _, err := b.client.EditMessageText(
		ConvertToTelegramHTML(text, b.tableOpts()),
		&gotgbot.EditMessageTextOpts{
			ChatId:    chatID,
			MessageId: id,
			ParseMode: "HTML",
			ReplyMarkup: gotgbot.InlineKeyboardMarkup{
				InlineKeyboard: rows,
			},
		})
	return err
}

// buildButtonRows converts platform.ButtonChoice slices into Telegram inline keyboard rows.
// Buttons are grouped by their Row field, and each row is auto-laid-out via layoutButtons.
func buildButtonRows(buttons []platform.ButtonChoice, callbackPrefix string) [][]gotgbot.InlineKeyboardButton {
	rowMap := make(map[int][]gotgbot.InlineKeyboardButton)
	maxRow := 0
	for _, btn := range buttons {
		rowMap[btn.Row] = append(rowMap[btn.Row], gotgbot.InlineKeyboardButton{
			Text:         btn.Label,
			CallbackData: callbackPrefix + btn.Data,
		})
		if btn.Row > maxRow {
			maxRow = btn.Row
		}
	}
	var rows [][]gotgbot.InlineKeyboardButton
	for i := 0; i <= maxRow; i++ {
		if btns, ok := rowMap[i]; ok {
			rows = append(rows, layoutButtons(btns)...)
		}
	}
	return rows
}

// SendText sends a text message to the default chat without any header.
// Returns an error if no chat ID is available. Sentinel/silent filtering is
// handled upstream — at the renderer (OnReply/Finalize) for interactive
// turns and at SessionSink for injected/notify flows; this method does not
// re-check.
func (b *Bot) SendText(text string) error {
	chatID := b.DefaultChatID()
	if chatID == 0 {
		// Fall back to last known chat (e.g. when no state store is configured).
		b.chatMu.Lock()
		chatID = b.chatID
		b.chatMu.Unlock()
	}
	if chatID == 0 {
		return fmt.Errorf("no chat ID — no default chat configured")
	}
	return b.SendTextToChat(chatID, text)
}

// SendInjectedMessage sends a system/injected text message to the chat
// associated with the given session key. Falls back to the bot's default chat
// if the session key doesn't contain a chat ID (e.g. independent sessions).
// Prepends the configured InjectedMessageHeader (if non-empty).
func (b *Bot) SendInjectedMessage(sessionKey, text string) error {
	if b.display.InjectedMessageHeader != "" && strings.TrimSpace(text) != "" {
		text = b.display.InjectedMessageHeader + "\n" + text
	}
	return b.SendToSession(sessionKey, text)
}

// SendToSession sends a text message (without header) to the chat
// associated with the given session key. Falls back to the bot's default chat
// if the session key doesn't contain a chat ID. Sentinel/silent filtering
// is handled upstream by SessionSink before this method is reached.
func (b *Bot) SendToSession(sessionKey, text string) error {
	chatID := session.ChatIDFromKey(sessionKey)
	if chatID == 0 {
		chatID = b.DefaultChatID()
	}
	if chatID == 0 {
		return fmt.Errorf("no chat ID for session %q and no default chat", sessionKey)
	}
	return b.SendTextToChat(chatID, text)
}
