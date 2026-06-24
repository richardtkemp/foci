package ccstream

import (
	"context"
	"strings"
	"testing"

	"foci/internal/delegator"
)

// recordingInjector captures InjectToAgent calls for assertion.
type recordingInjector struct {
	session, message, replyTo, trigger string
	calls                              int
}

func (r *recordingInjector) InjectToAgent(session, message, replyTo, trigger string) {
	r.calls++
	r.session, r.message, r.replyTo, r.trigger = session, message, replyTo, trigger
}

// TestPlanDeliveryInjectsEnterPlanMode verifies the ccstream delivery drives a
// fresh turn instructing CC to invoke EnterPlanMode for the user's request.
func TestPlanDeliveryInjectsEnterPlanMode(t *testing.T) {
	inj := &recordingInjector{}
	deps := delegator.PlanDeps{SessionKey: "agent:test:main", Notifier: inj}

	resp, err := planDelivery(context.Background(), deps, "design the cache layer")
	if err != nil {
		t.Fatalf("planDelivery: %v", err)
	}
	if inj.calls != 1 {
		t.Fatalf("InjectToAgent calls = %d, want 1", inj.calls)
	}
	wantMsg := "please invoke EnterPlanMode tool for:\ndesign the cache layer"
	if inj.message != wantMsg {
		t.Errorf("message = %q, want %q", inj.message, wantMsg)
	}
	if inj.session != "agent:test:main" {
		t.Errorf("session = %q, want %q", inj.session, "agent:test:main")
	}
	if inj.trigger != "plan-command" {
		t.Errorf("trigger = %q, want %q", inj.trigger, "plan-command")
	}
	if resp == "" {
		t.Error("expected a confirmation response")
	}
}

// TestPlanDeliveryNoNotifier verifies the ccstream delivery errors cleanly when
// no agent injector is wired, rather than silently no-op'ing.
func TestPlanDeliveryNoNotifier(t *testing.T) {
	_, err := planDelivery(context.Background(), delegator.PlanDeps{SessionKey: "x"}, "do a thing")
	if err == nil {
		t.Fatal("expected error when Notifier is nil")
	}
	if !strings.Contains(err.Error(), "async injection") {
		t.Errorf("error = %q, want mention of 'async injection'", err)
	}
}
