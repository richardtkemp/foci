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

// TestInbox_Inject_HeldWhileBackendAwaitingPendingWork is the Phase 2 (spec §4)
// gate. Before an autonomous run even STARTS — while a backgrounded subagent/Bash
// is merely pending — the backend reports AwaitingAutonomousRun()==true, and a
// system inject must be held: that pending work will chain an autonomous run that
// owns delivery. Nothing is markInFlight yet, so this is RED without the
// backendAwaitingAutonomousRun predicate (the inject would run immediately and
// poison the run's sink). Release comes via the wait loop's poll backstop, since
// a pending→clear transition has no InFlightWaitCh edge.
func TestInbox_Inject_HeldWhileBackendAwaitingPendingWork(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true)
	mgr := newMockDelegatedManager(t, be) // registers the backend under "test/s"
	a.DelegatedManager = mgr

	sk := "test/s"
	var ran atomic.Int32
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "reflection",
		Run:     func() { ran.Add(1) },
	}})

	if waitFor(300*time.Millisecond, func() bool { return ran.Load() == 1 }) {
		t.Fatal("reflection inject ran while the backend reported pending background work (spec §4) — its autonomous run would own delivery")
	}

	// Background work completes → AwaitingAutonomousRun flips false; the ~1s poll
	// backstop wakes the parked worker and the inject proceeds.
	be.setAwaiting(false)
	if !waitFor(2*time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatalf("reflection inject did not run after pending work cleared; ran=%d", ran.Load())
	}
}

// TestInbox_PlatformTurn_NotBlockedByBackendAwaiting pins the spec §4 scope: the
// pending-work gate holds SYSTEM injects only. A platform turn (a user message on
// the Driver path) must dispatch immediately even while the backend is awaiting an
// autonomous run — user input adopts/folds delivering work (spec §3), it is never
// held on pending work.
func TestInbox_PlatformTurn_NotBlockedByBackendAwaiting(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true)
	mgr := newMockDelegatedManager(t, be)
	a.DelegatedManager = mgr

	hook := make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	d := &recordingDriver{hookCh: hook, doneCh: done}
	a.SetTurnObserver(d.recordBatch)

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "addendum", Driver: d})

	select {
	case <-hook:
		// expected — the platform path never consults the pending-work gate.
	case <-time.After(time.Second):
		t.Fatal("platform turn was blocked while the backend was awaiting an autonomous run; spec §4 gates system input only")
	}
	<-done
}
