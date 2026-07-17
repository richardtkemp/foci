package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/turn"
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
// clearing via the 30-minute prune backstop) AND (b) OpenAutonomousTurn has
// adopted the CC run as a first-class foci turn.
//
// Condition (b)'s delivering status is derived from the REAL
// autonomousTurnSink resolution rather than a hand-picked bool: with
// ResolveLateConn unwired (nothing resolves) and DurableTurnSink wired to a
// sink shaped like production's turn.NewSessionSink (DeliversToPlatform() ==
// true unconditionally — see that method's doc comment), autonomousTurnSink
// takes the exact fallback branch the #1350 follow-up fix added, and
// markInFlight sees `true`. Before that fix landed, this same setup produced
// `false` and reproduced the block (up to 30 minutes in production, bounded
// only by SubagentTracker's prune backstop) — asserting against a
// hand-picked `false` today would just be pinning a state the real adoption
// path can no longer reach for any agent with a platform registered.
//
// TestInbox_PlatformTurn_NotBlockedByBackendAwaiting above already proves (a)
// alone does not block a platform turn. This test drives autonomousTurnSink
// for condition (b) to check the OTHER gate (inbox.go's sink-delivery / #767
// gate: `IsTurnInFlight && !IsInFlightDelivering`) against the actual
// delivering status the fixed code produces, per spec §4: a user's own
// follow-up must never wait on their own background subagent.
func TestInbox_PlatformTurn_NotBlockedByOrphanedAutonomousAdoption(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	sk := "test/s"

	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true) // (a): SubagentTracker.Pending() > 0, orphaned completion signal
	mgr := newMockDelegatedManager(t, be)
	a.DelegatedManager = mgr

	// (b): real autonomousTurnSink resolution. ResolveLateConn is left unset
	// (nothing resolves), so this exercises the DurableTurnSink fallback
	// branch — the same one production's OpenAutonomousTurn goes through.
	a.DurableTurnSink = func(string) turnevent.Sink { return turn.NewSessionSink(nil, sk, "autonomous") }
	sink, cleanup := a.autonomousTurnSink(nil, sk)
	if cleanup != nil {
		defer cleanup()
	}
	releaseAdoption := a.markInFlight(sk, sink.DeliversToPlatform())
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
		t.Fatal("platform turn was blocked by an orphaned autonomous adoption (backend awaiting + adopted turn in flight) — spec §4 says a user's own follow-up must never wait on their background subagent; this is the clutch #1350 wedge (up to 30 minutes in production, bounded only by SubagentTracker's prune backstop)")
	}
	<-done
}

// TestInbox_Inject_HeldWhileOrphanedAutonomousAdoption is the mirror of
// TestInbox_PlatformTurn_NotBlockedByOrphanedAutonomousAdoption above: same
// combined state (backend awaiting an autonomous run AND an adopted
// non-delivering turn in flight), but a SYSTEM inject (reflection/keepalive)
// instead of a platform message.
//
// Unlike the platform case, this one is SUPPOSED to hold — that's the
// correct, intentional behaviour TestInbox_Inject_HeldWhileAutonomousDelivering
// and TestInbox_Inject_HeldWhileBackendAwaitingPendingWork already pin
// individually. This test asserts it still holds when BOTH conditions are
// true at once (the exact state the platform-turn test above finds broken),
// to make the asymmetry explicit: the bug in #1350 is specifically that
// platform turns get caught by a gate that exists for, and should only ever
// hold, system-sourced work — running a reflection/keepalive in this state
// would rebind the shared session sink to the inject's own NopSink and
// silently drop the adopted run's output (#1068/#1070). So this side of the
// gate must NOT change when #767 is fixed for platform turns; only the
// platform-turn side should move.
func TestInbox_Inject_HeldWhileOrphanedAutonomousAdoption(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()

	sk := "test/s"

	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true) // (a): SubagentTracker.Pending() > 0, orphaned completion signal
	mgr := newMockDelegatedManager(t, be)
	a.DelegatedManager = mgr

	releaseAdoption := a.markInFlight(sk, false) // (b): OpenAutonomousTurn's own adoption, no live sink

	var ran atomic.Int32
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "reflection",
		Run:     func() { ran.Add(1) },
	}})

	if waitFor(300*time.Millisecond, func() bool { return ran.Load() == 1 }) {
		releaseAdoption()
		t.Fatal("reflection inject ran during an orphaned autonomous adoption (backend awaiting + non-delivering in-flight turn) — it will rebind the shared session sink to NopSink and drop the adopted run's text (#1068/#1070)")
	}

	// Clear both conditions — mirrors what the #1350 OnResult fix does for
	// (a) immediately on an interrupted/errored result, and what the adopted
	// turn's own CompletionChan close does for (b).
	be.setAwaiting(false)
	releaseAdoption()

	if !waitFor(time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatalf("reflection inject did not run after both conditions cleared; ran=%d", ran.Load())
	}
}
