package command

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/tools"
)

// okDelivery is a no-op PlanDelivery that records what the command passed it.
func okDelivery(rec *deliveryRecord) delegator.PlanDelivery {
	return func(_ context.Context, deps delegator.PlanDeps, args string) (string, error) {
		rec.called = true
		rec.args = args
		rec.sessionKey = deps.SessionKey
		rec.hasNotifier = deps.Notifier != nil
		rec.hasBackendThunk = deps.Backend != nil
		return "delivered: " + args, nil
	}
}

type deliveryRecord struct {
	called          bool
	args            string
	sessionKey      string
	hasNotifier     bool
	hasBackendThunk bool
}

// TestPlanCommandMetadata verifies PlanCommand returns a command with the
// expected name, description, and category.
func TestPlanCommandMetadata(t *testing.T) {
	cmd := PlanCommand(okDelivery(&deliveryRecord{}))
	if cmd.Name != "plan" {
		t.Errorf("Name = %q, want %q", cmd.Name, "plan")
	}
	if cmd.Description == "" {
		t.Error("Description should not be empty")
	}
	if cmd.Category != "operations" {
		t.Errorf("Category = %q, want %q", cmd.Category, "operations")
	}
}

// TestPlanExecuteNoDelegatedManager verifies /plan errors for API-mode agents
// (no delegated backend), before the delivery is ever consulted.
func TestPlanExecuteNoDelegatedManager(t *testing.T) {
	rec := &deliveryRecord{}
	cmd := PlanCommand(okDelivery(rec))
	cc := CommandContext{Agent: &agent.Agent{}}
	_, err := cmd.Execute(context.Background(), Request{Args: "do a thing"}, cc)
	if err == nil {
		t.Fatal("expected error for nil DelegatedManager")
	}
	if !strings.Contains(err.Error(), "delegated") {
		t.Errorf("error = %q, want mention of 'delegated'", err)
	}
	if rec.called {
		t.Error("delivery should not be called when DelegatedManager is nil")
	}
}

// TestPlanExecuteNoArgs verifies /plan with empty args returns a usage error.
func TestPlanExecuteNoArgs(t *testing.T) {
	cmd := PlanCommand(okDelivery(&deliveryRecord{}))
	cc := CommandContext{Agent: &agent.Agent{DelegatedManager: &agent.DelegatedManager{}}}
	_, err := cmd.Execute(context.Background(), Request{Args: ""}, cc)
	if err == nil {
		t.Fatal("expected error for empty args")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error = %q, want mention of 'usage'", err)
	}
}

// TestPlanExecuteNoSession verifies /plan errors when no session key can be
// resolved from context or request.
func TestPlanExecuteNoSession(t *testing.T) {
	cmd := PlanCommand(okDelivery(&deliveryRecord{}))
	cc := CommandContext{Agent: &agent.Agent{DelegatedManager: &agent.DelegatedManager{}}}
	_, err := cmd.Execute(context.Background(), Request{Args: "x"}, cc)
	if err == nil {
		t.Fatal("expected error when no session key is present")
	}
	if !strings.Contains(err.Error(), "session") {
		t.Errorf("error = %q, want mention of 'session'", err)
	}
}

// TestPlanExecuteInvokesDelivery verifies the command resolves the session,
// wires deps, and forwards args to the injected delivery, returning its text.
func TestPlanExecuteInvokesDelivery(t *testing.T) {
	rec := &deliveryRecord{}
	cmd := PlanCommand(okDelivery(rec))

	notifier := tools.NewAsyncNotifier(func(string, string, string, string) {})
	cc := CommandContext{Agent: &agent.Agent{
		DelegatedManager: &agent.DelegatedManager{},
		AsyncNotifier:    notifier,
	}}
	ctx := tools.WithSessionKey(context.Background(), "agent:test:main")

	resp, err := cmd.Execute(ctx, Request{Args: "design the cache layer"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !rec.called {
		t.Fatal("delivery was not called")
	}
	if rec.args != "design the cache layer" {
		t.Errorf("delivery args = %q, want %q", rec.args, "design the cache layer")
	}
	if rec.sessionKey != "agent:test:main" {
		t.Errorf("deps.SessionKey = %q, want %q", rec.sessionKey, "agent:test:main")
	}
	if !rec.hasNotifier {
		t.Error("deps.Notifier should be set when the agent has an AsyncNotifier")
	}
	if !rec.hasBackendThunk {
		t.Error("deps.Backend thunk should always be set")
	}
	if resp.Text != "delivered: design the cache layer" {
		t.Errorf("response = %q, want delivery's text", resp.Text)
	}
}

// TestPlanExecuteNoNotifierLeavesDepsNil verifies that an agent without an
// AsyncNotifier yields a nil deps.Notifier (not a non-nil interface wrapping a
// nil pointer), so a delivery's nil-guard works.
func TestPlanExecuteNoNotifierLeavesDepsNil(t *testing.T) {
	rec := &deliveryRecord{}
	cmd := PlanCommand(okDelivery(rec))
	cc := CommandContext{Agent: &agent.Agent{DelegatedManager: &agent.DelegatedManager{}}}
	ctx := tools.WithSessionKey(context.Background(), "agent:test:main")

	if _, err := cmd.Execute(ctx, Request{Args: "x"}, cc); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rec.hasNotifier {
		t.Error("deps.Notifier should be nil when the agent has no AsyncNotifier")
	}
}

// TestPlanExecutePropagatesDeliveryError verifies a delivery error surfaces from
// Execute unchanged.
func TestPlanExecutePropagatesDeliveryError(t *testing.T) {
	delivery := func(context.Context, delegator.PlanDeps, string) (string, error) {
		return "", fmt.Errorf("boom")
	}
	cmd := PlanCommand(delivery)
	cc := CommandContext{Agent: &agent.Agent{DelegatedManager: &agent.DelegatedManager{}}}
	ctx := tools.WithSessionKey(context.Background(), "agent:test:main")

	_, err := cmd.Execute(ctx, Request{Args: "x"}, cc)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want delivery error to propagate", err)
	}
}
