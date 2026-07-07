package agent

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestInbox_Inject_HeldWhileAutonomousDelivering is the #1070 repro. When CC
// runs an autonomous turn (one foci opened no turn for), foci learns of it via
// session_state:running and adopts it as an in-flight *delivering* turn
// (markInFlight(sk, true)). The single-threaded inbox worker is otherwise idle
// during that run — foci is not occupying it — so a reflection/keepalive
// injection that arrives now would be dequeued and run at inbox.go:691.
//
// Running it is the #1068 bug: the injection's RunInference calls
// AttachSessionEvents (turn_delegated.go), which rebinds the SHARED
// session-scoped se.OnText to its own NopSink (a reflection has no sink on
// ctx). The concurrent autonomous run shares that se, so its subsequent text
// blocks emit into NopSink and are silently dropped.
//
// The fix: while an autonomous delivering run is in flight, the worker must
// HOLD the injection (mirroring the #767 gate) and release it only when the
// run ends. This test drives the real Enqueue → sessionWorker → runInject path
// and asserts the injection does not run until the adopted turn clears.
func TestInbox_Inject_HeldWhileAutonomousDelivering(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	sk := "test/s"

	// Model adoption: onAutonomousStart marks the run in-flight-delivering.
	done := a.markInFlight(sk, true)

	var ran atomic.Int32
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "reflection",
		Run:     func() { ran.Add(1) },
	}})

	// While the autonomous run is in flight the injection must be HELD — running
	// it now would poison the shared session sink and drop the run's output.
	if waitFor(300*time.Millisecond, func() bool { return ran.Load() == 1 }) {
		t.Fatal("reflection inject ran while an autonomous delivering run was in flight (#1068/#1070) — it will rebind the shared session sink to NopSink and drop the run's text")
	}

	// The autonomous run ends (onAutonomousEnd) → the injection now proceeds.
	done()
	if !waitFor(time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatalf("reflection inject did not run after the autonomous run ended; ran=%d", ran.Load())
	}
}
