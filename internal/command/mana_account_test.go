package command

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/relogin"
	"foci/internal/usagehold"
)

// The #3104 phase-3 /usage tests: /usage and /mana in an agent's chat read
// that agent's own Claude Code account (its env), name it, and lift only it.

// accountCCAgent is a Claude Code agent on usage account acct, named name.
func accountCCAgent(reg *usagehold.Registry, id, acct, name string) *agent.Agent {
	a := &agent.Agent{AgentID: id, UsageHoldRegistry: reg, UsageAccountKey: acct}
	a.DelegatedManager = &agent.DelegatedManager{BackendType: "claude-code"}
	g := &relogin.Gate{}
	g.SetName(name)
	a.LoginGate = g
	return a
}

// TestManaCommand_PerAccount (H3, requirement 3): the read goes to the
// calling agent's account (its env), the reply names that account, and a
// read showing headroom lifts only that account.
func TestManaCommand_PerAccount(t *testing.T) {
	const ccB = "/tmp/ccB"
	usage := map[string]int{"": 10, ccB: 99} // used % by CLAUDE_CONFIG_DIR
	var asked []delegator.UsageTarget
	read := func(_ context.Context, tg delegator.UsageTarget) (*delegator.UsageInfo, error) {
		asked = append(asked, tg)
		pct := usage[tg.Env["CLAUDE_CONFIG_DIR"]]
		return &delegator.UsageInfo{
			Backend:             "Claude Code",
			RateLimitsAvailable: true,
			FiveHour:            delegator.UsageWindow{Percent: pct, Reported: true},
		}, nil
	}
	reg := usagehold.New()
	alpha := accountCCAgent(reg, "alpha", "claude-code:file:/host/.claude/.credentials.json", "/host/.claude")
	beta := accountCCAgent(reg, "beta", "claude-code:file:/tmp/ccB/.credentials.json", ccB)
	alphaMana := ManaCommand(read, delegator.UsageTarget{AgentID: "alpha"}, true)
	betaMana := ManaCommand(read, delegator.UsageTarget{AgentID: "beta", Env: map[string]string{"CLAUDE_CONFIG_DIR": ccB}, Binary: "/x/my-claude"}, true)
	run := func(c *Command, a *agent.Agent) string {
		t.Helper()
		resp, err := c.Execute(context.Background(), Request{}, CommandContext{Agent: a})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Text
	}

	got := run(betaMana, beta)
	if !strings.Contains(got, "99%") || !strings.Contains(got, "Account: "+ccB) {
		t.Errorf("/usage in beta's chat = %q, want 99%% and B named", got)
	}
	if last := asked[len(asked)-1]; last.AgentID != "beta" || last.Binary != "/x/my-claude" || last.Env["CLAUDE_CONFIG_DIR"] != ccB {
		t.Errorf("beta's read target = %+v, want beta's env and binary", last)
	}
	got = run(alphaMana, alpha)
	if !strings.Contains(got, "10%") || !strings.Contains(got, "Account: /host/.claude") {
		t.Errorf("/usage in alpha's chat = %q, want 10%% and A named", got)
	}

	// B held: alpha's read (headroom on A) does not lift B.
	beta.ObserveUsage(usagehold.Event{Window: "five_hour", Util: 0.99, HasUtil: true, ResetAt: time.Now().Add(2 * time.Hour)})
	run(alphaMana, alpha)
	if beta.UsageHold().Phase != usagehold.Hold {
		t.Errorf("B after /usage in alpha's chat: %v, want still held", beta.UsageHold().Phase)
	}
	// B's read still at 99%: nothing lifts.
	run(betaMana, beta)
	if beta.UsageHold().Phase != usagehold.Hold {
		t.Errorf("B after its own 99%% read: %v, want still held", beta.UsageHold().Phase)
	}
	// B reports headroom: /usage in beta's chat lifts B.
	usage[ccB] = 10
	if got := run(betaMana, beta); !strings.Contains(got, "Usage is available") {
		t.Errorf("/usage in beta's chat at 10%% = %q, want the lift", got)
	}
	if beta.UsageHold().Phase != usagehold.Open {
		t.Errorf("B after its own 10%% read: %v, want open", beta.UsageHold().Phase)
	}
}
