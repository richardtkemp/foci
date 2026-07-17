package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestWaitInjectGate_HoldsUntilClear covers the Phase 3 extracted gate helper
// applied at both runInject sites (the dequeue path and the post-batch
// heldInjects loop). It blocks while the backend reports pending/autonomous
// work and returns true once that clears — released here via the poll backstop
// since a pending→clear transition has no InFlightWaitCh edge.
func TestWaitInjectGate_HoldsUntilClear(t *testing.T) {
	a := newTestAgent(t)
	be := &mockBackendDT{}
	be.setAwaiting(true)
	mgr := newMockDelegatedManager(t, be)
	a.DelegatedManager = mgr

	returned := make(chan bool, 1)
	go func() { returned <- a.waitInjectGate(context.Background(), "test/s") }()
	select {
	case <-returned:
		t.Fatal("waitInjectGate returned while the backend was awaiting; expected to block")
	case <-time.After(300 * time.Millisecond):
	}

	be.setAwaiting(false)
	select {
	case ok := <-returned:
		if !ok {
			t.Fatal("waitInjectGate returned false when the gate opened, want true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitInjectGate did not return after awaiting cleared")
	}
}

// TestWaitInjectGate_CtxCancelReturnsFalse pins the shutdown contract both call
// sites depend on: a cancelled ctx makes the gate return false so the worker
// stops rather than spinning.
func TestWaitInjectGate_CtxCancelReturnsFalse(t *testing.T) {
	a := newTestAgent(t)
	be := &mockBackendDT{}
	be.setAwaiting(true)
	mgr := newMockDelegatedManager(t, be)
	a.DelegatedManager = mgr

	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan bool, 1)
	go func() { returned <- a.waitInjectGate(ctx, "test/s") }()
	cancel()
	select {
	case ok := <-returned:
		if ok {
			t.Fatal("waitInjectGate returned true on ctx cancel, want false")
		}
	case <-time.After(time.Second):
		t.Fatal("waitInjectGate did not return on ctx cancel")
	}
}

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

// TestInbox_PlatformTurn_NotBlockedByOrphanedAutonomousAdoption reproduces the
// production combination behind clutch todo #1350 (2026-07-17): a background
// subagent's completion signal never arrives, so (a) the backend keeps
// reporting AwaitingAutonomousRun()==true (SubagentTracker.Pending()>0, only
// clearing via the 30-minute prune backstop) AND (b) OpenAutonomousTurn had
// already adopted the CC run CC kept executing as a first-class, NON-delivering
// foci turn (markInFlight(sk, false) — no live app/platform connection was
// bound at adoption time), per in_flight.go's
// `markInFlight(sessionKey, sink.DeliversToPlatform())`.
//
// TestInbox_PlatformTurn_NotBlockedByBackendAwaiting above already proves (a)
// alone does not block a platform turn. This test additionally holds condition
// (b) — the real second half of what production actually had in flight — to
// check whether the OTHER gate (inbox.go's sink-delivery / #767 gate:
// `IsTurnInFlight && !IsInFlightDelivering`) reintroduces exactly the block
// spec §4 says platform turns must never see. In the live incident the user's
// own follow-up ("stop your agent, rebase to main, merge to main…") sat
// undispatched for ~28 minutes — bounded by the tracker's defaultAgentMaxAge
// prune, not by anything session-specific — which is the behaviour this test
// pins as wrong.
func TestInbox_PlatformTurn_NotBlockedByOrphanedAutonomousAdoption(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	sk := "test/s"

	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true) // (a): SubagentTracker.Pending() > 0, orphaned completion signal
	mgr := newMockDelegatedManager(t, be)
	a.DelegatedManager = mgr

	releaseAdoption := a.markInFlight(sk, false) // (b): OpenAutonomousTurn's own adoption, no live sink
	defer releaseAdoption()

	hook := make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	d := &recordingDriver{hookCh: hook, doneCh: done}
	a.SetTurnObserver(d.recordBatch)

	a.Enqueue(Envelope{SessionKey: sk, Text: "stop your agent, rebase to main, merge to main", Driver: d})

	select {
	case <-hook:
		// expected — a genuine user message must not wait on the user's own
		// background subagent, however that subagent's state is tracked.
	case <-time.After(time.Second):
		t.Fatal("platform turn was blocked by an orphaned autonomous adoption (backend awaiting + non-delivering in-flight turn) — spec §4 says a user's own follow-up must never wait on their background subagent; this is the clutch #1350 wedge (up to 30 minutes in production, bounded only by SubagentTracker's prune backstop)")
	}
	<-done
}
