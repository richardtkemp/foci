package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/relogin"
	"foci/internal/usagehold"
)

// holdAgent is a delegated agent of backend on the given registry.
func holdAgent(reg *usagehold.Registry, backend string) *Agent {
	a := &Agent{AgentID: backend + "-agent", Endpoint: "ep", UsageHoldRegistry: reg}
	a.DelegatedManager = &DelegatedManager{BackendType: backend}
	return a
}

func weekly(u float64) usagehold.Event {
	return usagehold.Event{Window: "seven_day", Util: u, HasUtil: true, ResetAt: time.Now().Add(12 * time.Hour)}
}

// TestUsageHold_AccountWide is requirement 1 and worked example G: a 0.98
// weekly event on one Claude Code agent holds every Claude Code agent, not a
// codex agent; a codex window at 98% holds codex only.
func TestUsageHold_AccountWide(t *testing.T) {
	reg := usagehold.New()
	a, b, x := holdAgent(reg, "claude-code"), holdAgent(reg, "claude-code"), holdAgent(reg, "codex")
	a.ObserveUsage(weekly(0.97))
	if b.UsageHold().Phase != usagehold.Open {
		t.Fatal("0.97 held the account")
	}
	a.ObserveUsage(weekly(0.98))
	if b.UsageHold().Phase != usagehold.Hold {
		t.Errorf("B phase = %v after A's 0.98 event, want hold", b.UsageHold().Phase)
	}
	if x.UsageHold().Phase != usagehold.Open {
		t.Errorf("codex phase = %v, want open", x.UsageHold().Phase)
	}
	x.ObserveUsage(usagehold.Event{Window: "codex codex primary", Util: 0.98, HasUtil: true, ResetAt: time.Now().Add(time.Hour)})
	if x.UsageHold().Phase != usagehold.Hold {
		t.Errorf("codex at 98%%: phase %v, want hold", x.UsageHold().Phase)
	}
	// An API agent has no account and is never held.
	if api := (&Agent{UsageHoldRegistry: reg}); api.UsageAccount() != "" || api.UsageHold().Phase != usagehold.Open {
		t.Errorf("API agent account = %q, want none", api.UsageAccount())
	}
}

// TestUsageHold_SchedulerGates: during a hold periodic work is blocked and
// keepalive is not; during a stop both are.
func TestUsageHold_SchedulerGates(t *testing.T) {
	reg := usagehold.New()
	a := holdAgent(reg, "claude-code")
	a.ObserveUsage(weekly(0.98))
	if lim, reason := a.SessionRateLimited("claude-code-agent/c1"); !lim || !strings.Contains(reason, "usage hold") {
		t.Errorf("SessionRateLimited during a hold = %v %q, want blocked by the usage hold", lim, reason)
	}
	if lim, _ := a.KeepaliveRateLimited("claude-code-agent/c1"); lim {
		t.Error("keepalive blocked during a hold, want it to run")
	}
	a.ObserveUsage(usagehold.Event{Window: "seven_day", Reached: true, ResetAt: time.Now().Add(time.Hour)})
	if lim, _ := a.KeepaliveRateLimited("claude-code-agent/c1"); !lim {
		t.Error("keepalive not blocked during a stop")
	}
	if msg := a.UsageHoldRefusal(); msg == "" {
		t.Error("no spawn refusal during a stop")
	}
}

// TestUsageHold_NoticePerAgent is requirement 7 and worked example I: each
// agent on the account posts one start and one end notice; the other account
// posts none; a second event in the same hold posts nothing.
func TestUsageHold_NoticePerAgent(t *testing.T) {
	reg := usagehold.New()
	var mu sync.Mutex
	notices := map[string][]string{}
	mk := func(backend, id string) *Agent {
		a := holdAgent(reg, backend)
		a.AgentID = id
		a.UsageHoldNotifyFunc = func(text string) { mu.Lock(); notices[id] = append(notices[id], text); mu.Unlock() }
		a.StartUsageHold()
		t.Cleanup(a.StopUsageHold)
		return a
	}
	a := mk("claude-code", "A")
	mk("claude-code", "B")
	mk("codex", "X")
	a.ObserveUsage(weekly(0.98))
	a.ObserveUsage(weekly(0.99))
	a.ObserveUsage(weekly(0.10))
	count := func(id string) int { mu.Lock(); defer mu.Unlock(); return len(notices[id]) }
	if !waitFor(time.Second, func() bool { return count("A") == 2 && count("B") == 2 }) {
		t.Fatalf("notices = %v, want 2 each for A and B", notices)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(notices["X"]) != 0 {
		t.Errorf("codex agent got notices %v", notices["X"])
	}
	if !strings.Contains(notices["A"][0], "7-day") || !strings.Contains(notices["A"][0], "98%") || !strings.Contains(notices["A"][1], "ended") {
		t.Errorf("A's notices = %q, want a 7-day 98%% start and an end", notices["A"])
	}
}

// TestInbox_UsageHold_HoldsAndFoldsUserMessages is requirements 2, 4 and 5:
// user messages during a hold run no turn, the sender is told, and at lift
// they run as ONE turn in arrival order.
func TestInbox_UsageHold_HoldsAndFoldsUserMessages(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	reg := usagehold.New()
	a.UsageHoldRegistry = reg
	a.DelegatedManager = &DelegatedManager{BackendType: "claude-code"}
	a.StartUsageHold()
	defer a.StopUsageHold()
	var told atomic.Int32
	a.TurnQueuedNotifyFunc = func(_, text string) {
		if strings.Contains(text, "Held") {
			told.Add(1)
		}
	}
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)

	a.ObserveUsage(weekly(0.98))
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "M1", Driver: d})
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "M2", Driver: d})
	if waitFor(300*time.Millisecond, func() bool { return d.NumCalls() > 0 }) {
		t.Fatalf("a held message ran a turn; calls=%d", d.NumCalls())
	}
	if !waitFor(time.Second, func() bool { return told.Load() == 2 }) {
		t.Errorf("sender told %d times, want once per held message", told.Load())
	}
	a.ObserveUsage(weekly(0.50))
	if !waitFor(2*time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("held messages did not run at lift; calls=%d", d.NumCalls())
	}
	got := d.Calls()[0]
	if len(got) != 2 || got[0].Text != "M1" || got[1].Text != "M2" {
		t.Errorf("lift turn = %+v, want M1 then M2 in one turn", got)
	}
}

// TestInbox_UsageHold_InjectionsWaitRemindRuns is requirements 2 and 6: a cron
// send waits during a hold while a foci_remind fire behind it runs; at a stop
// the remind waits too.
func TestInbox_UsageHold_InjectionsWaitRemindRuns(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	reg := usagehold.New()
	a.UsageHoldRegistry = reg
	a.DelegatedManager = &DelegatedManager{BackendType: "claude-code"}
	a.StartUsageHold()
	defer a.StopUsageHold()

	a.ObserveUsage(weekly(0.98))
	var cron, wake, ka atomic.Bool
	a.Enqueue(Envelope{SessionKey: "test/s", Inject: &InjectMeta{Trigger: "cron", Run: func() { cron.Store(true) }}})
	a.Enqueue(Envelope{SessionKey: "test/s", Inject: &InjectMeta{Trigger: "scheduled_wake", Run: func() { wake.Store(true) }}})
	a.Enqueue(Envelope{SessionKey: "test/s", Inject: &InjectMeta{Trigger: "keepalive", Run: func() { ka.Store(true) }}})
	if !waitFor(time.Second, func() bool { return wake.Load() && ka.Load() }) {
		t.Fatalf("during a hold: wake ran=%v keepalive ran=%v, want both", wake.Load(), ka.Load())
	}
	if cron.Load() {
		t.Fatal("a cron send ran during the hold")
	}

	a.ObserveUsage(usagehold.Event{Window: "seven_day", Reached: true, ResetAt: time.Now().Add(time.Hour)})
	var wake2 atomic.Bool
	a.Enqueue(Envelope{SessionKey: "test/s", Inject: &InjectMeta{Trigger: "scheduled_wake", Run: func() { wake2.Store(true) }}})
	if waitFor(300*time.Millisecond, wake2.Load) {
		t.Fatal("a remind fire ran during a stop")
	}
	a.ObserveUsage(usagehold.Event{Window: "seven_day", Util: 0.2, HasUtil: true})
	if !waitFor(3*time.Second, func() bool { return cron.Load() && wake2.Load() }) {
		t.Fatalf("at lift: cron ran=%v wake ran=%v, want both", cron.Load(), wake2.Load())
	}
}

// TestRelogin_PendingBlocksBackgroundWork (#3006): while a Claude Code login
// is pending, no periodic work runs on an agent that uses that login —
// keepalive included. Each such run 401'd and started a new login attempt
// (10 Nov: keepalive, reflection and two scheduled resets each did). A
// backend with no Claude Code login is not blocked.
func TestRelogin_PendingBlocksBackgroundWork(t *testing.T) {
	reg := usagehold.New()
	cc := holdAgent(reg, "claude-code")
	cc.ReloginTrigger = func(string, string) bool { return true }
	other := holdAgent(reg, "codex")
	sk := "claude-code-agent/c1"

	if lim, reason := cc.SessionRateLimited(sk); lim {
		t.Fatalf("blocked with no login pending: %q", reason)
	}
	if !relogin.G.Start() {
		t.Fatal("re-login gate already active at test start")
	}
	t.Cleanup(func() {
		relogin.G.Release()
		relogin.G.Complete()
	})
	relogin.G.Release() // the attempt failed: the login is still owed

	if lim, reason := cc.SessionRateLimited(sk); !lim || !strings.Contains(reason, "login") {
		t.Errorf("SessionRateLimited while a login is pending = %v %q, want blocked by the login", lim, reason)
	}
	if lim, reason := cc.KeepaliveRateLimited(sk); !lim || !strings.Contains(reason, "login") {
		t.Errorf("KeepaliveRateLimited while a login is pending = %v %q, want blocked by the login", lim, reason)
	}
	if ok, _ := cc.CanFireBackgroundOperation(context.Background(), sk); ok {
		t.Error("CanFireBackgroundOperation allowed background work while a login is pending")
	}
	// The credential is shared: one pending login blocks every CC agent.
	cc2 := &Agent{AgentID: "helen", Endpoint: "ep", UsageHoldRegistry: reg,
		DelegatedManager: &DelegatedManager{BackendType: "claude-code"},
		ReloginTrigger:   func(string, string) bool { return true }}
	if lim, _ := cc2.KeepaliveRateLimited("helen/c1"); !lim {
		t.Error("a second Claude Code agent ran background work while the shared login is pending")
	}
	if lim, reason := other.KeepaliveRateLimited("codex-agent/c1"); lim {
		t.Errorf("a backend without the Claude Code login was blocked: %q", reason)
	}

	relogin.G.Complete()
	if lim, reason := cc.KeepaliveRateLimited(sk); lim {
		t.Errorf("still blocked after the login completed: %q", reason)
	}
}
