package turnevent

import (
	"context"

	"foci/internal/provider"
)

// NopSink discards every event. Used by callers that want to run a turn but
// don't care about any of its output (e.g. internal hook-driven turns that
// only care about side effects).
type NopSink struct{}

// Emit implements Sink.
func (NopSink) Emit(context.Context, Event) {}

var nopSinkSingleton Sink = NopSink{}

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
