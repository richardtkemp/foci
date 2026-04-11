package turnevent

import (
	"context"
	"strings"
	"sync"

	"foci/internal/provider"
)

// NopSink discards every event. Used by callers that want to run a turn but
// don't care about any of its output (e.g. internal hook-driven turns that
// only care about side effects).
type NopSink struct{}

// Emit implements Sink.
func (NopSink) Emit(context.Context, Event) {}

var nopSinkSingleton Sink = NopSink{}

// SinkFunc adapts a plain function to the Sink interface. Useful for inline
// sinks in tests and call sites that would otherwise define a trivial type
// just to hold an Emit method.
type SinkFunc func(ctx context.Context, ev Event)

// Emit implements Sink.
func (f SinkFunc) Emit(ctx context.Context, ev Event) { f(ctx, ev) }

// BufferSink collects the final text and usage from a turn. It ignores
// intermediate events entirely, so callers that only need the completed
// answer (HTTP handlers, spawn tool, hooks that log results) can wire it up
// without worrying about ordering or streaming.
//
// BufferSink is not safe for concurrent use across turns; construct a new one
// per turn. Within a single turn the single-producer contract applies.
type BufferSink struct {
	// populated from TurnComplete
	finalText string
	usage     *provider.Usage
	cost      float64
	model     string
	err       error
	done      bool
}

// NewBufferSink constructs an empty BufferSink.
func NewBufferSink() *BufferSink { return &BufferSink{} }

// Emit implements Sink.
func (b *BufferSink) Emit(_ context.Context, ev Event) {
	if tc, ok := ev.(TurnComplete); ok {
		b.finalText = tc.FinalText
		b.usage = tc.Usage
		b.cost = tc.Cost
		b.model = tc.Model
		b.err = tc.Err
		b.done = true
	}
}

// FinalText returns the final text captured from TurnComplete. Empty if the
// turn did not complete or produced no text.
func (b *BufferSink) FinalText() string { return b.finalText }

// Usage returns the captured token usage, or nil.
func (b *BufferSink) Usage() *provider.Usage { return b.usage }

// Cost returns the captured cost, or 0.
func (b *BufferSink) Cost() float64 { return b.cost }

// Model returns the captured model identifier, or "".
func (b *BufferSink) Model() string { return b.model }

// Err returns the error carried by TurnComplete, or nil.
func (b *BufferSink) Err() error { return b.err }

// Done reports whether a TurnComplete event has been observed.
func (b *BufferSink) Done() bool { return b.done }

// RecordingSink records every event in order. It is primarily a test helper
// but is usable in any context that wants to inspect the full event sequence.
//
// Concurrent Emits are safe (guarded by a mutex) so tests can spawn
// goroutines without having to synchronise externally.
type RecordingSink struct {
	mu     sync.Mutex
	events []Event
}

// NewRecordingSink constructs an empty RecordingSink.
func NewRecordingSink() *RecordingSink { return &RecordingSink{} }

// Emit implements Sink.
func (r *RecordingSink) Emit(_ context.Context, ev Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

// Events returns a snapshot of the recorded events in order.
func (r *RecordingSink) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

// FinalText returns the FinalText from the last TurnComplete event, or "" if
// no TurnComplete has been recorded.
func (r *RecordingSink) FinalText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.events) - 1; i >= 0; i-- {
		if tc, ok := r.events[i].(TurnComplete); ok {
			return tc.FinalText
		}
	}
	return ""
}

// Texts returns the concatenation of every TextBlock Text and the final
// TurnComplete.FinalText (if any), joined by newlines. Intended for
// assertions that care about "what did the user see" without needing to
// reassemble deltas.
func (r *RecordingSink) Texts() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, ev := range r.events {
		switch e := ev.(type) {
		case TextBlock:
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(e.Text)
		case TurnComplete:
			if e.FinalText != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(e.FinalText)
			}
		}
	}
	return b.String()
}

// TeeSink fans every event out to each wrapped sink in order. Useful when a
// caller wants both buffered final text (BufferSink) and live delivery
// (StreamingSink / SessionSink), or for test scaffolding that attaches a
// RecordingSink alongside the real one.
type TeeSink struct {
	Sinks []Sink
}

// NewTeeSink constructs a TeeSink from the given sinks.
func NewTeeSink(sinks ...Sink) *TeeSink { return &TeeSink{Sinks: sinks} }

// Emit implements Sink by fanning out to each wrapped sink in order.
func (t *TeeSink) Emit(ctx context.Context, ev Event) {
	for _, s := range t.Sinks {
		if s != nil {
			s.Emit(ctx, ev)
		}
	}
}
