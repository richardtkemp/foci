package agent

import (
	"context"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/relogin"
	"foci/internal/tools"
)

// An Automated envelope is a choice-carrying HTTP /send that did not declare a
// human sender (#1318 threat scan, #1130). It may fold or queue like any user
// message, but it must never answer for the human: not as a pending foci_ask's
// answer, not as plan-revision feedback, not as the CC re-login code. The
// platform envelopes (Telegram/Discord) keep capturing — the existing
// PendingAsk/plan-cancel/relogin tests pin that side.

// TestInbox_Enqueue_Automated_PendingAsk_NotCaptured: mid-turn, with an
// unpaused ask pending, an automated steer folds into the running turn instead
// of being taken as the ask's answer.
func TestInbox_Enqueue_Automated_PendingAsk_NotCaptured(t *testing.T) {
	t.Parallel()
	a := newTestAgent(t)
	a.SetInboxSteerMode(true)

	var handled bool
	a.AskRouter = &tools.AskRouter{
		PendingForSession: func(string) string { return "ask-1" },
		IsPaused:          func(string) bool { return false },
		HandleResponse:    func(string, string) { handled = true },
	}
	be := &recordingBackend{}
	a.SetInboxBackend(func(_ context.Context, _ string) (delegator.Delegator, error) {
		return be, nil
	})
	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true)

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "yes", Steer: platform.SteerAlways, Automated: true})

	if handled {
		t.Fatal("an automated send answered the pending ask as if it were the human")
	}
	injects := be.Injects()
	if len(injects) != 1 || injects[0].Source != delegator.SourceSteer || injects[0].Text != "yes" {
		t.Errorf("expected the automated steer to fold into the running turn, got %+v", injects)
	}
}

// TestInbox_Enqueue_Automated_PendingPlan_NotDenied: mid-turn, with an
// ExitPlanMode approval pending, an automated steer must not deny the plan.
func TestInbox_Enqueue_Automated_PendingPlan_NotDenied(t *testing.T) {
	t.Parallel()
	a := newTestAgent(t)
	a.SetInboxSteerMode(true)

	be := &planResponderBackend{planReqID: "req-plan"}
	a.SetInboxBackend(func(_ context.Context, _ string) (delegator.Delegator, error) {
		return be, nil
	})
	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true)

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "no", Steer: platform.SteerAlways, Automated: true})

	if be.cancelCalls != 0 {
		t.Fatalf("CancelPlanWithFeedback called %d times by an automated send, want 0", be.cancelCalls)
	}
}

// TestInbox_Relogin_AutomatedNotCapturedAsCode: with the capture window open,
// an automated send is held like any message during re-login, and the window
// stays open for the user's code.
func TestInbox_Relogin_AutomatedNotCapturedAsCode(t *testing.T) {
	a := newTestAgent(t)
	a.AgentID = "clutch"
	a.DelegatedManager = &DelegatedManager{}

	claimRelogin(t)
	relogin.G.OpenCapture("clutch")
	if !a.Enqueue(Envelope{SessionKey: "test/s", Text: "not-the-code", Steer: platform.SteerAlways, Automated: true}) {
		t.Fatal("Enqueue rejected an automated send during re-login; it must be held")
	}
	if !relogin.G.ShouldCapture("clutch") {
		t.Fatal("automated send consumed the capture window; it must stay open for the user's code")
	}
	if inb := a.lookupInbox("test/s"); inb == nil || len(inb.ch) != 1 {
		t.Fatal("automated send was not held on the session channel")
	}
	if !a.Enqueue(Envelope{SessionKey: "test/s", Text: "the-code"}) {
		t.Fatal("capture should report the user's message as consumed")
	}
	if code, ok := relogin.G.AwaitCode(time.Second); !ok || code != "the-code" {
		t.Fatalf("AwaitCode = %q,%v; want the-code,true", code, ok)
	}
}

// TestTypedAnswerEligible_MixedBatch: RunTurn's idle ask capture skips a batch
// containing ANY automated envelope, wherever it sits — taking the first text
// would either answer with automation's text or silently drop it.
func TestTypedAnswerEligible_MixedBatch(t *testing.T) {
	t.Parallel()
	human := Envelope{Text: "yes"}
	auto := Envelope{Text: "cron says hi", Automated: true}
	for _, tc := range []struct {
		name  string
		batch []Envelope
		want  bool
	}{
		{"human only", []Envelope{human}, true},
		{"automated only", []Envelope{auto}, false},
		{"automated first", []Envelope{auto, human}, false},
		{"human first", []Envelope{human, auto}, false},
		{"queued human", []Envelope{{Text: "yes", Steer: platform.SteerNever}}, false},
	} {
		if got := typedAnswerEligible("telegram", tc.batch); got != tc.want {
			t.Errorf("%s: typedAnswerEligible = %v, want %v", tc.name, got, tc.want)
		}
	}
	if typedAnswerEligible(platformApp, []Envelope{human}) {
		t.Error("app batch must never be typed-answer captured")
	}
}

// TestInbox_Enqueue_Automated_BufferedSteerKeepsMarker: on an API-mode agent
// (no backend) a mid-turn automated steer lands in the steer buffer; the
// marker must survive the buffer and the orphan rebuild (buildFollowUp), or
// the follow-up turn's ask capture would treat it as the human's reply.
func TestInbox_Enqueue_Automated_BufferedSteerKeepsMarker(t *testing.T) {
	t.Parallel()
	a := newTestAgent(t)
	a.SetInboxSteerMode(true)
	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true)

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "auto", Steer: platform.SteerAlways, Automated: true})
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "human"})

	orphans := inb.drainSteer()
	if len(orphans) != 2 {
		t.Fatalf("steer buffer = %+v, want both messages", orphans)
	}
	followUp := buildFollowUp(Envelope{SessionKey: "test/s"}, orphans, nil)
	if !followUp[0].Automated || followUp[1].Automated {
		t.Errorf("follow-up Automated = [%v %v], want [true false]", followUp[0].Automated, followUp[1].Automated)
	}
	// The seed's marker never leaks onto a human orphan.
	followUp = buildFollowUp(Envelope{SessionKey: "test/s", Automated: true}, orphans[1:], nil)
	if followUp[0].Automated {
		t.Error("a human orphan inherited the seed's Automated marker")
	}
}

// TestInbox_Relogin_QueuedNotCapturedAsCode: an explicitly-queued message
// (SteerNever, e.g. foci send --human --queue) is a turn, never the login
// code — the window stays open and the message is held.
func TestInbox_Relogin_QueuedNotCapturedAsCode(t *testing.T) {
	a := newTestAgent(t)
	a.AgentID = "clutch"
	a.DelegatedManager = &DelegatedManager{}

	claimRelogin(t)
	relogin.G.OpenCapture("clutch")
	if !a.Enqueue(Envelope{SessionKey: "test/s", Text: "queued turn", Steer: platform.SteerNever}) {
		t.Fatal("Enqueue rejected a queued message during re-login; it must be held")
	}
	if !relogin.G.ShouldCapture("clutch") {
		t.Fatal("a SteerNever message consumed the capture window; it must stay open for the code")
	}
	if inb := a.lookupInbox("test/s"); inb == nil || len(inb.ch) != 1 {
		t.Fatal("queued message was not held on the session channel")
	}
}
