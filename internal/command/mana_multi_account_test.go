package command

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/account"
	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/relogin"
	"foci/internal/usagehold"
)

// TestManaAndLimitReset_ThisChatsAccount (#3186 F8, RULINGS 4 Q5: "Only this
// chat's account"): in an agent with two accounts, /usage in a chat reads,
// names and lifts only the account of that chat's session, and /limit_reset
// lifts only it.
func TestManaAndLimitReset_ThisChatsAccount(t *testing.T) {
	reg := usagehold.New()
	mk := func(key, dir string) *account.Account {
		g := &relogin.Gate{}
		return account.New(account.Options{Backend: "claude-code", Holds: reg, Login: g, Spec: delegator.AccountSpec{
			Key: key, Name: dir, Dir: dir, Env: map[string]string{"CLAUDE_CONFIG_DIR": dir}}})
	}
	accA, accB := mk("A", "/cc/a"), mk("B", "/cc/b")
	a := &agent.Agent{AgentID: "multi"}
	a.DelegatedManager = &agent.DelegatedManager{BackendType: "claude-code", Accounts: account.NewPool(nil, accA, accB)}
	const onA, onB = "multi/c1", "multi/c2"
	a.AccountFor(onA) // a new session takes the pick: A (first, both unused)
	accA.ObserveUsage(usagehold.Event{Window: "five_hour", Util: 0.99, HasUtil: true, ResetAt: time.Now().Add(time.Hour)})
	if a.AccountFor(onB) != accB { // A holds: the next new session takes B
		t.Fatal("setup: second chat not on B")
	}
	accB.ObserveUsage(usagehold.Event{Window: "five_hour", Util: 0.99, HasUtil: true, ResetAt: time.Now().Add(time.Hour)})

	usage := map[string]int{"/cc/a": 99, "/cc/b": 10}
	read := func(_ context.Context, tg delegator.UsageTarget) (*delegator.UsageInfo, error) {
		return &delegator.UsageInfo{Backend: "Claude Code", RateLimitsAvailable: true,
			FiveHour: delegator.UsageWindow{Percent: usage[tg.Env["CLAUDE_CONFIG_DIR"]], Reported: true}}, nil
	}
	mana := ManaCommand(read, func(cc CommandContext, sk string) delegator.UsageTarget {
		return delegator.UsageTarget{AgentID: "multi", Env: cc.Agent.AccountFor(sk).LaunchEnv(nil)}
	}, true)
	run := func(c *Command, sk string) string {
		t.Helper()
		resp, err := c.Execute(context.Background(), Request{SessionKey: sk}, CommandContext{Agent: a})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Text
	}

	if got := run(mana, onB); !strings.Contains(got, "10%") || !strings.Contains(got, "Account: /cc/b") {
		t.Errorf("/usage in B's chat = %q, want B's 10%% and B named", got)
	}
	if accB.Hold().Phase != usagehold.Open || accA.Hold().Phase != usagehold.Hold {
		t.Errorf("after /usage in B's chat: A %v, B %v; want only B lifted", accA.Hold().Phase, accB.Hold().Phase)
	}
	if got := run(mana, onA); !strings.Contains(got, "99%") || !strings.Contains(got, "Account: /cc/a") {
		t.Errorf("/usage in A's chat = %q, want A's 99%% and A named", got)
	}
	accB.ObserveUsage(usagehold.Event{Window: "five_hour", Util: 0.99, HasUtil: true, ResetAt: time.Now().Add(time.Hour)})
	run(LimitResetCommand(), onA)
	if accA.Hold().Phase != usagehold.Open || accB.Hold().Phase != usagehold.Hold {
		t.Errorf("after /limit_reset in A's chat: A %v, B %v; want only A lifted", accA.Hold().Phase, accB.Hold().Phase)
	}
}
