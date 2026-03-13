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
	parts := []string{response}
	if strings.Contains(response, "\x00") {
		parts = strings.Split(response, "\x00")
	}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		b.sendHTMLChunks(msg.Chat.Id, ConvertToTelegramHTML(part, b.tableOpts()))
	}
}

// sendReplyWithFullThinking sends thinking (italic) + divider + response as a single message.
// Thinking and response are converted to HTML separately to avoid markdown interference.
func (b *Bot) sendReplyWithFullThinking(msg *gotgbot.Message, response, thinkingText string) {
	thinkingHTML := "<i>" + htmlEscapeBot(thinkingText) + "</i>"
	responseHTML := ConvertToTelegramHTML(response, b.tableOpts())
	divider := "\n" + strings.Repeat("—", b.effectiveDisplayWidth()) + "\n\n"
	b.sendHTMLChunks(msg.Chat.Id, thinkingHTML+divider+responseHTML)
}

// sendReplyWithThinking sends a response with a "Show thinking" inline keyboard button.
// The thinking content is stored for later toggle via callback query.
func (b *Bot) sendReplyWithThinking(msg *gotgbot.Message, response, thinkingText string) {
	responseHTML := ConvertToTelegramHTML(response, b.tableOpts())

	// Send with placeholder button (msgID unknown until sent)
	sendOpts := &gotgbot.SendMessageOpts{
		ParseMode:   "HTML",
		ReplyMarkup: singleButtonKeyboard("Show thinking", "th:show:0"),
	}

	// If response is too long to fit with a button, chunk it — send all but last
	// chunk as plain, then send last chunk with button.
	chunks := splitMessage(responseHTML, 4096)
	for i, chunk := range chunks {
		if i < len(chunks)-1 {
			// Non-last chunks: use sendHTMLChunks for fallback + logging
			b.sendHTMLChunks(msg.Chat.Id, chunk)
			continue
		}
		// Last chunk — send with button
		sent, err := b.client.SendMessage(msg.Chat.Id, chunk, sendOpts)
		if err != nil {
			b.logger().Errorf("send reply with thinking button: %v", err)
			return
		}
		// Update button with real message ID and store thinking data
		kb := singleButtonKeyboard("Show thinking", fmt.Sprintf("th:show:%d", sent.MessageId))
		_, _, _ = b.client.EditMessageText(chunk, &gotgbot.EditMessageTextOpts{
			ChatId:      msg.Chat.Id,
			MessageId:   sent.MessageId,
			ParseMode:   "HTML",
			ReplyMarkup: kb,
		})
		b.thinkingStore.Store(sent.MessageId, thinkingEntry{
			responseHTML: chunk,
			thinkingText: thinkingText,
		})
	}
}

// SendNotification sends a plain text notification to the default chat.
// Used for system alerts (cache bust, etc.) — not an agent turn, no tokens spent.
// Silently skips empty or whitespace-only messages.
func (b *Bot) SendNotification(text string) {
	if strings.TrimSpace(text) == "" {
		b.logger().Debugf("skipping empty notification")
		return
	}

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
	if b.injectedMessageHeader != "" && strings.TrimSpace(text) != "" {
		text = b.injectedMessageHeader + "\n" + text
	}
	return b.SendText(text)
}

// SendToSession sends a system/injected text message to the chat associated
// with the given session key. Falls back to the bot's default chat if the
// session key doesn't contain a chat ID (e.g. independent sessions).
// Prepends the configured InjectedMessageHeader (if non-empty).
func (b *Bot) SendToSession(sessionKey, text string) error {
	if b.injectedMessageHeader != "" && strings.TrimSpace(text) != "" {
		text = b.injectedMessageHeader + "\n" + text
	}
	return b.SendTextToSession(sessionKey, text)
}

// SendTextToSession sends a text message (without header) to the chat
// associated with the given session key. Falls back to the bot's default chat
// if the session key doesn't contain a chat ID.
func (b *Bot) SendTextToSession(sessionKey, text string) error {
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
