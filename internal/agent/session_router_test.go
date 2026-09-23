package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"foci/internal/turnevent"
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

// wrappedRouterSink stands in for the decorators (telemetry.turnSink,
// loggingSink) that forward to an inner sink and expose it via Unwrap.
type wrappedRouterSink struct{ inner turnevent.Sink }

func (w *wrappedRouterSink) Emit(ctx context.Context, ev turnevent.Event) { w.inner.Emit(ctx, ev) }
func (w *wrappedRouterSink) DeliversToPlatform() bool                     { return w.inner.DeliversToPlatform() }
func (w *wrappedRouterSink) Unwrap() turnevent.Sink                       { return w.inner }

// #1944: a decorator of the router must never become the router's current
// sink — Emit would ping-pong between them until the stack overflows. The
// guard has to look through the decoration, because the wrapper is a
// different value from the router.
func TestSessionRouter_RefusesToRegisterWrappedSelf(t *testing.T) {
	t.Parallel()
	fallback := &recordingRouterSink{id: "fallback"}
	r := newSessionRouter(fallback)
	var warned int
	r.warnf = func(string, ...any) { warned++ }

	platform := &recordingRouterSink{id: "platform"}
	r.Register(platform) // what RunTurn does for a platform turn

	for name, sink := range map[string]turnevent.Sink{
		"router itself": r,
		"wrapped once":  &wrappedRouterSink{inner: r},
		"wrapped twice": &wrappedRouterSink{inner: &wrappedRouterSink{inner: r}},
	} {
		if !r.routesTo(sink) {
			t.Errorf("%s: routesTo = false, want true", name)
		}
		r.Register(sink) // what Phase 3.5 did before the fix
		// The platform registration must survive, and Emit must terminate.
		r.Emit(context.Background(), turnevent.ToolCall{ID: "t1", Name: "Bash"})
	}
	if got := platform.count.Load(); got != 3 {
		t.Errorf("platform sink events = %d, want 3 (registration replaced or events lost)", got)
	}
	if fallback.count.Load() != 0 {
		t.Errorf("fallback received events; the platform registration was dropped")
	}
	if warned != 3 {
		t.Errorf("warnf calls = %d, want 3", warned)
	}
}

func TestSessionRouter_RoutesTo_FalseForUnrelatedSinks(t *testing.T) {
	t.Parallel()
	r := newSessionRouter(&recordingRouterSink{id: "fallback"})
	other := newSessionRouter(&recordingRouterSink{id: "other-fallback"})
	for name, sink := range map[string]turnevent.Sink{
		"plain sink":           &recordingRouterSink{id: "x"},
		"wrapped plain sink":   &wrappedRouterSink{inner: &recordingRouterSink{id: "y"}},
		"a different router":   other,
		"wrapped other router": &wrappedRouterSink{inner: other},
		"nil":                  nil,
	} {
		if r.routesTo(sink) {
			t.Errorf("%s: routesTo = true, want false", name)
		}
	}
	// And a fresh sink still registers normally.
	fresh := &recordingRouterSink{id: "fresh"}
	r.Register(fresh)
	r.Emit(context.Background(), turnevent.TextBlock{Text: "hi", Phase: turnevent.PhaseIntermediate})
	if fresh.count.Load() != 1 {
		t.Errorf("fresh sink events = %d, want 1", fresh.count.Load())
	}
}
