package telegram

import (
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"foci/internal/display"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// streamMaxChars limits streamed message text to stay within Telegram's 4096 char limit.
const streamMaxChars = 3900

// streamWriter accumulates text deltas and periodically edits a Telegram message
// to show model output in real-time. The goroutine and initial message are lazily
// started on the first delta, so no resources are wasted if streaming is disabled
// or the agent returns no text.
type streamWriter struct {
	client    botClient
	chatID    int64
	interval  time.Duration
	tableOpts display.RenderOpts

	mu      sync.Mutex
	buf     strings.Builder
	msgID   int64        // 0 until first send
	dirty   bool         // buffer changed since last edit
	stopped bool         // true after Finish()
	ticker  *time.Ticker // nil until first delta
	stop    chan struct{} // signals editLoop to exit
	done    chan struct{} // closed when editLoop goroutine exits
}

// newStreamWriter creates a stream writer. No goroutines are started until OnDelta is called.
func newStreamWriter(client botClient, chatID int64, interval time.Duration, tableOpts display.RenderOpts) *streamWriter {
	return &streamWriter{
		client:    client,
		chatID:    chatID,
		interval:  interval,
		tableOpts: tableOpts,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// OnDelta appends a text delta to the buffer. On the first call, sends the
// initial message and starts the periodic edit goroutine.
func (sw *streamWriter) OnDelta(delta string) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	if sw.stopped {
		return
	}

	sw.buf.WriteString(delta)
	sw.dirty = true

	// Lazy start: first delta triggers initial message + edit loop.
	if sw.ticker == nil {
		sw.sendInitial()
		sw.ticker = time.NewTicker(sw.interval)
		go sw.editLoop()
	}
}

// formatForStream converts raw buffer text to Telegram HTML for streaming.
// Closes partial markdown delimiters, then converts to HTML.
func (sw *streamWriter) formatForStream(raw string) string {
	closed := closePartialMarkdown(raw)
	return ConvertToTelegramHTML(closed, sw.tableOpts)
}

// sendInitial sends the first message with accumulated text. Called under lock.
func (sw *streamWriter) sendInitial() {
	raw := sw.truncated()
	html := sw.formatForStream(raw)
	msg, err := sw.client.SendMessage(sw.chatID, html, &gotgbot.SendMessageOpts{
		ParseMode: "HTML",
	})
	if err != nil {
		// Fallback: send as plain text (malformed HTML or API error).
		msg, err = sw.client.SendMessage(sw.chatID, raw, nil)
		if err != nil {
			return
		}
	}
	sw.msgID = msg.MessageId
	sw.dirty = false
}

// editLoop runs on a ticker goroutine, editing the message with the latest buffer contents.
func (sw *streamWriter) editLoop() {
	defer close(sw.done)

	for {
		select {
		case <-sw.stop:
			return
		case <-sw.ticker.C:
		}

		sw.mu.Lock()
		if !sw.dirty {
			sw.mu.Unlock()
			continue
		}
		raw := sw.truncated()
		msgID := sw.msgID
		sw.dirty = false
		sw.mu.Unlock()

		if msgID == 0 {
			continue
		}

		// Convert to HTML with partial-markdown handling.
		html := sw.formatForStream(raw)

		// Edit outside the lock to avoid holding during network I/O.
		_, _, err := sw.client.EditMessageText(html, &gotgbot.EditMessageTextOpts{
			ChatId:    sw.chatID,
			MessageId: msgID,
			ParseMode: "HTML",
		})
		if err != nil {
			// Fallback: edit as plain text. Ignore "message is not modified" errors.
			_, _, _ = sw.client.EditMessageText(raw, &gotgbot.EditMessageTextOpts{
				ChatId:    sw.chatID,
				MessageId: msgID,
			})
		}
	}
}

// truncated returns the buffer contents, truncated to streamMaxChars with "..." appended.
// Truncation is rune-safe to avoid splitting multi-byte UTF-8 characters.
// Called under lock.
func (sw *streamWriter) truncated() string {
	s := sw.buf.String()
	if len(s) > streamMaxChars {
		// Find the last valid rune boundary at or before streamMaxChars.
		end := streamMaxChars
		for end > 0 && !utf8.RuneStart(s[end]) {
			end--
		}
		return s[:end] + "..."
	}
	return s
}

// Finish stops the edit loop and returns the message ID (0 if no deltas arrived).
// Does NOT do a final flush — the caller handles the final edit with proper HTML formatting.
func (sw *streamWriter) Finish() int64 {
	sw.mu.Lock()
	sw.stopped = true
	ticker := sw.ticker
	msgID := sw.msgID
	sw.mu.Unlock()

	if ticker != nil {
		ticker.Stop()
		close(sw.stop) // signal editLoop to exit
		<-sw.done      // wait for editLoop to exit
	}

	return msgID
}

// Content returns the full accumulated buffer contents.
// Safe to call after Finish.
func (sw *streamWriter) Content() string {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.buf.String()
}
