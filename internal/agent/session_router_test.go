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
	count atomic.Int32
}

func (s *recordingRouterSink) Emit(_ context.Context, _ turnevent.Event) {
	s.count.Add(1)
}

// DeliversToPlatform implements turnevent.Sink. Routing tests don't exercise
// the gate, but the interface needs satisfying — declare true so the router
// delegation tests below can assert sessionRouter.DeliversToPlatform()
// against a known answer.
func (s *recordingRouterSink) DeliversToPlatform() bool { return true }

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

// nonDeliveringSink is a turnevent.Sink that reports DeliversToPlatform=false
// — companion to recordingRouterSink for testing the router's delegation
// behaviour.
type nonDeliveringSink struct{}

func (nonDeliveringSink) Emit(_ context.Context, _ turnevent.Event) {}
func (nonDeliveringSink) DeliversToPlatform() bool                  { return false }

// TestSessionRouter_DeliversToPlatformDelegates verifies that the router
// forwards DeliversToPlatform to whichever sink Emit would currently route
// to: the registered per-turn sink when one is set, or the fallback. This
// is load-bearing for the sink-delivery gate (TODO #767), which asks the
// agent's session-scoped sink whether the in-flight turn's output reaches
// a user.
func TestSessionRouter_DeliversToPlatformDelegates(t *testing.T) {
	t.Parallel()

	// Delivering fallback, no registered per-turn sink → reports delivering.
	delivFallback := &recordingRouterSink{id: "fallback"}
	r := newSessionRouter(delivFallback)
	if !r.DeliversToPlatform() {
		t.Errorf("router with delivering fallback: DeliversToPlatform = false, want true")
	}

	// Register a non-delivering per-turn sink — router now reports false.
	r.Register(nonDeliveringSink{})
	if r.DeliversToPlatform() {
		t.Errorf("router with non-delivering registered sink: DeliversToPlatform = true, want false")
	}

	// Clear → falls back, delivering again.
	r.Clear()
	if !r.DeliversToPlatform() {
		t.Errorf("router after Clear: DeliversToPlatform = false, want true")
	}

	// Non-delivering fallback path.
	r2 := newSessionRouter(nonDeliveringSink{})
	if r2.DeliversToPlatform() {
		t.Errorf("router with non-delivering fallback: DeliversToPlatform = true, want false")
	}

	// Nil fallback installs the NopSink singleton (non-delivering).
	r3 := newSessionRouter(nil)
	if r3.DeliversToPlatform() {
		t.Errorf("router with nil (NopSink) fallback: DeliversToPlatform = true, want false")
	}
}

// #1257: late-stream slot streams autonomous output when no per-turn sink is
// registered, without ever colliding with a real turn's `current` sink.
func TestSessionRouter_LateStreamReceivesWhenNoTurnSink(t *testing.T) {
	t.Parallel()
	fallback := &recordingRouterSink{id: "fallback"}
	late := &recordingRouterSink{id: "late"}
	r := newSessionRouter(fallback)

	r.SetLateStream(late)
	r.Emit(context.Background(), turnevent.TextDelta{Delta: "streamed"})

	if got := late.count.Load(); got != 1 {
		t.Errorf("late-stream events = %d, want 1", got)
	}
	if got := fallback.count.Load(); got != 0 {
		t.Errorf("fallback events = %d, want 0 (late-stream should win over fallback)", got)
	}
}

func TestSessionRouter_CurrentSinkBeatsLateStream(t *testing.T) {
	t.Parallel()
	fallback := &recordingRouterSink{id: "fallback"}
	late := &recordingRouterSink{id: "late"}
	turnSink := &recordingRouterSink{id: "turn"}
	r := newSessionRouter(fallback)

	// A real turn is registered AND a late-stream is set: the per-turn sink must
	// win — the autonomous late-stream must never clobber a live turn.
	r.SetLateStream(late)
	r.Register(turnSink)
	r.Emit(context.Background(), turnevent.TextDelta{Delta: "during real turn"})

	if got := turnSink.count.Load(); got != 1 {
		t.Errorf("turn sink events = %d, want 1", got)
	}
	if got := late.count.Load(); got != 0 {
		t.Errorf("late-stream events = %d, want 0 while a real turn is registered", got)
	}
	if got := fallback.count.Load(); got != 0 {
		t.Errorf("fallback events = %d, want 0", got)
	}
}

func TestSessionRouter_ClearLateStreamRevertsToFallback(t *testing.T) {
	t.Parallel()
	fallback := &recordingRouterSink{id: "fallback"}
	late := &recordingRouterSink{id: "late"}
	r := newSessionRouter(fallback)

	r.SetLateStream(late)
	r.ClearLateStream()
	r.Emit(context.Background(), turnevent.TextDelta{Delta: "after clear"})

	if got := late.count.Load(); got != 0 {
		t.Errorf("late-stream events after clear = %d, want 0", got)
	}
	if got := fallback.count.Load(); got != 1 {
		t.Errorf("fallback events after clear = %d, want 1", got)
	}
}

func TestSessionRouter_DeliversToPlatform_LateStream(t *testing.T) {
	t.Parallel()
	// Non-delivering fallback, delivering late-stream: with no per-turn sink,
	// the router reports the late-stream's answer.
	r := newSessionRouter(nonDeliveringSink{})
	if r.DeliversToPlatform() {
		t.Fatalf("precondition: non-delivering fallback should report false")
	}
	r.SetLateStream(&recordingRouterSink{id: "late"}) // DeliversToPlatform() == true
	if !r.DeliversToPlatform() {
		t.Errorf("router with delivering late-stream: DeliversToPlatform = false, want true")
	}
	r.ClearLateStream()
	if r.DeliversToPlatform() {
		t.Errorf("router after ClearLateStream: DeliversToPlatform = true, want false")
	}
}
