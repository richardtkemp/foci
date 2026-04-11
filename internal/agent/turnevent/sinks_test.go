package turnevent

import (
	"context"
	"errors"
	"testing"

	"foci/internal/provider"
)

// TestBufferSinkIgnoresIntermediateEvents asserts that BufferSink captures only
// TurnComplete and leaves intermediate events untouched — HTTP handlers and
// other "just give me the final answer" callers rely on this.
func TestBufferSinkIgnoresIntermediateEvents(t *testing.T) {
	ctx := context.Background()
	b := NewBufferSink()

	b.Emit(ctx, TurnStart{})
	b.Emit(ctx, TextBlock{Text: "intermediate", Phase: PhaseIntermediate})
	b.Emit(ctx, TextDelta{Delta: "ignored"})
	b.Emit(ctx, ToolCall{Name: "noop"})

	if b.FinalText() != "" {
		t.Errorf("FinalText before TurnComplete = %q, want empty", b.FinalText())
	}
	if b.Done() {
		t.Errorf("Done() before TurnComplete = true, want false")
	}

	usage := &provider.Usage{InputTokens: 42}
	b.Emit(ctx, TurnComplete{FinalText: "final", Usage: usage, Cost: 1.23, Model: "m"})

	if got, want := b.FinalText(), "final"; got != want {
		t.Errorf("FinalText = %q, want %q", got, want)
	}
	if b.Usage() != usage {
		t.Errorf("Usage pointer not captured")
	}
	if got, want := b.Cost(), 1.23; got != want {
		t.Errorf("Cost = %v, want %v", got, want)
	}
	if got, want := b.Model(), "m"; got != want {
		t.Errorf("Model = %q, want %q", got, want)
	}
	if !b.Done() {
		t.Errorf("Done() after TurnComplete = false, want true")
	}
}

// TestBufferSinkCapturesError asserts that TurnComplete.Err (error-path turns)
// is preserved on the sink so callers can distinguish "turn errored" from
// "turn completed with empty text".
func TestBufferSinkCapturesError(t *testing.T) {
	b := NewBufferSink()
	want := errors.New("boom")
	b.Emit(context.Background(), TurnComplete{Err: want})
	if b.Err() != want {
		t.Errorf("Err = %v, want %v", b.Err(), want)
	}
}

// TestRecordingSinkOrdersEvents asserts the sink preserves insertion order —
// tests that assert on event sequences depend on this.
func TestRecordingSinkOrdersEvents(t *testing.T) {
	ctx := context.Background()
	r := NewRecordingSink()
	r.Emit(ctx, TurnStart{})
	r.Emit(ctx, TextBlock{Text: "a"})
	r.Emit(ctx, TextDelta{Delta: "b"})
	r.Emit(ctx, TurnComplete{FinalText: "done"})

	evs := r.Events()
	if len(evs) != 4 {
		t.Fatalf("len(events) = %d, want 4", len(evs))
	}
	if _, ok := evs[0].(TurnStart); !ok {
		t.Errorf("events[0] = %T, want TurnStart", evs[0])
	}
	if tb, ok := evs[1].(TextBlock); !ok || tb.Text != "a" {
		t.Errorf("events[1] = %v, want TextBlock{a}", evs[1])
	}
	if td, ok := evs[2].(TextDelta); !ok || td.Delta != "b" {
		t.Errorf("events[2] = %v, want TextDelta{b}", evs[2])
	}
	if tc, ok := evs[3].(TurnComplete); !ok || tc.FinalText != "done" {
		t.Errorf("events[3] = %v, want TurnComplete{done}", evs[3])
	}
	if got := r.FinalText(); got != "done" {
		t.Errorf("FinalText = %q, want done", got)
	}
}

// TestRecordingSinkTextsConcatenates asserts Texts() assembles TextBlocks and
// the final TurnComplete text with newlines, so assertions can write
// "a\nb\ndone" instead of picking events apart.
func TestRecordingSinkTextsConcatenates(t *testing.T) {
	ctx := context.Background()
	r := NewRecordingSink()
	r.Emit(ctx, TextBlock{Text: "a"})
	r.Emit(ctx, TextBlock{Text: "b"})
	r.Emit(ctx, TurnComplete{FinalText: "c"})

	if got, want := r.Texts(), "a\nb\nc"; got != want {
		t.Errorf("Texts = %q, want %q", got, want)
	}
}

// TestTeeSinkFansOut asserts TeeSink broadcasts each event to every wrapped
// sink — the "I want both a buffer and a renderer" pattern used by
// agents_notify.
func TestTeeSinkFansOut(t *testing.T) {
	ctx := context.Background()
	a := NewRecordingSink()
	b := NewRecordingSink()
	tee := NewTeeSink(a, b)
	tee.Emit(ctx, TurnStart{})
	tee.Emit(ctx, TurnComplete{FinalText: "hi"})

	if len(a.Events()) != 2 {
		t.Errorf("sink a got %d events, want 2", len(a.Events()))
	}
	if len(b.Events()) != 2 {
		t.Errorf("sink b got %d events, want 2", len(b.Events()))
	}
	if a.FinalText() != "hi" || b.FinalText() != "hi" {
		t.Errorf("FinalText not propagated to both sinks")
	}
}

// TestTeeSinkSkipsNil asserts TeeSink tolerates nil entries — callers build
// tee arrays conditionally and we don't want a nil to panic mid-turn.
func TestTeeSinkSkipsNil(t *testing.T) {
	tee := NewTeeSink(nil, NewRecordingSink(), nil)
	// Must not panic.
	tee.Emit(context.Background(), TurnStart{})
}

// TestNopSinkDiscards asserts NopSink drops every event silently — the
// default fallback for ctx with no sink attached.
func TestNopSinkDiscards(t *testing.T) {
	var n NopSink
	n.Emit(context.Background(), TurnStart{})
	n.Emit(context.Background(), TurnComplete{FinalText: "ignored"})
	// If it compiles and doesn't panic, it works.
}

// TestSinkFromContextFallback asserts callers that emit into a ctx without a
// sink attached get a no-op rather than a nil-pointer panic.
func TestSinkFromContextFallback(t *testing.T) {
	ctx := context.Background()
	sink := SinkFromContext(ctx)
	if sink == nil {
		t.Fatal("SinkFromContext returned nil; want NopSink fallback")
	}
	// Must not panic.
	Emit(ctx, TurnComplete{FinalText: "dropped"})
}

// TestWithSinkStoresAndRetrieves asserts the context helper round-trips a
// sink correctly.
func TestWithSinkStoresAndRetrieves(t *testing.T) {
	r := NewRecordingSink()
	ctx := WithSink(context.Background(), r)
	Emit(ctx, TurnStart{})
	Emit(ctx, TurnComplete{FinalText: "x"})
	if r.FinalText() != "x" {
		t.Errorf("FinalText = %q, want x", r.FinalText())
	}
}

// TestWithSinkNilIsNoop asserts passing nil to WithSink is a no-op rather
// than storing a typed-nil that explodes on Emit.
func TestWithSinkNilIsNoop(t *testing.T) {
	ctx := WithSink(context.Background(), nil)
	// Should still fall back to NopSink, not typed-nil.
	Emit(ctx, TurnComplete{})
}

// TestSteererFuncAdapter asserts the function adapter forwards to the closure.
func TestSteererFuncAdapter(t *testing.T) {
	called := false
	var s Steerer = SteererFunc(func() []string {
		called = true
		return []string{"a", "b"}
	})
	got := s.PendingSteers()
	if !called {
		t.Error("SteererFunc did not invoke closure")
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("PendingSteers = %v, want [a b]", got)
	}
}

// TestWithSteererStoresAndRetrieves asserts the context helper round-trips.
func TestWithSteererStoresAndRetrieves(t *testing.T) {
	s := SteererFunc(func() []string { return []string{"hi"} })
	ctx := WithSteerer(context.Background(), s)
	got := SteererFromContext(ctx)
	if got == nil {
		t.Fatal("SteererFromContext returned nil after WithSteerer")
	}
	if p := got.PendingSteers(); len(p) != 1 || p[0] != "hi" {
		t.Errorf("PendingSteers = %v, want [hi]", p)
	}
}

// TestSteererFromContextAbsent asserts callers can distinguish "no steerer
// wired" from "steerer returned empty" via a nil check.
func TestSteererFromContextAbsent(t *testing.T) {
	if got := SteererFromContext(context.Background()); got != nil {
		t.Errorf("SteererFromContext(empty ctx) = %v, want nil", got)
	}
}
