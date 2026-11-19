package agent

import (
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/ratelimit"
	"foci/internal/relogin"
	"foci/internal/usagehold"
)

// The #3104 phase-3 agent tests: a Claude Code agent's sessions are on its
// login's account (the gateway builds it from Spec.Account). alpha is on
// account A; beta and beta2 are on account B.

const (
	acctA = "claude-code:file:/host/.claude/.credentials.json"
	acctB = "claude-code:file:/tmp/ccB/.credentials.json"
)

// accountAgent is a Claude Code agent on usage account acct.
func accountAgent(reg *usagehold.Registry, id, acct string) *Agent {
	a := holdAgent(reg, "claude-code")
	a.AgentID = id
	putOnAccount(a, acct, reg, &relogin.Gate{})
	return a
}

func fiveHour(u float64) usagehold.Event {
	return usagehold.Event{Window: "five_hour", Util: u, HasUtil: true, ResetAt: time.Now().Add(2 * time.Hour)}
}

// TestUsageHold_PerAccount (H1, requirement 1): a usage event from beta (B)
// at 99% holds beta and beta2 and leaves alpha (A) open; a rejected window
// from beta stops B, and alpha still runs, keepalives included.
func TestUsageHold_PerAccount(t *testing.T) {
	reg := usagehold.New()
	alpha, beta, beta2 := accountAgent(reg, "alpha", acctA), accountAgent(reg, "beta", acctB), accountAgent(reg, "beta2", acctB)

	testAcct(beta).ObserveUsage(fiveHour(0.99))
	if beta.UsageHold("").Phase != usagehold.Hold || beta2.UsageHold("").Phase != usagehold.Hold {
		t.Errorf("B after beta's 99%% event: beta %v, beta2 %v; want both held", beta.UsageHold("").Phase, beta2.UsageHold("").Phase)
	}
	if alpha.UsageHold("").Phase != usagehold.Open || alpha.usageHoldsTrigger("", "user", false) {
		t.Errorf("alpha (A) phase %v after B's event; want open and its messages running", alpha.UsageHold("").Phase)
	}

	testAcct(beta).ObserveUsage(usagehold.Event{Window: "five_hour", Reached: true, ResetAt: time.Now().Add(2 * time.Hour)})
	if beta2.UsageHold("").Phase != usagehold.Stop {
		t.Errorf("beta2 after B's rejected window: %v, want stop", beta2.UsageHold("").Phase)
	}
	if blocked, why := alpha.UsageHoldBlocks("", true); blocked {
		t.Errorf("alpha's keepalive blocked by B's stop: %s", why)
	}
	if blocked, why := alpha.UsageHoldBlocks("", false); blocked {
		t.Errorf("alpha's periodic work blocked by B's stop: %s", why)
	}
	if testAcct(alpha).Key() == testAcct(beta).Key() || testAcct(beta).Key() != testAcct(beta2).Key() {
		t.Errorf("accounts alpha %q beta %q beta2 %q", testAcct(alpha).Key(), testAcct(beta).Key(), testAcct(beta2).Key())
	}
}

// startedAccountAgent is a running Claude Code agent on usage account acct
// with one turn that has started and is asleep in a rate-limit retry.
func startedAccountAgent(t *testing.T, reg *usagehold.Registry, acct, sk string) (*Agent, *blockingDriver) {
	t.Helper()
	a, cancel := startedAgent(t)
	t.Cleanup(cancel)
	a.DelegatedManager = &DelegatedManager{BackendType: "claude-code"}
	putOnAccount(a, acct, reg, &relogin.Gate{})
	a.StartUsageHold()
	t.Cleanup(a.StopUsageHold)
	d := newBlockingDriver()
	a.SetTurnObserver(d.record)
	a.UsageHeldDriver = func(string) Driver { return d }
	a.Enqueue(Envelope{SessionKey: sk, Text: "M0", Driver: d})
	select {
	case <-d.started:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s's turn never started", sk)
	}
	a.noteBackendRetry(sk, rateLimitRetry(2*time.Hour))
	return a, d
}

// TestResetLimits_PerAccount (H2, requirement 2): /limit_reset in beta's
// chat wakes beta2's turn asleep in a rate-limit retry (B) and not alpha's
// (A). Then, with A and B both held, it lifts B for beta and beta2, while A
// stays held and alpha's rate-limit gate stays closed.
func TestResetLimits_PerAccount(t *testing.T) {
	reg := usagehold.New()
	alpha, alphaTurn := startedAccountAgent(t, reg, acctA, "alpha/c1")
	beta2, beta2Turn := startedAccountAgent(t, reg, acctB, "beta2/c1")
	beta := accountAgent(reg, "beta", acctB)
	beta.StartUsageHold()
	defer beta.StopUsageHold()

	beta.ResetLimits("", "/limit_reset")
	select {
	case <-beta2Turn.ended:
	case <-time.After(2 * time.Second):
		t.Error("beta2's turn asleep in a rate-limit retry was not woken by beta's reset")
	}
	select {
	case <-alphaTurn.ended:
		t.Error("alpha's turn (account A) was woken by beta's reset")
	case <-time.After(300 * time.Millisecond):
	}

	alphaGate := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	alpha.EngageRateLimit(testAcct(alpha), ratelimit.Signal{Kind: ratelimit.KindUsage, ResetAt: alphaGate})
	testAcct(alpha).ObserveUsage(fiveHour(0.99))
	testAcct(beta).ObserveUsage(fiveHour(0.99))
	if alpha.UsageHold("").Phase != usagehold.Hold || beta2.UsageHold("").Phase != usagehold.Hold {
		t.Fatalf("before the reset: alpha %v, beta2 %v; want both held", alpha.UsageHold("").Phase, beta2.UsageHold("").Phase)
	}

	res := beta.ResetLimits("", "/limit_reset")
	if res.HoldFrom != usagehold.Hold {
		t.Errorf("beta's reset: hold was %v, want hold", res.HoldFrom)
	}
	if beta.UsageHold("").Phase != usagehold.Open || beta2.UsageHold("").Phase != usagehold.Open {
		t.Errorf("B after beta's reset: beta %v, beta2 %v; want open", beta.UsageHold("").Phase, beta2.UsageHold("").Phase)
	}
	if alpha.UsageHold("").Phase != usagehold.Hold {
		t.Errorf("A after beta's reset: %v, want still held", alpha.UsageHold("").Phase)
	}
	if lim, _ := alpha.rateLimitGate(alpha.Endpoint, testAcct(alpha)).IsLimited(); !lim {
		t.Error("alpha's rate-limit gate was opened by beta's reset")
	}
}

// TestUsageHold_NoticesNameAccount (H7, requirement 5): the hold notice and
// the "held" reply in beta's chat name B's account.
func TestUsageHold_NoticesNameAccount(t *testing.T) {
	gate := &relogin.Gate{}
	gate.SetName("/tmp/ccB")
	beta, cancel := startedAgent(t)
	defer cancel()
	beta.DelegatedManager = &DelegatedManager{BackendType: "claude-code"}
	putOnAccount(beta, acctB, usagehold.New(), gate)
	var mu sync.Mutex
	var notices, held []string
	beta.UsageHoldNotifyFunc = func(_, text string) { mu.Lock(); notices = append(notices, text); mu.Unlock() }
	beta.ChatSessionsFunc = func() []string { return []string{"beta/c1"} }
	beta.TurnQueuedNotifyFunc = func(_, text string) { mu.Lock(); held = append(held, text); mu.Unlock() }
	beta.StartUsageHold()
	defer beta.StopUsageHold()
	d := &recordingDriver{}
	beta.SetTurnObserver(d.recordBatch)

	testAcct(beta).ObserveUsage(fiveHour(0.99))
	beta.Enqueue(Envelope{SessionKey: "beta/c1", Text: "M1", Driver: d})
	ok := waitFor(2*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(notices) == 1 && len(held) == 1 })
	mu.Lock()
	defer mu.Unlock()
	if !ok {
		t.Fatalf("notices %q, held replies %q; want one of each", notices, held)
	}
	if !strings.Contains(notices[0], "Usage hold") || !strings.Contains(notices[0], "(account: /tmp/ccB)") {
		t.Errorf("hold notice %q does not name B's account", notices[0])
	}
	if !strings.Contains(held[0], "Held:") || !strings.Contains(held[0], "(account: /tmp/ccB)") {
		t.Errorf("held reply %q does not name B's account", held[0])
	}
	gate.SetEmail("b@example.com")
	if got := testAcct(beta).Name(); got != "b@example.com" {
		t.Errorf("name once the email is known = %q, want the email (Q15)", got)
	}
}

// TestUsageAccount_CodexUnchanged (H9): a codex agent is on the backend-wide
// account, unnamed, as before.
func TestUsageAccount_CodexUnchanged(t *testing.T) {
	x := holdAgent(usagehold.New(), "codex")
	if testAcct(x).Key() != "codex" || testAcct(x).Name() != "" {
		t.Errorf("codex account %q name %q, want \"codex\" and none", testAcct(x).Key(), testAcct(x).Name())
	}
}
