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

// TestNopSinkDiscards asserts NopSink drops every event silently — the
// default fallback for ctx with no sink attached.
func TestNopSinkDiscards(t *testing.T) {
	var n NopSink
	n.Emit(context.Background(), TurnStart{})
	n.Emit(context.Background(), TurnComplete{FinalText: "ignored"})
	// If it compiles and doesn't panic, it works.
}

// TestDeliversToPlatformByType pins the DeliversToPlatform answer per sink
// type. NopSink discards everything and BufferSink accumulates for an
// in-process caller, so both report false — the sink-delivery gate (TODO
// #767) relies on these answers to decide whether a fresh Telegram message
// can fold into an in-flight turn or must wait for a new turn with a
// delivering sink.
func TestDeliversToPlatformByType(t *testing.T) {
	if (NopSink{}).DeliversToPlatform() {
		t.Errorf("NopSink.DeliversToPlatform() = true, want false")
	}
	if NewBufferSink().DeliversToPlatform() {
		t.Errorf("BufferSink.DeliversToPlatform() = true, want false")
	}
	// The SinkFromContext fallback (no sink attached) must also report
	// false so callers that read it directly get the same gate signal as
	// when they unwrap the singleton.
	if SinkFromContext(context.Background()).DeliversToPlatform() {
		t.Errorf("SinkFromContext(empty).DeliversToPlatform() = true, want false")
	}
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
	b := NewBufferSink()
	ctx := WithSink(context.Background(), b)
	Emit(ctx, TurnStart{})
	Emit(ctx, TurnComplete{FinalText: "x"})
	if b.FinalText() != "x" {
		t.Errorf("FinalText = %q, want x", b.FinalText())
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
