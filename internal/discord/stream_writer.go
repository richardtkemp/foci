package discord

import (
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// streamMaxChars limits streamed message text to stay within Discord's 2000 char limit.
const streamMaxChars = 1900

// streamWriter accumulates text deltas and periodically edits a Discord message
// to show model output in real-time. The goroutine and initial message are lazily
// started on the first delta (when live is true). When live is false, the writer
// accumulates text into its buffer without sending any Discord messages,
// allowing a uniform interface regardless of streaming mode.
type streamWriter struct {
	bot       *Bot
	channelID string
	interval  time.Duration
	live      bool // when false, buffer-only (no messages or goroutines)

	mu      sync.Mutex
	buf     strings.Builder
	msgID   string       // "" until first send
	dirty   bool         // buffer changed since last edit
	stopped bool         // true after Finish()
	ticker  *time.Ticker // nil until first delta
	stop    chan struct{} // signals editLoop to exit
	done    chan struct{} // closed when editLoop goroutine exits
}

// newStreamWriter creates a stream writer. When live is true, the first OnDelta
// call sends a Discord message and starts the periodic edit goroutine. When
// live is false, deltas are accumulated in the buffer but no messages are sent.
func newStreamWriter(bot *Bot, channelID string, interval time.Duration, live bool) *streamWriter {
	return &streamWriter{
		bot:       bot,
		channelID: channelID,
		interval:  interval,
		live:      live,
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

	// Lazy start: first delta triggers initial message + edit loop (live mode only).
	if sw.live && sw.ticker == nil {
		sw.sendInitial()
		sw.ticker = time.NewTicker(sw.interval)
		go sw.editLoop()
	}
}

// sendInitial sends the first message with accumulated text. Called under lock.
func (sw *streamWriter) sendInitial() {
	raw := sw.truncated()
	msg, err := sw.bot.session.ChannelMessageSend(sw.channelID, raw)
	if err != nil {
		return
	}
	sw.msgID = msg.ID
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

		if msgID == "" {
			continue
		}

		// Edit outside the lock to avoid holding during network I/O.
		_, _ = sw.bot.session.ChannelMessageEdit(sw.channelID, msgID, raw)
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

// Finish stops the edit loop and returns the message ID ("" if no deltas arrived).
// Does NOT do a final flush -- the caller handles the final edit with proper formatting.
// Idempotent: safe to call multiple times (e.g. Finalize then deferred Cleanup).
func (sw *streamWriter) Finish() string {
	sw.mu.Lock()
	alreadyStopped := sw.stopped
	sw.stopped = true
	ticker := sw.ticker
	msgID := sw.msgID
	sw.mu.Unlock()

	if alreadyStopped {
		return msgID
	}

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
