package turnevent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// recordingSink captures everything Emit'd into it, for routing assertions.
// Goroutine-safe via atomic counters and a mutex on the slice.
type recordingSink struct {
	id    string
	mu    sync.Mutex
	count atomic.Int32
	events []Event
}

func (s *recordingSink) Emit(_ context.Context, ev Event) {
	s.count.Add(1)
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
}

func TestSessionRouter_FallbackWhenNoSinkRegistered(t *testing.T) {
	t.Parallel()
	fallback := &recordingSink{id: "fallback"}
	r := NewSessionRouter(fallback)

	r.Emit(context.Background(), TextBlock{Text: "hello", Phase: PhaseIntermediate})

	if got := fallback.count.Load(); got != 1 {
		t.Errorf("fallback events = %d, want 1", got)
	}
}

func TestSessionRouter_RegisteredSinkReceivesEvents(t *testing.T) {
	t.Parallel()
	fallback := &recordingSink{id: "fallback"}
	turnSink := &recordingSink{id: "turn"}
	r := NewSessionRouter(fallback)

	r.Register(turnSink)
	r.Emit(context.Background(), TextBlock{Text: "during turn"})

	if got := turnSink.count.Load(); got != 1 {
		t.Errorf("turn sink events = %d, want 1", got)
	}
	if got := fallback.count.Load(); got != 0 {
		t.Errorf("fallback events = %d, want 0 (turn sink should have absorbed it)", got)
	}
}

func TestSessionRouter_ClearRevertsToFallback(t *testing.T) {
	t.Parallel()
	fallback := &recordingSink{id: "fallback"}
	turnSink := &recordingSink{id: "turn"}
	r := NewSessionRouter(fallback)

	r.Register(turnSink)
	r.Emit(context.Background(), TextBlock{Text: "in-turn"})
	r.Clear()
	r.Emit(context.Background(), TextBlock{Text: "post-turn"})

	if got := turnSink.count.Load(); got != 1 {
		t.Errorf("turn sink events = %d, want 1 (only the in-turn event)", got)
	}
	if got := fallback.count.Load(); got != 1 {
		t.Errorf("fallback events = %d, want 1 (the post-turn event)", got)
	}
}

func TestSessionRouter_ReRegisterReplaces(t *testing.T) {
	t.Parallel()
	fallback := &recordingSink{id: "fallback"}
	first := &recordingSink{id: "first"}
	second := &recordingSink{id: "second"}
	r := NewSessionRouter(fallback)

	r.Register(first)
	r.Register(second)
	r.Emit(context.Background(), TextBlock{Text: "to second"})

	if got := first.count.Load(); got != 0 {
		t.Errorf("first sink events = %d, want 0 (replaced before any emit)", got)
	}
	if got := second.count.Load(); got != 1 {
		t.Errorf("second sink events = %d, want 1", got)
	}
}

func TestSessionRouter_RegisterNilEquivalentToClear(t *testing.T) {
	t.Parallel()
	fallback := &recordingSink{id: "fallback"}
	turnSink := &recordingSink{id: "turn"}
	r := NewSessionRouter(fallback)

	r.Register(turnSink)
	r.Register(nil) // should clear
	r.Emit(context.Background(), TextBlock{Text: "post-nil"})

	if got := turnSink.count.Load(); got != 0 {
		t.Errorf("turn sink events = %d, want 0", got)
	}
	if got := fallback.count.Load(); got != 1 {
		t.Errorf("fallback events = %d, want 1", got)
	}
}

func TestSessionRouter_NilFallbackUsesNopSink(t *testing.T) {
	t.Parallel()
	r := NewSessionRouter(nil) // explicit nil fallback

	// Should not panic.
	r.Emit(context.Background(), TextBlock{Text: "into the void"})
	r.Emit(context.Background(), TurnComplete{FinalText: "void"})
}

func TestSessionRouter_ConcurrentRegisterEmitClear(t *testing.T) {
	// Stresses the atomic.Pointer dispatch under concurrent Register / Clear /
	// Emit. The contract is: every Emit reaches exactly one sink (current if
	// set, else fallback) without panicking, racing, or double-delivering.
	t.Parallel()
	fallback := &recordingSink{id: "fallback"}
	turnSink := &recordingSink{id: "turn"}
	r := NewSessionRouter(fallback)

	const N = 200
	var wg sync.WaitGroup

	// Registrar / clearer goroutine — flips the slot back and forth.
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

	// Emitter goroutines — race with the registrar.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < N; j++ {
				r.Emit(context.Background(), TextBlock{Text: "concurrent"})
			}
		}()
	}

	wg.Wait()

	total := turnSink.count.Load() + fallback.count.Load()
	const expected = 4 * N
	if total != expected {
		t.Errorf("total events delivered = %d, want %d (every Emit must reach exactly one sink)", total, expected)
	}
}

func TestSessionRouter_ImplementsSinkInterface(t *testing.T) {
	// Compile-time check that SessionRouter satisfies the Sink interface.
	t.Parallel()
	var _ Sink = (*SessionRouter)(nil)
}
