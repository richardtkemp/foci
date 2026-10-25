package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/tools"
)

// userInputInject is the envelope shape the ask tool's user-input delivery
// produces (cmd/foci-gw asUserInput): Text carries the answer so the steer
// path can fold it, and InjectMeta.UserInput routes it through the user gates.
func userInputInject(sk, text, askReqID string, run func()) Envelope {
	return Envelope{
		SessionKey: sk,
		Text:       text,
		Inject:     &InjectMeta{Trigger: "ask_grader", Run: run, AskReqID: askReqID, UserInput: true},
	}
}

// TestUserInputInject_RunsWhileBackendPending pins requirement 3's timing
// half: an ask answer is never held by the pending-work hold or the adopted
// run hold — it runs while background work is pending, unlike the same
// trigger without the user-input marker.
func TestUserInputInject_RunsWhileBackendPending(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()

	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true)
	a.DelegatedManager = newMockDelegatedManager(t, be)

	sk := "test/s"
	var ran atomic.Int32
	a.Enqueue(userInputInject(sk, "the answer", "req-1", func() { ran.Add(1) }))

	if !waitFor(500*time.Millisecond, func() bool { return ran.Load() == 1 }) {
		t.Fatal("ask answer was held behind background work — it is the user's own input and must deliver at once (#2233)")
	}
}

// TestUserInputInject_SteersCCWhenTurnInFlight pins the steer half: with a
// turn in flight and steer mode on, the answer text steers the running turn
// (ImmediateInject(SourceSteer)) exactly like a typed message, and its Run
// closure never fires (the in-flight turn delivers).
func TestUserInputInject_SteersCCWhenTurnInFlight(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()
	a.SetInboxSteerMode(true)

	be := &recordingBackend{}
	a.SetInboxBackend(func(context.Context, string) (delegator.Delegator, error) { return be, nil })

	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true)

	var ran atomic.Int32
	a.Enqueue(userInputInject("test/s", "steer this answer", "req-1", func() { ran.Add(1) }))

	injects := be.Injects()
	if len(injects) != 1 {
		t.Fatalf("expected 1 steer inject, got %d (channel=%d)", len(injects), len(inb.drainAvailable()))
	}
	if injects[0].Source != delegator.SourceSteer {
		t.Errorf("Source = %v, want SourceSteer (the answer folds like typed text)", injects[0].Source)
	}
	if injects[0].Text != "steer this answer" {
		t.Errorf("Text = %q, want the answer text", injects[0].Text)
	}
	if ran.Load() != 0 {
		t.Error("a steered answer's Run closure fired — the in-flight turn owns delivery")
	}
}

// TestUserInputInject_SteersAPIBuffer: on an API agent (no backend), the
// answer takes the existing steer path — the API steer buffer.
func TestUserInputInject_SteersAPIBuffer(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()
	a.SetInboxSteerMode(true)

	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true)

	a.Enqueue(userInputInject("test/s", "buffered answer", "req-1", func() {}))

	entries := inb.drainSteer()
	if len(entries) != 1 || entries[0].Text != "buffered answer" {
		t.Fatalf("steer buffer = %+v, want the answer text", entries)
	}
}

// TestUserInputInject_SteersDuringAdoptedRun pins the same moment for an ask
// answer: the adopted autonomous run's backend turn is in flight (the worker's
// own flag is not), and the answer steers into it exactly as a typed message
// would — requirement 3's "if a text-only user message would steer at this
// moment, the answer text steers the same way".
func TestUserInputInject_SteersDuringAdoptedRun(t *testing.T) {
	t.Parallel()
	a := newTestAgent(t) // inbox not started — a queued envelope stays observable
	a.SetInboxSteerMode(true)

	be := &mockBackendDT{}
	be.setTurnInFlight(true) // the adopted run: backend in flight, worker idle
	a.DelegatedManager = newMockDelegatedManager(t, be)

	var ran atomic.Int32
	a.Enqueue(userInputInject("test/s", "answer during adopted run", "req-1", func() { ran.Add(1) }))

	injects := be.InjectsRecorded()
	if len(injects) != 1 || injects[0].Source != delegator.SourceSteer || injects[0].Text != "answer during adopted run" {
		t.Fatalf("injects = %+v, want one SourceSteer of the answer text", injects)
	}
	if ran.Load() != 0 {
		t.Error("a steered answer's Run closure fired — the running turn owns delivery")
	}
}

// TestUserInputInject_NotConsumedByPendingAsk pins requirement 3: a user-input
// inject is NEVER consumed by the pending-ask typed-reply capture (#884) —
// it IS an ask answer, not a reply to be captured as another answer.
func TestUserInputInject_NotConsumedByPendingAsk(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()
	a.SetInboxSteerMode(true)

	var handled atomic.Int32
	a.AskRouter = &tools.AskRouter{
		PendingForSession: func(string) string { return "ask-1" },
		IsPaused:          func(string) bool { return false },
		HandleResponse:    func(string, string) { handled.Add(1) },
	}

	be := &recordingBackend{}
	a.SetInboxBackend(func(context.Context, string) (delegator.Delegator, error) { return be, nil })

	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true)

	a.Enqueue(userInputInject("test/s", "not a capture", "req-1", func() {}))

	if handled.Load() != 0 {
		t.Error("the pending-ask capture consumed a user-input inject — an ask answer must never feed another ask (#2233)")
	}
	injects := be.Injects()
	if len(injects) != 1 || injects[0].Source != delegator.SourceSteer {
		t.Errorf("injects = %+v, want one SourceSteer (the answer steers, uncaptured)", injects)
	}
}

// TestUserInputInject_NotConsumedByPlanPrompt pins the other capture a typed
// message can fall into: a user-input inject must never deny a pending
// ExitPlanMode plan with its text (#858).
func TestUserInputInject_NotConsumedByPlanPrompt(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()
	a.SetInboxSteerMode(true)

	be := &planResponderBackend{planReqID: "req-plan"}
	a.SetInboxBackend(func(context.Context, string) (delegator.Delegator, error) { return be, nil })

	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true)

	a.Enqueue(userInputInject("test/s", "not plan feedback", "req-1", func() {}))

	if be.cancelCalls != 0 {
		t.Error("an ask answer denied a pending plan — the plan-cancel capture must never consume it (#2233)")
	}
	injects := be.Injects()
	if len(injects) != 1 || injects[0].Source != delegator.SourceSteer {
		t.Errorf("injects = %+v, want one SourceSteer (the answer steers, uncaptured)", injects)
	}
}

// TestUserInputInject_SteerDeclinedFallsBackToQueue: when the steer dispatch
// fails outright, the answer falls back to the session channel for a fresh
// turn — the same fallback a user message takes, never a drop.
func TestUserInputInject_SteerDeclinedFallsBackToQueue(t *testing.T) {
	t.Parallel()
	a := newTestAgent(t) // inbox not started — channel content stays observable
	a.SetInboxSteerMode(true)

	a.SetInboxBackend(func(context.Context, string) (delegator.Delegator, error) {
		return &failingSteerBackend{}, nil
	})

	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true)

	accepted := a.Enqueue(userInputInject("test/s", "must not be lost", "req-1", func() {}))
	if !accepted {
		t.Fatal("Enqueue = false; a failed steer that fell back to the queue was accepted")
	}
	got := inb.drainAvailable()
	if len(got) != 1 || got[0].Text != "must not be lost" || got[0].Inject == nil || !got[0].Inject.UserInput {
		t.Fatalf("queued envelope = %+v, want the user-input inject with its text", got)
	}
}

// TestUserInputInject_NoSteerWhenCompacting pins the compaction carve-out for
// the new steer path: during a /compact turn the answer queues for a clean
// fresh turn instead of folding into the compaction transcript (#856).
func TestUserInputInject_NoSteerWhenCompacting(t *testing.T) {
	t.Parallel()
	a := newTestAgent(t)
	a.SetInboxSteerMode(true)

	be := &recordingBackend{}
	a.SetInboxBackend(func(context.Context, string) (delegator.Delegator, error) { return be, nil })

	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true)
	a.markCompacting("test/s")

	a.Enqueue(userInputInject("test/s", "during compaction", "req-1", func() {}))

	if injects := be.Injects(); len(injects) != 0 {
		t.Errorf("answer steered during compaction: %+v", injects)
	}
	if got := inb.drainAvailable(); len(got) != 1 {
		t.Errorf("expected the answer queued, got %+v", got)
	}
}

// TestUserInputInject_NoSteerWhenSteerModeOff: with steer mode off the answer
// queues for a fresh turn, as a user message would.
func TestUserInputInject_NoSteerWhenSteerModeOff(t *testing.T) {
	t.Parallel()
	a := newTestAgent(t)

	be := &recordingBackend{}
	a.SetInboxBackend(func(context.Context, string) (delegator.Delegator, error) { return be, nil })

	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true)

	a.Enqueue(userInputInject("test/s", "queue me", "req-1", func() {}))

	if injects := be.Injects(); len(injects) != 0 {
		t.Errorf("answer steered with steer mode off: %+v", injects)
	}
	if got := inb.drainAvailable(); len(got) != 1 {
		t.Errorf("expected the answer queued, got %+v", got)
	}
}

// TestUserInputInject_WaitsForUserTurnGates pins requirement 3's remaining
// waits: the answer waits only for what a user turn waits for — a compaction
// in flight holds it, and it runs once the compaction clears.
func TestUserInputInject_WaitsForUserTurnGates(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()

	sk := "test/s"
	a.markCompacting(sk)

	var ran atomic.Int32
	a.Enqueue(userInputInject(sk, "after compaction", "req-1", func() { ran.Add(1) }))

	if waitFor(300*time.Millisecond, func() bool { return ran.Load() == 1 }) {
		t.Fatal("ask answer dispatched mid-compaction — it waits the user-turn gates (#856)")
	}
	a.clearCompacting(sk)
	if !waitFor(time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatal("ask answer did not run after the compaction cleared")
	}
}

// TestUserInputInject_NotDeferredBehindOtherAsk pins the #1712 rule applied
// to the answer route: an answer for ask N is not held behind an unrelated,
// later ask N+1.
func TestUserInputInject_NotDeferredBehindOtherAsk(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()

	sk := "test/s"
	a.AskRouter = &tools.AskRouter{
		PendingForSession: func(string) string { return "ask-2" },
		IsPaused:          func(string) bool { return false },
	}

	var ran atomic.Int32
	a.Enqueue(userInputInject(sk, "verdict for ask-1", "ask-1", func() { ran.Add(1) }))

	if !waitFor(500*time.Millisecond, func() bool { return ran.Load() == 1 }) {
		t.Fatal("an ask-1 verdict was deferred behind the unrelated ask-2 (#1712)")
	}
}

// TestUserInputInject_DeferredBehindSameAsk pins the still-closed half of the
// same rule: an inject whose AskReqID matches the ask still pending keeps the
// #1712 same-ask deferral.
func TestUserInputInject_DeferredBehindSameAsk(t *testing.T) {
	t.Parallel()
	a, cancel := startedAgent(t)
	defer cancel()

	sk := "test/s"
	a.AskRouter = &tools.AskRouter{
		PendingForSession: func(string) string { return "ask-1" },
		IsPaused:          func(string) bool { return false },
	}

	var ran atomic.Int32
	a.Enqueue(userInputInject(sk, "same-ask message", "ask-1", func() { ran.Add(1) }))

	if waitFor(300*time.Millisecond, func() bool { return ran.Load() == 1 }) {
		t.Fatal("a same-ask inject ran while its own ask was still pending (#1712 must stay closed)")
	}
}
