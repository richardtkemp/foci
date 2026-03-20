package telegram

import (
	"strconv"
	"strings"

	"foci/internal/display"
	"foci/internal/log"
	"foci/internal/turn"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// newTurnRenderer creates a turn.TurnRenderer backed by Telegram APIs.
// The tracker is created externally and passed in so the worker can wire
// its observeToolCall/observeToolResult directly to agent callbacks.
func newTurnRenderer(bot *Bot, msg *gotgbot.Message, tracker *turn.ToolCallTracker, d turn.TurnDisplay) *turn.TurnRenderer {
	backend := &telegramBackend{
		bot:    bot,
		msg:    msg,
		chatID: msg.Chat.Id,
		opts:   d.RenderOpts,
		width:  d.DisplayWidth,
	}
	transport := &telegramStreamTransport{
		client: bot.client,
		chatID: msg.Chat.Id,
		opts:   d.RenderOpts,
	}
	interval := bot.streamInterval()
	newSW := func() *turn.StreamWriter {
		return turn.NewStreamWriter(transport, interval, d.MaxChars-196, d.StreamOutput)
	}
	return turn.NewTurnRenderer(backend, tracker, d, newSW)
}

// telegramBackend implements turn.TurnBackend for Telegram.
type telegramBackend struct {
	bot    *Bot
	msg    *gotgbot.Message
	chatID int64
	opts   display.RenderOpts
	width  int
}

func (b *telegramBackend) FormatResponse(text string) string {
	return ConvertToTelegramHTML(text, b.opts)
}

func (b *telegramBackend) SendReply(text string) {
	b.bot.sendReply(b.msg, text)
}

func (b *telegramBackend) SendChunked(formatted string) {
	b.bot.sendHTMLChunks(b.chatID, formatted)
}

func (b *telegramBackend) EditMessage(msgID, formatted string) error {
	id := parseTelegramMsgID(msgID)
	_, _, err := b.bot.client.EditMessageText(formatted, &gotgbot.EditMessageTextOpts{
		ChatId:    b.chatID,
		MessageId: id,
		ParseMode: "HTML",
	})
	return err
}

func (b *telegramBackend) SendWithThinkingButton(formatted, thinkingText string) error {
	sendOpts := &gotgbot.SendMessageOpts{
		ParseMode:   "HTML",
		ReplyMarkup: singleButtonKeyboard("Show thinking", "th:show"),
	}
	chunks := splitMessage(formatted, 4096)
	for i, chunk := range chunks {
		if i < len(chunks)-1 {
			b.bot.sendHTMLChunks(b.chatID, chunk)
			continue
		}
		sent, err := b.bot.client.SendMessage(b.chatID, chunk, sendOpts)
		if err != nil {
			return err
		}
		b.bot.thinkingStore.Store(sent.MessageId, thinkingEntry{
			responseHTML: chunk,
			thinkingText: thinkingText,
		})
	}
	return nil
}

func (b *telegramBackend) EditWithThinkingButton(msgID, formatted, thinkingText string) error {
	id := parseTelegramMsgID(msgID)
	kb := singleButtonKeyboard("Show thinking", "th:show")
	_, _, err := b.bot.client.EditMessageText(formatted, &gotgbot.EditMessageTextOpts{
		ChatId:      b.chatID,
		MessageId:   id,
		ParseMode:   "HTML",
		ReplyMarkup: kb,
	})
	if err != nil {
		return err
	}
	b.bot.thinkingStore.Store(id, thinkingEntry{
		responseHTML: formatted,
		thinkingText: thinkingText,
	})
	return nil
}

func (b *telegramBackend) BuildThinkingCombined(responseFormatted, thinkingText string) string {
	return buildThinkingHTML(responseFormatted, thinkingText, b.width)
}

func (b *telegramBackend) FormatStreamPreview(preview string) string {
	return htmlEscape(preview) + "\n\n<i>(full response below)</i>"
}

func (b *telegramBackend) SendTyping() {
	_, _ = b.bot.client.SendChatAction(b.chatID, "typing", nil)
}

func (b *telegramBackend) Logger() *log.ComponentLogger {
	return b.bot.logger()
}

// telegramStreamTransport implements turn.StreamTransport for Telegram.
type telegramStreamTransport struct {
	client botClient
	chatID int64
	opts   display.RenderOpts
}

func (t *telegramStreamTransport) SendInitial(text string) (string, error) {
	html := t.formatForStream(text)
	msg, err := t.client.SendMessage(t.chatID, html, &gotgbot.SendMessageOpts{
		ParseMode: "HTML",
	})
	if err != nil {
		// Fallback: send as plain text (malformed HTML or API error).
		msg, err = t.client.SendMessage(t.chatID, text, nil)
		if err != nil {
			return "", err
		}
	}
	return formatTelegramMsgID(msg.MessageId), nil
}

func (t *telegramStreamTransport) EditStream(msgID, text string) error {
	id := parseTelegramMsgID(msgID)
	html := t.formatForStream(text)
	_, _, err := t.client.EditMessageText(html, &gotgbot.EditMessageTextOpts{
		ChatId:    t.chatID,
		MessageId: id,
		ParseMode: "HTML",
	})
	if err != nil {
		// Fallback: edit as plain text. Ignore "message is not modified" errors.
		_, _, _ = t.client.EditMessageText(text, &gotgbot.EditMessageTextOpts{
			ChatId:    t.chatID,
			MessageId: id,
		})
	}
	return nil
}

func (t *telegramStreamTransport) formatForStream(raw string) string {
	closed := closePartialMarkdown(raw)
	return ConvertToTelegramHTML(closed, t.opts)
}

// ID conversion helpers.

func formatTelegramMsgID(id int64) string {
	return strconv.FormatInt(id, 10)
}

func parseTelegramMsgID(s string) int64 {
	id, _ := strconv.ParseInt(s, 10, 64)
	return id
}

// buildThinkingHTML builds a combined thinking + divider + response HTML string.
func buildThinkingHTML(responseHTML, thinkingText string, displayWidth int) string {
	thinkingHTML := "<i>" + htmlEscape(thinkingText) + "</i>"
	divider := "\n" + strings.Repeat("—", displayWidth) + "\n\n"
	return thinkingHTML + divider + responseHTML
}
