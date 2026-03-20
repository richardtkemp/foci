package telegram

import (
	"encoding/json"
	"fmt"
	"strconv"

	"foci/internal/log"
	"foci/internal/turn"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// newToolCallTracker creates a shared turn.ToolCallTracker backed by
// Telegram-specific formatting and messaging.
func newToolCallTracker(bot *Bot, chatID int64, d turn.TurnDisplay) *turn.ToolCallTracker {
	backend := &telegramTrackerBackend{bot: bot, chatID: chatID}
	store := &telegramTrackerStore{bot: bot, chatID: chatID}
	display := turn.TrackerDisplay{ShowToolCalls: d.ShowToolCalls}
	return turn.NewToolCallTracker(backend, store, display, compactResultHint)
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
	return strconv.FormatInt(sent.MessageId, 10), nil
}

func (b *telegramTrackerBackend) SendWithButton(text, btnLabel, btnData string) (string, error) {
	kb := singleButtonKeyboard(btnLabel, btnData)
	sent, err := b.bot.client.SendMessage(b.chatID, text, &gotgbot.SendMessageOpts{
		ParseMode:   "HTML",
		ReplyMarkup: kb,
	})
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(sent.MessageId, 10), nil
}

func (b *telegramTrackerBackend) Edit(msgID, text string) error {
	id, _ := strconv.ParseInt(msgID, 10, 64)
	_, _, err := b.bot.client.EditMessageText(text, &gotgbot.EditMessageTextOpts{
		ChatId:    b.chatID,
		MessageId: id,
		ParseMode: "HTML",
	})
	return err
}

func (b *telegramTrackerBackend) EditWithButton(msgID, text, btnLabel, btnData string) error {
	id, _ := strconv.ParseInt(msgID, 10, 64)
	kb := singleButtonKeyboard(btnLabel, btnData)
	_, _, err := b.bot.client.EditMessageText(text, &gotgbot.EditMessageTextOpts{
		ChatId:      b.chatID,
		MessageId:   id,
		ParseMode:   "HTML",
		ReplyMarkup: kb,
	})
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

// telegramTrackerStore implements turn.TrackerStore backed by the Bot's
// sync.Map and optional ToolDetailStore.
type telegramTrackerStore struct {
	bot    *Bot
	chatID int64
}

func (s *telegramTrackerStore) StoreEntry(msgID, compact, full, result string, expanded bool) {
	id, _ := strconv.ParseInt(msgID, 10, 64)
	s.bot.toolResults.Store(id, toolResultEntry{
		compactText: compact,
		fullInput:   full,
		result:      result,
		expanded:    expanded,
		chatID:      s.chatID,
	})
}

func (s *telegramTrackerStore) IsExpanded(msgID string) bool {
	id, _ := strconv.ParseInt(msgID, 10, 64)
	if prev, ok := s.bot.toolResults.Load(id); ok {
		return prev.(toolResultEntry).expanded
	}
	return false
}

func (s *telegramTrackerStore) Persist(msgID, compact, full, result string) {
	if s.bot.toolDetailStore == nil {
		return
	}
	id, _ := strconv.ParseInt(msgID, 10, 64)
	s.bot.toolDetailStore.Store(id, compact, full, result)
}

// toolResultEntry stores the compact summary, full input text, and result
// for inline keyboard expansion in "full" mode.
type toolResultEntry struct {
	compactText string // compact one-line summary (collapsed state)
	fullInput   string // full formatted tool call HTML with JSON params
	result      string // the raw tool result text (empty while tool is running)
	expanded    bool   // true if user clicked "Show full" before result arrived
	chatID      int64  // chat where the message lives (for deferred edits)
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
