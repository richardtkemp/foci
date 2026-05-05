package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"foci/internal/agent/turnevent"
)

// recordingRouterSink is a turnevent.Sink that counts calls — used by
// the sessionRouter dispatch tests. Distinct from any platform sink
// type; this is purely for routing assertions.
type recordingRouterSink struct {
	id    string
	mu    sync.Mutex
	count atomic.Int32
}

func (s *recordingRouterSink) Emit(_ context.Context, _ turnevent.Event) {
	s.count.Add(1)
}

func TestSessionRouter_FallbackWhenNoSinkRegistered(t *testing.T) {
	t.Parallel()
	fallback := &recordingRouterSink{id: "fallback"}
	r := newSessionRouter(fallback)

	r.Emit(context.Background(), turnevent.TextBlock{Text: "hello", Phase: turnevent.PhaseIntermediate})

	if got := fallback.count.Load(); got != 1 {
		t.Errorf("fallback events = %d, want 1", got)
	}
}

func TestSessionRouter_RegisteredSinkReceivesEvents(t *testing.T) {
	t.Parallel()
	fallback := &recordingRouterSink{id: "fallback"}
	turnSink := &recordingRouterSink{id: "turn"}
	r := newSessionRouter(fallback)

	r.Register(turnSink)
	r.Emit(context.Background(), turnevent.TextBlock{Text: "during turn"})

	if got := turnSink.count.Load(); got != 1 {
		t.Errorf("turn sink events = %d, want 1", got)
	}
	if got := fallback.count.Load(); got != 0 {
		t.Errorf("fallback events = %d, want 0", got)
	}
}

func TestSessionRouter_ClearRevertsToFallback(t *testing.T) {
	t.Parallel()
	fallback := &recordingRouterSink{id: "fallback"}
	turnSink := &recordingRouterSink{id: "turn"}
	r := newSessionRouter(fallback)

	r.Register(turnSink)
	r.Emit(context.Background(), turnevent.TextBlock{Text: "in-turn"})
	r.Clear()
	r.Emit(context.Background(), turnevent.TextBlock{Text: "post-turn"})

	if got := turnSink.count.Load(); got != 1 {
		t.Errorf("turn sink events = %d, want 1", got)
	}
	if got := fallback.count.Load(); got != 1 {
		t.Errorf("fallback events = %d, want 1", got)
	}
}

func TestSessionRouter_ReRegisterReplaces(t *testing.T) {
	t.Parallel()
	fallback := &recordingRouterSink{id: "fallback"}
	first := &recordingRouterSink{id: "first"}
	second := &recordingRouterSink{id: "second"}
	r := newSessionRouter(fallback)

	r.Register(first)
	r.Register(second)
	r.Emit(context.Background(), turnevent.TextBlock{Text: "to second"})

	if got := first.count.Load(); got != 0 {
		t.Errorf("first sink events = %d, want 0", got)
	}
	if got := second.count.Load(); got != 1 {
		t.Errorf("second sink events = %d, want 1", got)
	}
}

func TestSessionRouter_RegisterNilEquivalentToClear(t *testing.T) {
	t.Parallel()
	fallback := &recordingRouterSink{id: "fallback"}
	turnSink := &recordingRouterSink{id: "turn"}
	r := newSessionRouter(fallback)

	r.Register(turnSink)
	r.Register(nil)
	r.Emit(context.Background(), turnevent.TextBlock{Text: "post-nil"})

	if got := turnSink.count.Load(); got != 0 {
		t.Errorf("turn sink events = %d, want 0", got)
	}
	if got := fallback.count.Load(); got != 1 {
		t.Errorf("fallback events = %d, want 1", got)
	}
}

func TestSessionRouter_NilFallbackUsesNopSink(t *testing.T) {
	t.Parallel()
	r := newSessionRouter(nil)
	// Should not panic.
	r.Emit(context.Background(), turnevent.TextBlock{Text: "into the void"})
	r.Emit(context.Background(), turnevent.TurnComplete{FinalText: "void"})
}

func TestSessionRouter_ConcurrentRegisterEmitClear(t *testing.T) {
	// Stresses the atomic.Pointer dispatch under concurrent Register /
	// Clear / Emit. Every Emit must reach exactly one sink — current if
	// set, else fallback — without races or double-delivery.
	t.Parallel()
	fallback := &recordingRouterSink{id: "fallback"}
	turnSink := &recordingRouterSink{id: "turn"}
	r := newSessionRouter(fallback)

	const N = 200
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			if i%2 == 0 {
				r.Register(turnSink)
			} else {
				r.Clear()
			}
		}
	}()

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < N; j++ {
				r.Emit(context.Background(), turnevent.TextBlock{Text: "concurrent"})
			}
		}()
	}

	wg.Wait()

	total := turnSink.count.Load() + fallback.count.Load()
	const expected = 4 * N
	if total != expected {
		t.Errorf("total events delivered = %d, want %d", total, expected)
	}
}

func TestSessionRouter_ImplementsSinkInterface(t *testing.T) {
	t.Parallel()
	var _ turnevent.Sink = (*sessionRouter)(nil)
}
