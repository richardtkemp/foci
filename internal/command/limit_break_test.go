package command

import (
	"context"
	"testing"

	"foci/internal/account/accounttest"
	"foci/internal/agent"
	"foci/internal/platform"
	"foci/internal/usagehold"
)

// TestLimitBreakCommand_BareForm (#3182): a bare /lb with no hold on answers
// the usage; with the hold on and nothing held it says so; with no agent it
// answers the usage. (The release itself is tested in internal/agent,
// usage_hold_force_bare_test.go.)
func TestLimitBreakCommand_BareForm(t *testing.T) {
	run := func(cc CommandContext) string {
		t.Helper()
		resp, err := LimitBreakCommand().Execute(context.Background(), Request{SessionKey: "cam/s"}, cc)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Text
	}
	if got := run(CommandContext{}); got != platform.LimitBreakUsage {
		t.Errorf("no agent: reply = %q, want the usage", got)
	}
	open := &agent.Agent{AgentID: "cam"}
	open.DelegatedManager = &agent.DelegatedManager{BackendType: "claude-code", Accounts: accounttest.Pool("claude-code", "", usagehold.New(), nil)}
	if got := run(CommandContext{Agent: open}); got != platform.LimitBreakUsage {
		t.Errorf("no hold: reply = %q, want the usage", got)
	}
	if got := run(CommandContext{Agent: stoppedCCAgent()}); got != platform.LimitBreakNothingHeld {
		t.Errorf("hold, nothing held: reply = %q, want %q", got, platform.LimitBreakNothingHeld)
	}
}
