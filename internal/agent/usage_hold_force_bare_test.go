package agent

import (
	"testing"
	"time"

	"foci/internal/usagehold"
)

// #3182 (Dick 2026-11-19): a bare /lb "should operate on the previous message
// sent by the user, releasing the hold so it sends to the backend
// immediately"; with no hold on it shows the usage.

// TestLimitBreakHeld_RunsAllParked: three plain messages and an automated
// send are held; a bare /lb runs the three as one turn, oldest first, now;
// the hold stays on and the automated send stays held until the lift.
func TestLimitBreakHeld_RunsAllParked(t *testing.T) {
	a, told := forceAgent(t)
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)
	const sk = "test/s"

	testAcct(a).ObserveUsage(weekly(0.98))
	for _, m := range []string{"M1", "M2", "M3"} {
		a.Enqueue(Envelope{SessionKey: sk, Text: m, Driver: d})
	}
	a.Enqueue(Envelope{SessionKey: sk, Text: "auto", Driver: d, Automated: true})
	if !waitFor(2*time.Second, func() bool { return told.Load() == 3 && a.lookupInbox(sk).usageHeldLen() == 4 }) {
		t.Fatalf("messages not held (told %d)", told.Load())
	}
	if got := a.LimitBreakHeld(sk); got != LimitBreakReleased {
		t.Fatalf("LimitBreakHeld = %v, want released", got)
	}
	if !waitFor(2*time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("the held messages ran no turn; calls=%d", d.NumCalls())
	}
	time.Sleep(100 * time.Millisecond)
	if d.NumCalls() != 1 {
		t.Fatalf("calls = %d, want the released messages as one turn", d.NumCalls())
	}
	if got := texts(d.Calls()[0]); len(got) != 3 || got[0] != "M1" || got[1] != "M2" || got[2] != "M3" {
		t.Fatalf("bare /lb turn = %q, want [M1 M2 M3]", got)
	}
	if a.UsageHold("").Phase != usagehold.Hold {
		t.Fatalf("phase = %v, want the hold still on", a.UsageHold("").Phase)
	}
	if !waitFor(2*time.Second, func() bool { return a.UsageHeldInput(sk) }) {
		t.Error("UsageHeldInput false while the automated send is still held")
	}
	if got := a.LimitBreakHeld(sk); got != LimitBreakNothingHeld {
		t.Fatalf("second LimitBreakHeld = %v, want nothing held (only the automated send)", got)
	}

	// The next plain message is held again.
	a.Enqueue(Envelope{SessionKey: sk, Text: "M4", Driver: d})
	if !waitFor(2*time.Second, func() bool { return told.Load() == 4 }) {
		t.Fatalf("the message after the bare /lb was not held (told %d)", told.Load())
	}

	testAcct(a).ObserveUsage(weekly(0.10))
	if !waitFor(2*time.Second, func() bool { return d.NumCalls() == 2 }) {
		t.Fatalf("held messages did not run at lift; calls=%d", d.NumCalls())
	}
	if got := texts(d.Calls()[1]); len(got) != 2 || got[0] != "auto" || got[1] != "M4" {
		t.Errorf("lift turn = %q, want [auto M4]", got)
	}
}

// TestLimitBreakHeld_NoHoldNothingHeld: no hold → NoHold; a hold with
// nothing held → NothingHeld; neither runs a turn. An automated send is not
// the user's message.
func TestLimitBreakHeld_NoHoldNothingHeld(t *testing.T) {
	a, _ := forceAgent(t)
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)
	const sk = "test/s"

	if got := a.LimitBreakHeld(sk); got != LimitBreakNoHold {
		t.Fatalf("no hold: LimitBreakHeld = %v, want no hold", got)
	}
	testAcct(a).ObserveUsage(weekly(0.98))
	if got := a.LimitBreakHeld(sk); got != LimitBreakNothingHeld {
		t.Fatalf("hold, no inbox: LimitBreakHeld = %v, want nothing held", got)
	}
	a.Enqueue(Envelope{SessionKey: sk, Text: "auto", Driver: d, Automated: true})
	if !waitFor(2*time.Second, func() bool { return a.UsageHeldInput(sk) }) {
		t.Fatal("the automated send was not held")
	}
	if got := a.LimitBreakHeld(sk); got != LimitBreakNothingHeld {
		t.Fatalf("hold, only an automated send held: LimitBreakHeld = %v, want nothing held", got)
	}
	time.Sleep(100 * time.Millisecond)
	if d.NumCalls() != 0 {
		t.Errorf("a turn ran: %d calls", d.NumCalls())
	}
}

// TestLimitBreakHeld_QueuedBehindRunningTurn: while a turn runs, P1 is
// parked (it arrived first and the worker took it) and Q1, Q2 wait on the
// worker's queue. A bare /lb releases all three: right after the running
// turn they run as one turn, in arrival order, and nothing is parked.
func TestLimitBreakHeld_QueuedBehindRunningTurn(t *testing.T) {
	a, told := forceAgent(t)
	d := newGateDriver()
	a.SetTurnObserver(d.recordBatch)
	const sk = "test/c1"
	testAcct(a).ObserveUsage(weekly(0.98))

	a.Enqueue(Envelope{SessionKey: sk, Text: "P1", Driver: d})
	if !waitFor(2*time.Second, func() bool { return told.Load() == 1 }) {
		t.Fatalf("P1 was not held (told %d)", told.Load())
	}
	a.Enqueue(Envelope{SessionKey: sk, Text: "/lb first", Driver: d})
	select {
	case <-d.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the /lb turn never started")
	}
	a.Enqueue(Envelope{SessionKey: sk, Text: "Q1", Driver: d})
	a.Enqueue(Envelope{SessionKey: sk, Text: "Q2", Driver: d})
	if got := a.LimitBreakHeld(sk); got != LimitBreakReleased {
		t.Fatalf("LimitBreakHeld = %v, want released", got)
	}
	if got := a.LimitBreakHeld(sk); got != LimitBreakNothingHeld {
		t.Fatalf("second LimitBreakHeld = %v, want nothing held (already released)", got)
	}
	close(d.release)
	if !waitFor(2*time.Second, func() bool { return d.NumCalls() == 2 }) {
		t.Fatalf("calls=%d, want the released messages to run", d.NumCalls())
	}
	if got := texts(d.Calls()[1]); len(got) != 3 || got[0] != "P1" || got[1] != "Q1" || got[2] != "Q2" {
		t.Errorf("second turn = %q, want [P1 Q1 Q2]", got)
	}
	time.Sleep(100 * time.Millisecond)
	if told.Load() != 1 {
		t.Errorf("a released message was parked (told %d, want 1: P1 before the /lb)", told.Load())
	}
	if got := a.LimitBreakHeld(sk); got != LimitBreakNothingHeld {
		t.Errorf("after they ran: LimitBreakHeld = %v, want nothing held", got)
	}
}
