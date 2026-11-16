package agent

import (
	"testing"

	"foci/internal/delegator"
	"foci/internal/usagehold"
)

// #3131 (seen 16 Nov on the work Mac, Stuart): during a usage hold a foci_remind
// wake runs (the hold lets wakes through), and Dick's "/lb directory followup
// now please" waited ten minutes behind it instead of steering in. The usage
// hold gate in Enqueue sent every user message to the worker queue, the
// limit-break message included, so it never reached trySteer. #3128 says a
// limit-break message runs "now"; #3120 Q57 says user messages steer into a
// remind-wake turn.

// heldWorkTurnAgent is a Claude Code agent in a usage hold with a remind-wake
// work turn running on test/s and steer mode on.
func heldWorkTurnAgent(t *testing.T) (*Agent, *typingBackendDT) {
	t.Helper()
	a, be, _ := newTypingTestAgent(t)
	a.DelegatedManager.BackendType = "claude-code"
	a.UsageHoldRegistry = usagehold.New()
	a.StartUsageHold()
	t.Cleanup(a.StopUsageHold)
	a.ObserveUsage(weekly(0.98))
	if a.UsageHold().Phase != usagehold.Hold {
		t.Fatalf("phase = %v, want hold", a.UsageHold().Phase)
	}
	runTypingTurn(t, a, deliveringCtx(), "scheduled_wake")
	be.setTurnInFlight(true) // the wake's turn is still running
	a.SetInboxSteerMode(true)
	return a, be
}

// TestLimitBreak_SteersIntoWorkTurnDuringHold: the limit-break message folds
// into the running wake turn at once, without its word.
func TestLimitBreak_SteersIntoWorkTurnDuringHold(t *testing.T) {
	t.Parallel()
	a, be := heldWorkTurnAgent(t)
	inb := a.getOrCreateInbox("test/s")

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "/lb directory followup now please"})

	injects := be.InjectsRecorded()
	if last := injects[len(injects)-1]; last.Source != delegator.SourceSteer || last.Text != "directory followup now please" {
		t.Fatalf("last inject = {Source:%v Text:%q}, want the /lb text steered into the wake turn", last.Source, last.Text)
	}
	if q := inb.drainAvailable(); len(q) != 0 {
		t.Fatalf("/lb message queued behind the wake turn instead of steering: %q", texts(q))
	}
}

// TestUsageHold_PlainMessageStillWaitsDuringWorkTurn: the hold itself is
// unchanged for a plain message: it does not steer into the wake turn.
func TestUsageHold_PlainMessageStillWaitsDuringWorkTurn(t *testing.T) {
	t.Parallel()
	a, be := heldWorkTurnAgent(t)
	before := len(be.InjectsRecorded())

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "plain"})

	for _, inj := range be.InjectsRecorded()[before:] {
		if inj.Source == delegator.SourceSteer {
			t.Fatalf("a plain message steered into the wake turn during a usage hold: %q", inj.Text)
		}
	}
}
