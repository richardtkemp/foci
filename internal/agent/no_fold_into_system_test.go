package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
)

// User input never folds into a running SYSTEM turn (#2591, Dick's ruling
// 2026-10-31): it waits for the keepalive / reflection / cron / notification
// turn to end and runs as its own turn. Folding into a running USER turn is
// unchanged.

// foldRecorder counts the mock backend's in-flight folds (SendCommand is the
// mock's fold seam for SourceUser/SourceSteer in flight) and fresh begins.
type foldRecorder struct {
	folds  atomic.Int32
	begins atomic.Int32
}

func (r *foldRecorder) install(be *mockBackendDT) {
	be.sendCommandFn = func(context.Context, string) error { r.folds.Add(1); return nil }
	be.sendToPaneFn = func(context.Context, string, *mockHandler) (*delegator.TurnResult, error) {
		r.begins.Add(1)
		return &delegator.TurnResult{Text: "ok"}, nil
	}
}

// TestNoFold_UserInputWaitsOutSystemTurn: a user message (voice) or an ask
// answer arriving while a system turn runs waits for it to end, then begins
// its own fresh SourceUser turn — it is never written into the system turn.
func TestNoFold_UserInputWaitsOutSystemTurn(t *testing.T) {
	t.Parallel()
	users := []struct {
		name    string
		ctx     context.Context
		trigger string
	}{
		{"voice", context.Background(), "voice"},
		{"ask answer", WithUserInput(context.Background()), "ask_answer"},
	}
	for _, sys := range []string{TriggerKeepalive, "reflection", "scheduled_wake", "user", "webhook"} {
		for _, u := range users {
			t.Run(sys+"/"+u.name, func(t *testing.T) {
				t.Parallel()
				a, be, _ := newTypingTestAgent(t)
				runTypingTurn(t, a, context.Background(), sys)
				be.setTurnInFlight(true) // the system turn is still running

				rec := &foldRecorder{}
				rec.install(be.mockBackendDT)
				var waited atomic.Int32
				be.waitForTurnFn = func(context.Context) error {
					waited.Add(1)
					be.setTurnInFlight(false) // the system turn ends
					return nil
				}
				runTypingTurn(t, a, u.ctx, u.trigger)

				if n := rec.folds.Load(); n != 0 {
					t.Fatalf("user input folded into the running %q turn (%d folds), want it to wait (#2591)", sys, n)
				}
				if waited.Load() == 0 {
					t.Fatalf("user input did not wait for the running %q turn", sys)
				}
				injects := be.InjectsRecorded()
				if len(injects) != 2 || injects[1].Source != delegator.SourceUser || rec.begins.Load() != 1 {
					t.Fatalf("injects = %d (last source %v), begins = %d; want the system begin plus one fresh SourceUser begin",
						len(injects), injects[len(injects)-1].Source, rec.begins.Load())
				}
			})
		}
	}
}

// TestNoFold_UserInputStillFoldsIntoUserTurn is the regression arm: a second
// user message during a running user turn folds into it as before, no wait.
func TestNoFold_UserInputStillFoldsIntoUserTurn(t *testing.T) {
	t.Parallel()
	a, be, _ := newTypingTestAgent(t)
	runTypingTurn(t, a, context.Background(), "voice")
	be.setTurnInFlight(true)

	rec := &foldRecorder{}
	rec.install(be.mockBackendDT)
	be.waitForTurnFn = func(context.Context) error {
		t.Error("user input waited for a running user turn; it must fold")
		be.setTurnInFlight(false)
		return nil
	}
	runTypingTurn(t, a, context.Background(), "voice")
	if n := rec.folds.Load(); n != 1 {
		t.Fatalf("folds = %d, want 1 (folding into a user turn is unchanged)", n)
	}
}

// TestNoFold_SystemMarkClearsAtCompletion: once the system turn completes, a
// backend run foci did not begin (an adopted autonomous run) is no longer
// treated as a system turn, so user input folds into it as before.
func TestNoFold_SystemMarkClearsAtCompletion(t *testing.T) {
	t.Parallel()
	a, be, _ := newTypingTestAgent(t)
	var complete func(*delegator.TurnResult)
	be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
		complete = h.OnTurnComplete
		return &delegator.TurnResult{Text: "ok"}, nil
	}
	runTypingTurn(t, a, context.Background(), TriggerKeepalive)
	be.setTurnInFlight(true)
	if !a.DelegatedManager.SystemTurnInFlight("test/s") {
		t.Fatal("SystemTurnInFlight = false while the keepalive runs")
	}
	if complete == nil {
		t.Fatal("keepalive begin installed no OnTurnComplete")
	}
	complete(&delegator.TurnResult{Text: "done"})
	// An autonomous run starts on the same backend.
	if a.DelegatedManager.SystemTurnInFlight("test/s") {
		t.Fatal("SystemTurnInFlight = true after the keepalive completed; an adopted run would be mistaken for it")
	}
}

// TestNoFold_RefusedKeepaliveLeavesNoMark: a keepalive refused because a user
// turn is in flight must not mark that user turn as a system turn.
func TestNoFold_RefusedKeepaliveLeavesNoMark(t *testing.T) {
	t.Parallel()
	a, be, _ := newTypingTestAgent(t)
	runTypingTurn(t, a, context.Background(), "voice")
	be.setTurnInFlight(true)

	ctx := WithTrigger(context.Background(), TriggerKeepalive)
	ts := NewTurnState(ctx, "test/s", []string{"prompt"}, nil)
	ts.Trigger = TriggerKeepalive
	ts.Prompt = "prompt"
	ts.StartedAt = time.Now()
	_ = (&DelegatedTransport{sharedTurnOps{agent: a}}).RunInference(ts) // ErrKeepaliveSkipped

	if a.DelegatedManager.SystemTurnInFlight("test/s") {
		t.Fatal("a refused keepalive marked the running user turn as a system turn")
	}
}

// TestNoFold_InboxDoesNotSteerIntoSystemTurn: with steer mode on, a typed
// message arriving while a system turn runs is queued for its own turn, not
// steered (ImmediateInject(SourceSteer)) into the system turn.
func TestNoFold_InboxDoesNotSteerIntoSystemTurn(t *testing.T) {
	t.Parallel()
	a, be, _ := newTypingTestAgent(t)
	runTypingTurn(t, a, context.Background(), TriggerKeepalive)
	be.setTurnInFlight(true)
	a.SetInboxSteerMode(true)
	inb := a.getOrCreateInbox("test/s") // inbox not started: a queued envelope stays observable

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "hello during keepalive"})

	for _, inj := range be.InjectsRecorded() {
		if inj.Source == delegator.SourceSteer {
			t.Fatalf("message steered into the running keepalive turn: %+v (#2591)", inj)
		}
	}
	if queued := inb.drainAvailable(); len(queued) != 1 || queued[0].Text != "hello during keepalive" {
		t.Fatalf("queued = %+v, want the message queued for its own turn", queued)
	}
}

// TestNoFold_InboxStillSteersIntoUserTurn is the inbox regression arm.
func TestNoFold_InboxStillSteersIntoUserTurn(t *testing.T) {
	t.Parallel()
	a, be, _ := newTypingTestAgent(t)
	runTypingTurn(t, a, context.Background(), "voice")
	be.setTurnInFlight(true)
	a.SetInboxSteerMode(true)
	inb := a.getOrCreateInbox("test/s")

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "follow-up"})

	injects := be.InjectsRecorded()
	if last := injects[len(injects)-1]; last.Source != delegator.SourceSteer || last.Text != "follow-up" {
		t.Fatalf("last inject = %+v, want the follow-up steered into the user turn", last)
	}
	if queued := inb.drainAvailable(); len(queued) != 0 {
		t.Fatalf("follow-up queued instead of steered: %+v", queued)
	}
}
