package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"foci/internal/platform"
	"foci/internal/session"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// sendHTMLChunks sends pre-converted HTML to a chat, splitting into chunks
// and falling back to plain text if HTML parsing fails.
func (b *Bot) sendHTMLChunks(chatID int64, html string) {
	for _, chunk := range splitMessage(html, 4096) {
		if _, err := b.client.SendMessage(chatID, chunk, &gotgbot.SendMessageOpts{ParseMode: "HTML"}); err != nil {
			if _, err := b.client.SendMessage(chatID, chunk, nil); err != nil {
				b.logger().Errorf("send error: %s", b.sanitizeError(err))
			}
		}
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
	b.turnMu.Lock()
	active := b.turnCancel != nil
	b.turnMu.Unlock()
	if active {
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
func (b *Bot) SendNotificationDirect(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	b.sendNotificationImmediate(text)
}

// sendNotificationImmediate sends a notification directly to the default chat.
func (b *Bot) sendNotificationImmediate(text string) {
	chatID := b.DefaultChatID()
	if chatID == 0 {
		// Fall back to last known chat (e.g. when no state store is configured).
		b.chatMu.Lock()
		chatID = b.chatID
		b.chatMu.Unlock()
	}
	if chatID == 0 {
		truncated := text
		if len(truncated) > 40 {
			truncated = truncated[:40] + "..."
		}
		b.logger().Warnf("no chat ID for notification: %s", truncated)
		return
	}

	if _, err := b.client.SendMessage(chatID, text, nil); err != nil {
		b.logger().Errorf("send notification: %s", b.sanitizeError(err))
	}
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

	if _, err := b.client.SendMessage(chatID, text, nil); err != nil {
		b.logger().Errorf("send startup notification: %s", b.sanitizeError(err))
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
	chatID := b.DefaultChatID()
	if chatID == 0 {
		b.chatMu.Lock()
		chatID = b.chatID
		b.chatMu.Unlock()
	}
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
// Returns an error if no chat ID is available.
// Delegates to SendTextToChat, which handles IsSilent filtering.
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
// if the session key doesn't contain a chat ID.
// Delegates to SendTextToChat, which handles IsSilent filtering.
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
