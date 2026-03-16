package telegram

import (
	"fmt"
	"strings"
	"time"

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
}

// sendReply sends a response back to the user, splitting long messages and
// falling back to plain text if HTML formatting fails.
func (b *Bot) sendReply(msg *gotgbot.Message, response string) {
	response = strings.TrimSpace(response)
	if response == "" {
		return
	}
	b.sendHTMLChunks(msg.Chat.Id, ConvertToTelegramHTML(response, b.tableOpts()))
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

// SendTyping sends a "typing" chat action to the bot's default chat.
func (b *Bot) SendTyping() {
	_, _ = b.client.SendChatAction(b.chatID, "typing", nil)
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
	chatID := b.defaultChatID()
	if chatID == 0 {
		// Fall back to last known chat (e.g. when no state store is configured).
		b.chatMu.Lock()
		chatID = b.chatID
		b.chatMu.Unlock()
	}
	if chatID == 0 {
		b.logger().Warnf("no chat ID for notification: %s", text)
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

// SendText sends a text message to the default chat without any header.
// Returns an error if no chat ID is available.
// Silently skips empty or whitespace-only messages.
func (b *Bot) SendText(text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	chatID := b.defaultChatID()
	if chatID == 0 {
		// Fall back to last known chat (e.g. when no state store is configured).
		b.chatMu.Lock()
		chatID = b.chatID
		b.chatMu.Unlock()
	}
	if chatID == 0 {
		return fmt.Errorf("no chat ID — no default chat configured")
	}

	b.sendHTMLChunks(chatID, ConvertToTelegramHTML(text, b.tableOpts()))
	return nil
}

// SendInjected sends a system/injected text message to the default chat.
// Prepends the configured InjectedMessageHeader (if non-empty) so users can
// distinguish system messages from agent replies.
//
// Prefer SendToSession when a session key is available — it routes to the
// correct chat for chat-based sessions.
func (b *Bot) SendInjected(text string) error {
	if b.display.InjectedMessageHeader != "" && strings.TrimSpace(text) != "" {
		text = b.display.InjectedMessageHeader + "\n" + text
	}
	return b.SendText(text)
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
func (b *Bot) SendToSession(sessionKey, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	chatID := session.ChatIDFromKey(sessionKey)
	if chatID == 0 {
		chatID = b.defaultChatID()
	}
	if chatID == 0 {
		return fmt.Errorf("no chat ID for session %q and no default chat", sessionKey)
	}

	b.sendHTMLChunks(chatID, ConvertToTelegramHTML(text, b.tableOpts()))
	return nil
}
