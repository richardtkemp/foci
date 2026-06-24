package cctmux

import (
	"context"
	"errors"
	"strings"
	"testing"

	"foci/internal/delegator"
)

// recordingDelegator is a partial Delegator that records the last Inject. It
// embeds the interface so it satisfies the full method set without boilerplate;
// only Inject is exercised here (any other call would panic on the nil
// embedded interface, which is the intended guard).
type recordingDelegator struct {
	delegator.Delegator
	inj delegator.Inject
}

func (r *recordingDelegator) Inject(_ context.Context, inj delegator.Inject) error {
	r.inj = inj
	return nil
}

// TestPlanDeliveryForwardsVerbatim verifies the cctmux delivery forwards
// "/plan <args>" verbatim as a SourcePass slash command.
func TestPlanDeliveryForwardsVerbatim(t *testing.T) {
	rd := &recordingDelegator{}
	deps := delegator.PlanDeps{
		SessionKey: "agent:test:main",
		Backend:    func() (delegator.Delegator, error) { return rd, nil },
	}

	resp, err := planDelivery(context.Background(), deps, "add caching to the API")
	if err != nil {
		t.Fatalf("planDelivery: %v", err)
	}
	if rd.inj.Source != delegator.SourcePass {
		t.Errorf("Inject.Source = %v, want SourcePass", rd.inj.Source)
	}
	if rd.inj.Text != "/plan add caching to the API" {
		t.Errorf("Inject.Text = %q, want %q", rd.inj.Text, "/plan add caching to the API")
	}
	if !strings.Contains(resp, "Sent to CC") {
		t.Errorf("response = %q, want 'Sent to CC' confirmation", resp)
	}
}

// TestPlanDeliveryBackendError verifies a backend-fetch failure surfaces.
func TestPlanDeliveryBackendError(t *testing.T) {
	deps := delegator.PlanDeps{
		SessionKey: "agent:test:main",
		Backend:    func() (delegator.Delegator, error) { return nil, errors.New("no backend") },
	}
	_, err := planDelivery(context.Background(), deps, "x")
	if err == nil {
		t.Fatal("expected error when Backend() fails")
	}
	if !strings.Contains(err.Error(), "get backend") {
		t.Errorf("error = %q, want mention of 'get backend'", err)
	}
}
