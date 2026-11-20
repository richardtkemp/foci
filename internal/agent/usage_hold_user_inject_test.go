package agent

import (
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/usagehold"
)

// #3195 (work Mac, 2026-11-20): an ask answer (a user-input injection) that
// the usage hold held blocked the session worker in waitUserTurnGates. Every
// later message, "/lb <text>" included, sat on the channel behind it, a bare
// /lb could not reach it, and the user saw nothing.

// askAnswer is a user-input injection (an ask answer, #2233) whose Run counts.
func askAnswer(sk string, ran *atomic.Int32) Envelope {
	return Envelope{SessionKey: sk, Text: "answer", Inject: &InjectMeta{
		Trigger: "ask_grader", UserInput: true, Run: func() { ran.Add(1) },
	}}
}

// TestUsageHold_HeldAskAnswerDoesNotBlockWorker: a held ask answer is parked,
// its sender is told, and a later "/lb <text>" runs its turn now.
func TestUsageHold_HeldAskAnswerDoesNotBlockWorker(t *testing.T) {
	a, told := forceAgent(t)
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)
	const sk = "test/s"
	testAcct(a).ObserveUsage(weekly(0.98))

	var ran atomic.Int32
	a.Enqueue(askAnswer(sk, &ran))
	// The worker has taken the answer before the next message arrives.
	if !waitFor(2*time.Second, func() bool { return !a.InboxHasPendingInput(sk) }) {
		t.Fatal("the answer was never taken off the queue")
	}
	a.Enqueue(Envelope{SessionKey: sk, Text: "/lb go ahead", Driver: d})
	if !waitFor(2*time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("the /lb message behind the held answer ran no turn; calls=%d", d.NumCalls())
	}
	if !waitFor(2*time.Second, func() bool { return told.Load() == 1 }) {
		t.Errorf("the sender of the held answer was not told (told %d)", told.Load())
	}
	if !a.UsageHeldInput(sk) {
		t.Error("UsageHeldInput false while the answer is held")
	}
	if ran.Load() != 0 {
		t.Fatal("the answer ran during the hold")
	}
	testAcct(a).ObserveUsage(weekly(0.10))
	if !waitFor(3*time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatal("the held answer did not run at lift")
	}
}

// TestLimitBreakHeld_RunsHeldAskAnswer: a bare /lb runs a held ask answer now;
// the hold stays on.
func TestLimitBreakHeld_RunsHeldAskAnswer(t *testing.T) {
	a, told := forceAgent(t)
	const sk = "test/s"
	testAcct(a).ObserveUsage(weekly(0.98))

	var ran atomic.Int32
	a.Enqueue(askAnswer(sk, &ran))
	if !waitFor(2*time.Second, func() bool { return told.Load() == 1 && a.UsageHeldInput(sk) }) {
		t.Fatalf("the answer was not held (told %d)", told.Load())
	}
	if got := a.LimitBreakHeld(sk); got != LimitBreakReleased {
		t.Fatalf("LimitBreakHeld = %v, want released", got)
	}
	if !waitFor(2*time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatal("the bare /lb did not run the held answer")
	}
	if a.UsageHold("").Phase != usagehold.Hold {
		t.Fatalf("phase = %v, want the hold still on", a.UsageHold("").Phase)
	}
	if !waitFor(2*time.Second, func() bool { return !a.UsageHeldInput(sk) }) {
		t.Error("UsageHeldInput still true after the answer ran")
	}
	if got := a.LimitBreakHeld(sk); got != LimitBreakNothingHeld {
		t.Errorf("second LimitBreakHeld = %v, want nothing held", got)
	}
}

// TestUsageHold_HeldAskAnswerRunsFirstAtLift: an answer held before a plain
// message runs before that message's turn at lift (#3121 arrival order).
func TestUsageHold_HeldAskAnswerRunsFirstAtLift(t *testing.T) {
	a, told := forceAgent(t)
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)
	const sk = "test/s"
	testAcct(a).ObserveUsage(weekly(0.98))

	callsAtAnswer := atomic.Int32{}
	callsAtAnswer.Store(-1)
	a.Enqueue(Envelope{SessionKey: sk, Text: "answer", Inject: &InjectMeta{
		Trigger: "ask_grader", UserInput: true,
		Run: func() { callsAtAnswer.Store(int32(d.NumCalls())) },
	}})
	a.Enqueue(Envelope{SessionKey: sk, Text: "M1", Driver: d})
	if !waitFor(2*time.Second, func() bool { return told.Load() == 2 }) {
		t.Fatalf("answer and message not both held (told %d)", told.Load())
	}
	testAcct(a).ObserveUsage(weekly(0.10))
	if !waitFor(3*time.Second, func() bool { return d.NumCalls() == 1 && callsAtAnswer.Load() >= 0 }) {
		t.Fatalf("at lift: calls=%d answer ran=%v", d.NumCalls(), callsAtAnswer.Load() >= 0)
	}
	if got := callsAtAnswer.Load(); got != 0 {
		t.Errorf("the answer ran after %d turn(s), want before M1's turn", got)
	}
}

// TestLimitBreakHeld_RunsAnswerBehindAutomatedSend: an automated send held
// ahead of the answer stays held (a bare /lb does not cover it), and the
// answer behind it still runs now.
func TestLimitBreakHeld_RunsAnswerBehindAutomatedSend(t *testing.T) {
	a, told := forceAgent(t)
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)
	const sk = "test/s"
	testAcct(a).ObserveUsage(weekly(0.98))

	a.Enqueue(Envelope{SessionKey: sk, Text: "auto", Driver: d, Automated: true})
	var ran atomic.Int32
	a.Enqueue(askAnswer(sk, &ran))
	if !waitFor(2*time.Second, func() bool { return told.Load() == 1 && a.lookupInbox(sk).userInjectsHeld.Load() == 1 }) {
		t.Fatalf("the answer was not held (told %d)", told.Load())
	}
	if got := a.LimitBreakHeld(sk); got != LimitBreakReleased {
		t.Fatalf("LimitBreakHeld = %v, want released", got)
	}
	if !waitFor(2*time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatal("the bare /lb did not run the answer held behind the automated send")
	}
	if d.NumCalls() != 0 {
		t.Errorf("the automated send ran during the hold (calls=%d)", d.NumCalls())
	}
}
