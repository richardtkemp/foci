package command

import (
	"context"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/tools"
)

// TestPlanCommandMetadata verifies PlanCommand returns a command with the
// expected name, description, and category regardless of mode.
func TestPlanCommandMetadata(t *testing.T) {
	for _, mode := range []PlanMode{PlanNativeSlash, PlanEnterTool} {
		cmd := PlanCommand(mode)
		if cmd.Name != "plan" {
			t.Errorf("mode %d: Name = %q, want %q", mode, cmd.Name, "plan")
		}
		if cmd.Description == "" {
			t.Errorf("mode %d: Description should not be empty", mode)
		}
		if cmd.Category != "operations" {
			t.Errorf("mode %d: Category = %q, want %q", mode, cmd.Category, "operations")
		}
	}
}

// TestPlanExecuteNoDelegatedManager verifies /plan errors for API-mode agents
// (no delegated backend), in either mode.
func TestPlanExecuteNoDelegatedManager(t *testing.T) {
	for _, mode := range []PlanMode{PlanNativeSlash, PlanEnterTool} {
		cmd := PlanCommand(mode)
		cc := CommandContext{Agent: &agent.Agent{}}
		_, err := cmd.Execute(context.Background(), Request{Args: "do a thing"}, cc)
		if err == nil {
			t.Fatalf("mode %d: expected error for nil DelegatedManager", mode)
		}
		if !strings.Contains(err.Error(), "delegated") {
			t.Errorf("mode %d: error = %q, want mention of 'delegated'", mode, err)
		}
	}
}

// TestPlanExecuteNoArgs verifies /plan with empty args returns a usage error.
func TestPlanExecuteNoArgs(t *testing.T) {
	cmd := PlanCommand(PlanEnterTool)
	cc := CommandContext{Agent: &agent.Agent{DelegatedManager: &agent.DelegatedManager{}}}
	_, err := cmd.Execute(context.Background(), Request{Args: ""}, cc)
	if err == nil {
		t.Fatal("expected error for empty args")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error = %q, want mention of 'usage'", err)
	}
}

// TestPlanExecuteNativeSlash_ForwardsVerbatim verifies the cctmux path forwards
// "/plan <args>" verbatim to the backend as a SourcePass slash command.
func TestPlanExecuteNativeSlash_ForwardsVerbatim(t *testing.T) {
	cmd := PlanCommand(PlanNativeSlash)

	mb := &mockPassBackendNoCapturer{}
	dm := &agent.DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) { return mb, nil },
	}
	cc := CommandContext{Agent: &agent.Agent{DelegatedManager: dm}}

	ctx := tools.WithSessionKey(context.Background(), "agent:test:main")
	if _, err := dm.Get(ctx, "agent:test:main"); err != nil { // pre-seed backend
		t.Fatalf("seeding backend: %v", err)
	}

	resp, err := cmd.Execute(ctx, Request{Args: "add caching to the API"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if mb.sentCommand != "/plan add caching to the API" {
		t.Errorf("sentCommand = %q, want %q", mb.sentCommand, "/plan add caching to the API")
	}
	if !strings.Contains(resp.Text, "Sent to CC") {
		t.Errorf("response = %q, want 'Sent to CC' confirmation", resp.Text)
	}
}

// TestPlanExecuteEnterTool_InjectsEnterPlanModePrompt verifies the ccstream path
// injects a fresh turn whose prompt instructs CC to invoke EnterPlanMode for the
// user's request, via AsyncNotifier.
func TestPlanExecuteEnterTool_InjectsEnterPlanModePrompt(t *testing.T) {
	cmd := PlanCommand(PlanEnterTool)

	var gotSession, gotMessage, gotTrigger string
	notifier := tools.NewAsyncNotifier(func(targetSession, message, _ /*replyTo*/, trigger string) {
		gotSession, gotMessage, gotTrigger = targetSession, message, trigger
	})
	cc := CommandContext{Agent: &agent.Agent{
		DelegatedManager: &agent.DelegatedManager{},
		AsyncNotifier:    notifier,
	}}

	ctx := tools.WithSessionKey(context.Background(), "agent:test:main")
	resp, err := cmd.Execute(ctx, Request{Args: "design the cache layer"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	wantMsg := "please invoke EnterPlanMode tool for:\ndesign the cache layer"
	if gotMessage != wantMsg {
		t.Errorf("injected message = %q, want %q", gotMessage, wantMsg)
	}
	if gotSession != "agent:test:main" {
		t.Errorf("injected session = %q, want %q", gotSession, "agent:test:main")
	}
	if gotTrigger != "plan-command" {
		t.Errorf("trigger = %q, want %q", gotTrigger, "plan-command")
	}
	if resp.Text == "" {
		t.Error("expected a confirmation response")
	}
}

// TestPlanExecuteEnterTool_NoNotifier verifies the ccstream path errors cleanly
// when async injection isn't configured (rather than silently no-op'ing).
func TestPlanExecuteEnterTool_NoNotifier(t *testing.T) {
	cmd := PlanCommand(PlanEnterTool)
	cc := CommandContext{Agent: &agent.Agent{DelegatedManager: &agent.DelegatedManager{}}}

	ctx := tools.WithSessionKey(context.Background(), "agent:test:main")
	_, err := cmd.Execute(ctx, Request{Args: "x"}, cc)
	if err == nil {
		t.Fatal("expected error when AsyncNotifier is nil")
	}
	if !strings.Contains(err.Error(), "async injection") {
		t.Errorf("error = %q, want mention of 'async injection'", err)
	}
}
