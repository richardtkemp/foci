package telegram

import (
	"encoding/json"
	"fmt"
	"strconv"

	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/turn"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// newToolCallTracker creates a shared turn.ToolCallTracker backed by
// Telegram-specific formatting and messaging.
func newToolCallTracker(bot *Bot, chatID int64, d turn.TurnDisplay) *turn.ToolCallTracker {
	backend := &telegramTrackerBackend{bot: bot, chatID: chatID}
	display := turn.TrackerDisplay{ShowToolCalls: d.ShowToolCalls}
	return turn.NewToolCallTracker(backend, &bot.toolStore, display, compactResultHint)
}

// telegramTrackerBackend implements turn.TrackerBackend for Telegram.
type telegramTrackerBackend struct {
	bot    *Bot
	chatID int64
}

func (b *telegramTrackerBackend) FormatCompact(toolName string, params json.RawMessage) string {
	return formatToolCallCompact(toolName, params)
}

func (b *telegramTrackerBackend) FormatFull(toolName string, params json.RawMessage, showMode string) string {
	return b.bot.formatToolCall(toolName, params, showMode)
}

func (b *telegramTrackerBackend) FormatWithResult(toolText, result string) string {
	return formatToolCallWithResult(toolText, result)
}

func (b *telegramTrackerBackend) FormatHintSuffix(hint string) string {
	return " → " + htmlEscape(hint)
}

func (b *telegramTrackerBackend) FormatRetry(endpoint string) string {
	return fmt.Sprintf("⏳ <i>%s is busy right now, retrying...</i>", endpoint)
}

func (b *telegramTrackerBackend) FormatRetryClear() string {
	return "✓ <i>Request completed</i>"
}

func (b *telegramTrackerBackend) Send(text string) (string, error) {
	sent, err := b.bot.client.SendMessage(b.chatID, text, &gotgbot.SendMessageOpts{
		ParseMode: "HTML",
	})
	if err != nil {
		return "", err
	}
	b.bot.refreshTyping()
	return strconv.FormatInt(sent.MessageId, 10), nil
}

func (b *telegramTrackerBackend) SendWithButton(text, btnLabel, btnData string) (string, error) {
	rows := buildButtonRows([]platform.ButtonChoice{{Label: btnLabel, Data: btnData}}, "")
	sent, err := b.bot.client.SendMessage(b.chatID, text, &gotgbot.SendMessageOpts{
		ParseMode: "HTML",
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{
			InlineKeyboard: rows,
		},
	})
	if err != nil {
		return "", err
	}
	b.bot.refreshTyping()
	return strconv.FormatInt(sent.MessageId, 10), nil
}

func (b *telegramTrackerBackend) Edit(msgID, text string) error {
	id, _ := strconv.ParseInt(msgID, 10, 64)
	_, _, err := b.bot.client.EditMessageText(text, &gotgbot.EditMessageTextOpts{
		ChatId:    b.chatID,
		MessageId: id,
		ParseMode: "HTML",
	})
	b.bot.refreshTyping()
	return err
}

func (b *telegramTrackerBackend) EditWithButton(msgID, text, btnLabel, btnData string) error {
	id, _ := strconv.ParseInt(msgID, 10, 64)
	rows := buildButtonRows([]platform.ButtonChoice{{Label: btnLabel, Data: btnData}}, "")
	_, _, err := b.bot.client.EditMessageText(text, &gotgbot.EditMessageTextOpts{
		ChatId:    b.chatID,
		MessageId: id,
		ParseMode: "HTML",
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{
			InlineKeyboard: rows,
		},
	})
	b.bot.refreshTyping()
	return err
}

func (b *telegramTrackerBackend) Delete(msgID string) error {
	id, _ := strconv.ParseInt(msgID, 10, 64)
	_, _ = b.bot.client.DeleteMessage(b.chatID, id, nil)
	return nil
}

func (b *telegramTrackerBackend) Logger() *log.ComponentLogger {
	return b.bot.logger()
}

// formatToolCallWithResult combines a tool call message with its result,
// truncating the result so the total message fits within Telegram's 4096 char limit.
func formatToolCallWithResult(toolText, result string) string {
	const maxLen = 4096
	separator := "\n\n📋 <b>Result:</b>\n<pre>"
	suffix := "</pre>"

	overhead := len(toolText) + len(separator) + len(suffix)
	if overhead >= maxLen {
		return toolText
	}

	escapedResult := htmlEscape(result)
	available := maxLen - overhead
	if len(escapedResult) > available {
		escapedResult = escapedResult[:available-3] + "..."
	}
	return toolText + separator + escapedResult + suffix
}
