package telegram

import (
	"strconv"
	"strings"
	"sync"

	"foci/internal/display"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/turn"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// telegramMaxChars is the maximum message length Telegram allows.
const telegramMaxChars = 4096

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
	interval := bot.streamInterval()
	newSB := func() *turn.StreamBuffer {
		return turn.NewStreamBuffer(backend.OpenStream(), interval, d.StreamOutput)
	}
	return turn.NewTurnRenderer(backend, tracker, d, newSB)
}

// Compile-time checks.
var (
	_ turn.Platform    = (*telegramBackend)(nil)
	_ turn.ChunkWriter = (*telegramBackend)(nil)
)

// telegramBackend implements turn.Platform and turn.ChunkWriter for Telegram. It
// owns all layout: HTML formatting, chopping at Telegram's 4096-char limit,
// message identity, streaming rollover, and thinking-button placement. The
// shared delivery loop (turn.DeliverChunks) drives the ChunkWriter primitives.
type telegramBackend struct {
	bot    *Bot
	msg    *gotgbot.Message
	chatID int64
	opts   display.RenderOpts
	width  int
}

// OpenStream begins a live streaming surface.
func (b *telegramBackend) OpenStream() turn.StreamSink {
	return &telegramStreamSink{
		client: b.bot.client,
		bot:    b.bot,
		chatID: b.chatID,
		opts:   b.opts,
	}
}

// Deliver performs the terminal delivery via the shared chunk-delivery loop.
func (b *telegramBackend) Deliver(p turn.Payload, stream turn.StreamSink) (turn.DeliveryResult, error) {
	return turn.DeliverChunks(b, p, stream)
}

// EditInPlace replaces a single existing message (a tool-call preview) in place
// via the shared single-message edit. Returns turn.ErrTooLongForEdit if the
// composed body would need to split across more than one message.
func (b *telegramBackend) EditInPlace(msgID string, p turn.Payload) error {
	return turn.EditChunksInPlace(b, msgID, p)
}

// Split chops an HTML body at Telegram's char limit, preserving open tags.
func (b *telegramBackend) Split(body string) []string {
	return splitMessage(body, telegramMaxChars)
}

// DeleteMsg deletes a leftover live-stream message, best-effort.
func (b *telegramBackend) DeleteMsg(msgID string) {
	id := parseTelegramMsgID(msgID)
	if _, err := b.bot.client.DeleteMessage(b.chatID, id, nil); err != nil {
		b.bot.logger().Debugf("deliver delete orphan %s: %v", msgID, err)
	}
}

func (b *telegramBackend) SendTyping() {
	b.bot.SetTyping(true)
}

func (b *telegramBackend) Logger() *log.ComponentLogger {
	return b.bot.logger()
}

// composeBody builds the message body for the payload per thinking mode and
// reports whether the last chunk should carry a "Show thinking" button (compact
// mode) and the raw thinking text to store with it.
func (b *telegramBackend) ComposeBody(p turn.Payload) (body string, hasButton bool, thinkingText string) {
	response := ConvertToTelegramHTML(p.Text, b.opts)
	switch p.ThinkingMode {
	case "full":
		return buildThinkingHTML(response, p.ThinkingText, b.width), false, ""
	case "compact":
		return response, true, p.ThinkingText
	default:
		return response, false, ""
	}
}

// SendChunk sends a single (already-chunked) HTML body as one message, falling
// back to plain text on HTML error; ok=false on a (logged) send failure.
func (b *telegramBackend) SendChunk(html string) (string, bool) {
	msg, err := b.bot.client.SendMessage(b.chatID, html, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
	if err != nil {
		msg, err = b.bot.client.SendMessage(b.chatID, html, nil)
		if err != nil {
			b.bot.logger().Errorf("send error: %s", b.bot.sanitizeError(err))
			return "", false
		}
	}
	b.bot.refreshTyping()
	return formatTelegramMsgID(msg.MessageId), true
}

// SendChunkWithButton sends a chunk with a "Show thinking" button and stores the
// thinking entry keyed on the sent message ID.
func (b *telegramBackend) SendChunkWithButton(html, thinkingText string) (string, error) {
	thinkingRows := buildButtonRows([]platform.ButtonChoice{{Label: "Show thinking", Data: "show"}}, "th:")
	sent, err := b.bot.client.SendMessage(b.chatID, html, &gotgbot.SendMessageOpts{
		ParseMode: "HTML",
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{
			InlineKeyboard: thinkingRows,
		},
	})
	if err != nil {
		return "", err
	}
	b.bot.thinkingStore.Store(sent.MessageId, thinkingEntry{
		responseHTML: html,
		thinkingText: thinkingText,
	})
	b.bot.refreshTyping()
	return formatTelegramMsgID(sent.MessageId), nil
}

// EditChunk edits an existing message with the given HTML body.
func (b *telegramBackend) EditChunk(msgID, html string) error {
	id := parseTelegramMsgID(msgID)
	_, _, err := b.bot.client.EditMessageText(html, &gotgbot.EditMessageTextOpts{
		ChatId:    b.chatID,
		MessageId: id,
		ParseMode: "HTML",
	})
	return err
}

// EditChunkWithButton edits an existing message with a "Show thinking" button
// and stores the thinking entry keyed on that message ID.
func (b *telegramBackend) EditChunkWithButton(msgID, html, thinkingText string) error {
	id := parseTelegramMsgID(msgID)
	thinkingRows := buildButtonRows([]platform.ButtonChoice{{Label: "Show thinking", Data: "show"}}, "th:")
	_, _, err := b.bot.client.EditMessageText(html, &gotgbot.EditMessageTextOpts{
		ChatId:    b.chatID,
		MessageId: id,
		ParseMode: "HTML",
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{
			InlineKeyboard: thinkingRows,
		},
	})
	if err != nil {
		return err
	}
	b.bot.thinkingStore.Store(id, thinkingEntry{
		responseHTML: html,
		thinkingText: thinkingText,
	})
	return nil
}

// telegramStreamSink is the live streaming surface returned by OpenStream. It
// owns the live message sequence and rolls over to new messages when the
// formatted text exceeds a single 4096-char message.
type telegramStreamSink struct {
	client botClient
	bot    *Bot
	chatID int64
	opts   display.RenderOpts

	mu       sync.Mutex
	closed   bool
	msgIDs   []int64
	lastSent []string // last formatted content sent per chunk index
}

// Update formats the full accumulated text to Telegram HTML, chops it into
// 4096-char chunks, and edits existing messages (skipping unchanged chunks) or
// sends new messages (rollover) for chunks beyond the live sequence.
func (s *telegramStreamSink) Update(fullText string) {
	formatted := ConvertToTelegramHTML(closePartialMarkdown(fullText), s.opts)
	chunks := splitMessage(formatted, telegramMaxChars)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}

	for i, chunk := range chunks {
		if i < len(s.msgIDs) {
			// Skip unchanged chunks to avoid "message is not modified" churn.
			if i < len(s.lastSent) && s.lastSent[i] == chunk {
				continue
			}
			s.editStream(s.msgIDs[i], chunk)
			s.setLastSent(i, chunk)
			continue
		}
		// Rollover: send a new message for this chunk.
		id, ok := s.sendStream(chunk)
		if !ok {
			break
		}
		s.msgIDs = append(s.msgIDs, id)
		s.setLastSent(i, chunk)
	}
}

// setLastSent records the last content sent at chunk index i (extending the
// slice as needed). Caller holds the lock.
func (s *telegramStreamSink) setLastSent(i int, content string) {
	for len(s.lastSent) <= i {
		s.lastSent = append(s.lastSent, "")
	}
	s.lastSent[i] = content
}

// sendStream sends one HTML chunk as a new message, falling back to plain text.
func (s *telegramStreamSink) sendStream(html string) (int64, bool) {
	msg, err := s.client.SendMessage(s.chatID, html, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
	s.bot.refreshTyping()
	if err != nil {
		msg, err = s.client.SendMessage(s.chatID, html, nil)
		if err != nil {
			return 0, false
		}
	}
	return msg.MessageId, true
}

// editStream edits a stream message with HTML, falling back to plain text.
func (s *telegramStreamSink) editStream(id int64, html string) {
	_, _, err := s.client.EditMessageText(html, &gotgbot.EditMessageTextOpts{
		ChatId:    s.chatID,
		MessageId: id,
		ParseMode: "HTML",
	})
	if err != nil {
		// Fallback: edit as plain text. Ignore "message is not modified".
		_, _, _ = s.client.EditMessageText(html, &gotgbot.EditMessageTextOpts{
			ChatId:    s.chatID,
			MessageId: id,
		})
	}
	s.bot.refreshTyping()
}

// Close stops accepting updates and reports whether any message surfaced.
func (s *telegramStreamSink) Close() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return len(s.msgIDs) > 0
}

// MsgIDs returns the live sequence IDs as strings, in order.
func (s *telegramStreamSink) MsgIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.msgIDs))
	for i, id := range s.msgIDs {
		out[i] = formatTelegramMsgID(id)
	}
	return out
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
