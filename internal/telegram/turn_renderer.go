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

// telegramBackend implements turn.Platform for Telegram. It owns all layout:
// HTML formatting, chopping at Telegram's 4096-char limit, message identity,
// streaming rollover, and thinking-button placement.
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

// Deliver performs the terminal delivery. It composes the message body per
// thinking mode, chops it into 4096-char chunks, and lays those chunks over the
// stream's existing message sequence (editing/appending/deleting as needed) or
// sends fresh messages when nothing surfaced.
func (b *telegramBackend) Deliver(p turn.Payload, stream turn.StreamSink) (turn.DeliveryResult, error) {
	body, hasButton, thinkingText := b.composeBody(p)
	chunks := splitMessage(body, telegramMaxChars)
	if len(chunks) == 0 {
		chunks = []string{""}
	}

	var ids []string
	if stream != nil {
		ids = stream.MsgIDs()
	}

	// Fresh send: nothing surfaced. Send each chunk as a new message; the last
	// chunk carries the thinking button in compact mode.
	if len(ids) == 0 {
		var used []string
		for i, chunk := range chunks {
			last := i == len(chunks)-1
			if last && hasButton {
				id, err := b.sendChunkWithButton(chunk, thinkingText)
				if err != nil {
					return turn.DeliveryResult{MsgIDs: used}, err
				}
				used = append(used, id)
				continue
			}
			used = append(used, b.sendHTMLChunkIDs(chunk)...)
		}
		return turn.DeliveryResult{MsgIDs: used}, nil
	}

	// Finalize-in-place: lay the chunks over the existing live sequence.
	var used []string
	for i, chunk := range chunks {
		last := i == len(chunks)-1
		if i < len(ids) {
			// Edit the existing message at this position.
			if last && hasButton {
				if err := b.editChunkWithButton(ids[i], chunk, thinkingText); err != nil {
					return turn.DeliveryResult{MsgIDs: used}, err
				}
			} else {
				if err := b.editHTML(ids[i], chunk); err != nil {
					b.bot.logger().Debugf("deliver edit: %v", err)
				}
			}
			used = append(used, ids[i])
			continue
		}
		// Append a new message beyond the live sequence.
		if last && hasButton {
			id, err := b.sendChunkWithButton(chunk, thinkingText)
			if err != nil {
				return turn.DeliveryResult{MsgIDs: used}, err
			}
			used = append(used, id)
		} else {
			used = append(used, b.sendHTMLChunkIDs(chunk)...)
		}
	}

	// Delete any leftover messages from the live sequence (final shorter than
	// the live stream). When the final needed more chunks than the stream had
	// messages there are no leftovers — min() keeps the slice in bounds.
	for _, orphan := range ids[min(len(chunks), len(ids)):] {
		id := parseTelegramMsgID(orphan)
		if _, err := b.bot.client.DeleteMessage(b.chatID, id, nil); err != nil {
			b.bot.logger().Debugf("deliver delete orphan %s: %v", orphan, err)
		}
	}

	return turn.DeliveryResult{MsgIDs: used}, nil
}

// EditInPlace replaces a single existing message (a tool-call preview) in
// place. Returns turn.ErrTooLongForEdit if the composed body would need to
// split across more than one message.
func (b *telegramBackend) EditInPlace(msgID string, p turn.Payload) error {
	body, hasButton, thinkingText := b.composeBody(p)
	chunks := splitMessage(body, telegramMaxChars)
	if len(chunks) > 1 {
		return turn.ErrTooLongForEdit
	}
	chunk := body
	if len(chunks) == 1 {
		chunk = chunks[0]
	}
	if hasButton {
		return b.editChunkWithButton(msgID, chunk, thinkingText)
	}
	return b.editHTML(msgID, chunk)
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
func (b *telegramBackend) composeBody(p turn.Payload) (body string, hasButton bool, thinkingText string) {
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

// sendHTMLChunkIDs sends a single (already-chunked) HTML body as one message,
// falling back to plain text on HTML error, and returns the sent message IDs.
func (b *telegramBackend) sendHTMLChunkIDs(html string) []string {
	msg, err := b.bot.client.SendMessage(b.chatID, html, &gotgbot.SendMessageOpts{ParseMode: "HTML"})
	if err != nil {
		msg, err = b.bot.client.SendMessage(b.chatID, html, nil)
		if err != nil {
			b.bot.logger().Errorf("send error: %s", b.bot.sanitizeError(err))
			return nil
		}
	}
	b.bot.refreshTyping()
	return []string{formatTelegramMsgID(msg.MessageId)}
}

// sendChunkWithButton sends a chunk with a "Show thinking" button and stores the
// thinking entry keyed on the sent message ID.
func (b *telegramBackend) sendChunkWithButton(html, thinkingText string) (string, error) {
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

// editHTML edits an existing message with the given HTML body.
func (b *telegramBackend) editHTML(msgID, html string) error {
	id := parseTelegramMsgID(msgID)
	_, _, err := b.bot.client.EditMessageText(html, &gotgbot.EditMessageTextOpts{
		ChatId:    b.chatID,
		MessageId: id,
		ParseMode: "HTML",
	})
	return err
}

// editChunkWithButton edits an existing message with a "Show thinking" button
// and stores the thinking entry keyed on that message ID.
func (b *telegramBackend) editChunkWithButton(msgID, html, thinkingText string) error {
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
