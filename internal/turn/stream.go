package turn

import (
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// StreamTransport sends and edits streaming messages on a specific platform.
// Implementations handle platform-specific formatting (e.g. HTML for Telegram).
type StreamTransport interface {
	// SendInitial sends the first streaming message with raw buffer text.
	// Returns a string message ID for subsequent edits.
	SendInitial(text string) (msgID string, err error)
	// EditStream edits the streaming message with updated raw buffer text.
	EditStream(msgID string, text string) error
}

// StreamWriter accumulates text deltas and periodically edits a message
// to show model output in real-time. The goroutine and initial message are
// lazily started on the first delta (when live is true). When live is false,
// the writer accumulates text into its buffer without sending any messages,
// allowing a uniform interface regardless of streaming mode.
type StreamWriter struct {
	transport StreamTransport
	interval  time.Duration
	maxChars  int  // truncation limit for streaming
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

// NewStreamWriter creates a stream writer. When live is true, the first OnDelta
// call sends a message and starts the periodic edit goroutine. When live is
// false, deltas are accumulated in the buffer but no messages are sent.
func NewStreamWriter(transport StreamTransport, interval time.Duration, maxChars int, live bool) *StreamWriter {
	return &StreamWriter{
		transport: transport,
		interval:  interval,
		maxChars:  maxChars,
		live:      live,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// OnDelta appends a text delta to the buffer. On the first call (live mode),
// sends the initial message and starts the periodic edit goroutine.
func (sw *StreamWriter) OnDelta(delta string) {
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
func (sw *StreamWriter) sendInitial() {
	raw := sw.truncated()
	msgID, err := sw.transport.SendInitial(raw)
	if err != nil {
		return
	}
	sw.msgID = msgID
	sw.dirty = false
}

// editLoop runs on a ticker goroutine, editing the message with the latest buffer contents.
func (sw *StreamWriter) editLoop() {
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
		_ = sw.transport.EditStream(msgID, raw)
	}
}

// truncated returns the buffer contents, truncated to maxChars with "..." appended.
// Truncation is rune-safe to avoid splitting multi-byte UTF-8 characters.
// Called under lock.
func (sw *StreamWriter) truncated() string {
	s := sw.buf.String()
	if len(s) > sw.maxChars {
		// Find the last valid rune boundary at or before maxChars.
		end := sw.maxChars
		for end > 0 && !utf8.RuneStart(s[end]) {
			end--
		}
		return s[:end] + "..."
	}
	return s
}

// Finish stops the edit loop and returns the message ID ("" if no deltas arrived).
// Does NOT do a final flush — the caller handles the final edit with proper formatting.
// Idempotent: safe to call multiple times (e.g. Finalize then deferred Cleanup).
func (sw *StreamWriter) Finish() string {
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
func (sw *StreamWriter) Content() string {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.buf.String()
}
