package telegram

import (
	"strings"
	"sync"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// streamMaxChars limits streamed message text to stay within Telegram's 4096 char limit.
const streamMaxChars = 3900

// streamWriter accumulates text deltas and periodically edits a Telegram message
// to show model output in real-time. The goroutine and initial message are lazily
// started on the first delta, so no resources are wasted if streaming is disabled
// or the agent returns no text.
type streamWriter struct {
	client   botClient
	chatID   int64
	interval time.Duration

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
func newStreamWriter(client botClient, chatID int64, interval time.Duration) *streamWriter {
	return &streamWriter{
		client:   client,
		chatID:   chatID,
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
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

// sendInitial sends the first message with accumulated text. Called under lock.
func (sw *streamWriter) sendInitial() {
	text := sw.truncated()
	msg, err := sw.client.SendMessage(sw.chatID, text, nil) // plain text, no parse mode
	if err != nil {
		return
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
		text := sw.truncated()
		msgID := sw.msgID
		sw.dirty = false
		sw.mu.Unlock()

		if msgID == 0 {
			continue
		}

		// Edit outside the lock to avoid holding during network I/O.
		// Ignore "message is not modified" errors.
		_, _, _ = sw.client.EditMessageText(text, &gotgbot.EditMessageTextOpts{
			ChatId:    sw.chatID,
			MessageId: msgID,
		})
	}
}

// truncated returns the buffer contents, truncated to streamMaxChars with "..." appended.
// Called under lock.
func (sw *streamWriter) truncated() string {
	s := sw.buf.String()
	if len(s) > streamMaxChars {
		return s[:streamMaxChars] + "..."
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
