package provider

import (
	"context"
	"sync"
	"time"
)

// StreamIdleWatchdog bounds the gap between streamed chunks without capping the
// total stream duration. It derives a cancellable context from a parent and
// cancels it if Reset is not called within the idle timeout. Callers Reset on
// every received chunk; a genuinely stalled stream is then aborted after one
// idle window of silence, while a long-but-progressing stream runs to
// completion.
//
// This replaces an http.Client.Timeout wall-clock cap, which truncated long
// streaming responses mid-stream as non-retryable (P2-6).
type StreamIdleWatchdog struct {
	idle   time.Duration
	cancel context.CancelFunc

	mu    sync.Mutex
	timer *time.Timer
	fired bool
}

// NewStreamIdleWatchdog returns a context derived from parent and a watchdog
// that cancels it after idle of inactivity. An idle <= 0 disables the timeout:
// the returned watchdog still cancels on Stop, but never fires on its own.
func NewStreamIdleWatchdog(parent context.Context, idle time.Duration) (context.Context, *StreamIdleWatchdog) {
	ctx, cancel := context.WithCancel(parent)
	w := &StreamIdleWatchdog{idle: idle, cancel: cancel}
	if idle > 0 {
		w.mu.Lock()
		w.timer = time.AfterFunc(idle, w.onTimeout)
		w.mu.Unlock()
	}
	return ctx, w
}

func (w *StreamIdleWatchdog) onTimeout() {
	w.mu.Lock()
	w.fired = true
	w.mu.Unlock()
	w.cancel()
}

// Reset restarts the idle countdown. Call it each time a chunk is received.
// Once the watchdog has fired it stays fired (the context is already cancelled).
func (w *StreamIdleWatchdog) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fired || w.timer == nil {
		return
	}
	w.timer.Reset(w.idle)
}

// Stop releases the watchdog and cancels the derived context. Idempotent; call
// via defer once the stream loop has finished.
func (w *StreamIdleWatchdog) Stop() {
	w.mu.Lock()
	if w.timer != nil {
		w.timer.Stop()
	}
	w.mu.Unlock()
	w.cancel()
}

// Fired reports whether the context was cancelled by the idle timeout (rather
// than by the parent or by Stop), so callers can return a clear timeout error.
func (w *StreamIdleWatchdog) Fired() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}
