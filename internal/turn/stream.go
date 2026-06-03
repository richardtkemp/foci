package turn

import (
	"strings"
	"sync"
	"time"

	"foci/internal/platform"
)

// StreamBuffer is a pure turn-side accumulator + silencing gate + pacing pump.
// It holds NO msgID, NO maxChars, NO formatting. It wraps a StreamSink and
// drives it: deltas accumulate into the buffer, and a ticker goroutine pushes
// the latest snapshot to the sink at a fixed interval. All layout (chopping,
// capping, rollover, message identity) lives in the StreamSink implementation
// on the platform side.
//
// When live is false, the buffer accumulates text but never calls the sink —
// the sink never surfaces and Finish returns (sink, false). This gives a
// uniform interface regardless of streaming mode.
type StreamBuffer struct {
	sink     StreamSink
	interval time.Duration
	live     bool

	mu       sync.Mutex
	buf      strings.Builder
	started  bool // pump started (first non-silencing delta released)
	stopped  bool
	surfaced bool // cached from sink.Close()
	dirty    bool
	stop     chan struct{}
	done     chan struct{}
}

// NewStreamBuffer creates a stream buffer wrapping the given sink. When live is
// true, the first non-silencing OnDelta releases the pump (fires an immediate
// Update and starts the ticker goroutine). When live is false, deltas are
// accumulated but the sink is never driven.
func NewStreamBuffer(sink StreamSink, interval time.Duration, live bool) *StreamBuffer {
	return &StreamBuffer{
		sink:     sink,
		interval: interval,
		live:     live,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// OnDelta appends a text delta to the buffer. On the first release (live mode),
// it fires one immediate sink.Update so the first message appears promptly,
// then starts the periodic pump goroutine.
//
// Streaming-prefix gate: the pump start is held while the accumulated buffer
// could still resolve to a silencing sentinel ([[NO_RESPONSE]] etc.). Once
// platform.IsSilencingPrefix returns false, the turn is known not to be
// silenced and the pump is released. If the stream ends while still in the
// prefix-ambiguous window, the sink is never driven and never surfaces — the
// StreamingSink's IsSilent check at TurnComplete is the authoritative final
// gate.
func (b *StreamBuffer) OnDelta(delta string) {
	b.mu.Lock()

	if b.stopped {
		b.mu.Unlock()
		return
	}

	b.buf.WriteString(delta)
	b.dirty = true

	if !b.live || b.started {
		b.mu.Unlock()
		return
	}

	// Silencing gate: hold the start while the buffer could still resolve to a
	// silencing sentinel.
	if platform.IsSilencingPrefix(b.buf.String()) {
		b.mu.Unlock()
		return
	}

	// Release: snapshot for the immediate Update, then start the pump.
	b.started = true
	b.dirty = false
	snapshot := b.buf.String()
	b.mu.Unlock()

	// Immediate first Update outside the lock (network I/O).
	b.sink.Update(snapshot)

	go b.pump()
}

// pump runs on a ticker goroutine, pushing the latest buffer snapshot to the
// sink whenever it is dirty.
func (b *StreamBuffer) pump() {
	defer close(b.done)

	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()

	for {
		select {
		case <-b.stop:
			return
		case <-ticker.C:
		}

		b.mu.Lock()
		if !b.dirty {
			b.mu.Unlock()
			continue
		}
		snapshot := b.buf.String()
		b.dirty = false
		b.mu.Unlock()

		// Update outside the lock to avoid holding during network I/O.
		b.sink.Update(snapshot)
	}
}

// Content returns the full accumulated buffer contents. Safe to call after
// Finish (used by the empty-FinalText fallback in the renderer).
func (b *StreamBuffer) Content() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Finish stops the pump, closes the sink, caches whether it surfaced, and
// returns the sink for the renderer to pass to Deliver. Idempotent: safe to
// call multiple times (e.g. Finalize/OnReply then deferred Cleanup) — the pump
// is stopped exactly once.
func (b *StreamBuffer) Finish() (StreamSink, bool) {
	b.mu.Lock()
	alreadyStopped := b.stopped
	b.stopped = true
	started := b.started
	b.mu.Unlock()

	if alreadyStopped {
		b.mu.Lock()
		surfaced := b.surfaced
		b.mu.Unlock()
		return b.sink, surfaced
	}

	// Stop the pump (if it was started) and wait for it to exit, so no Update
	// races with the Close below.
	if started {
		close(b.stop)
		<-b.done
	}

	surfaced := b.sink.Close()

	b.mu.Lock()
	b.surfaced = surfaced
	b.mu.Unlock()

	return b.sink, surfaced
}
