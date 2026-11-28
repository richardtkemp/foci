package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/account"
	"foci/internal/delegator"
	"foci/internal/relogin"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/usagehold"
)

// The #3296 tests: spawn_send and a cross-agent spawn's reserve check ask
// about the account the spawn runs on, never the caller's. Dick 2026-11-27
// 16:14: "It doesn't matter about the hold on the parent's account. If there
// is a valid target, spawns should be permitted".

// TestSpawnSend_GatedOnTheSpawnsAccount (#3296 F2): a background character
// spawn of the two-account agent runs on B (the spawner's account A held).
// With A held and B open, spawn_send reaches it; with A open and B held,
// spawn_send is refused, naming B.
func TestSpawnSend_GatedOnTheSpawnsAccount(t *testing.T) {
	a, accA, accB, _, spawn, bg, sp := spawnAccountSetupBG(t)
	bg.block = make(chan struct{})
	t.Cleanup(func() { close(bg.block) })
	holdAt(accA, time.Hour)
	if _, err := spawn("character-bg"); err != nil {
		t.Fatalf("background character spawn with A held and B open: %v", err)
	}
	if got := a.AccountFor(bg.branch); got != accB {
		t.Fatalf("setup: the spawn's branch is on %s, want B", got.Key())
	}
	if a.UsageHoldRefusal(multiOnA) == "" {
		t.Fatal("setup: the caller's account A is not held")
	}
	send := sp.SendTool()
	call := func() (string, error) {
		params, _ := json.Marshal(map[string]any{"id": bg.branch, "message": "one more thing"})
		res, err := send.Execute(tools.WithSessionKey(context.Background(), multiOnA), params)
		return res.Text, err
	}

	res, err := call()
	if err != nil {
		t.Fatalf("spawn_send with the caller's A held and the spawn's B open: %v, want delivered", err)
	}
	if !strings.Contains(res, "spawn") || strings.Contains(res, "NOT") {
		t.Errorf("spawn_send result = %q, want delivered or queued for the spawn", res)
	}

	// A lifts (a report of the same window under the threshold clears it);
	// B holds.
	accA.ObserveUsage(usagehold.Event{Window: "five_hour", Util: 0.5, HasUtil: true, ResetAt: time.Now().Add(time.Hour)})
	holdAt(accB, time.Hour)
	if a.UsageHoldRefusal(multiOnA) != "" {
		t.Fatal("setup: the caller's account A is still held")
	}
	_, err = call()
	if err == nil {
		t.Fatal("spawn_send with the spawn's account B held: delivered, want refused")
	}
	if msg := err.Error(); !strings.Contains(msg, "NOT delivered to spawn") || !strings.Contains(msg, "/cc/B") || strings.Contains(msg, "/cc/A") {
		t.Errorf("refusal = %q, want NOT delivered naming B only", msg)
	}
}

// crossAgentReserveSetup is two agents on accounts of their own: the caller
// "caller" (its session callerSession on account C) and the target "tgt"
// (account T). Each spawn tool's allowance reader is the production one
// (tools.AccountUsageReader) over a reader per account, which reports 95%
// used of the 5-hour window for an account in low and 10% otherwise. The
// caller keeps a 20% reserve. spawn starts a direct spawn as tgt from
// callerSession; runner records the batches tgt runs.
func crossAgentReserveSetup(t *testing.T) (accC, accT *account.Account, low map[*account.Account]bool, runner *accountBatchRunner, spawn func() (string, error)) {
	t.Helper()
	const callerSession = "caller/c1"
	reg := usagehold.New()
	mkAcct := func(key string) *account.Account {
		return account.New(account.Options{Backend: "claude-code", Holds: reg, Login: &relogin.Gate{},
			Spec: delegator.AccountSpec{Key: key, Name: "/cc/" + key}})
	}
	accC, accT = mkAcct("C"), mkAcct("T")
	caller := holdAgent(reg, "claude-code")
	caller.AgentID = "caller"
	caller.DelegatedManager.Accounts = account.NewPool(&bindingMap{m: map[string]string{
		callerSession + "\x00" + session.MetaKeyAccount: "C",
	}}, accC).ForAgent("caller")
	tgt := holdAgent(reg, "claude-code")
	tgt.AgentID = "tgt"
	tgt.DelegatedManager.Accounts = account.NewPool(&bindingMap{m: map[string]string{}}, accT).ForAgent("tgt")

	var mu sync.Mutex
	low = map[*account.Account]bool{}
	reader := func(acct *account.Account) tools.UsageReader {
		return func(context.Context) tools.UsageRead {
			mu.Lock()
			pct := 10
			if low[acct] {
				pct = 95
			}
			mu.Unlock()
			return tools.UsageRead{Info: &delegator.UsageInfo{Backend: "Claude Code",
				FiveHour: delegator.UsageWindow{Percent: pct, ResetsAt: time.Now().Add(time.Hour), Reported: true}}}
		}
	}
	delegated := func(r tools.BatchRunner) *tools.DelegatedSpawn {
		return &tools.DelegatedSpawn{
			Spec: delegator.Spec{Caps: map[delegator.Capability]delegator.Support{
				delegator.CapBranch:        delegator.Yes(),
				delegator.CapIsolatedBatch: delegator.Yes(),
				delegator.CapReadOnlyBatch: delegator.Yes(),
			}},
			Runner: func() tools.BatchRunner { return r },
		}
	}
	notifier := tools.NewAsyncNotifier(func(string, string, string, string, *tools.AgentOrigin) {})
	runner = &accountBatchRunner{a: tgt}
	tsp := tools.NewSpawner(tools.SpawnDeps{
		AgentID:        "tgt",
		MaxInherit:     2,
		Tracker:        tools.NewSpawnTracker(),
		Notifier:       notifier,
		ReservePercent: func() int { return 20 },
		Delegated:      delegated(runner),
		Usage:          tools.AccountUsageReader(tgt.AccountFor, map[*account.Account]tools.UsageReader{accT: reader(accT)}),
	}, func() tools.SpawnAgent { return &branchRecordingAgent{Agent: tgt} })
	tools.RegisterSpawner("tgt", tsp)
	t.Cleanup(func() { tools.UnregisterSpawner("tgt") })
	csp := tools.NewSpawner(tools.SpawnDeps{
		AgentID:        "caller",
		MaxInherit:     2,
		Tracker:        tools.NewSpawnTracker(),
		Notifier:       notifier,
		ReservePercent: func() int { return 20 },
		Delegated:      delegated(&accountBatchRunner{a: caller}),
		Usage:          tools.AccountUsageReader(caller.AccountFor, map[*account.Account]tools.UsageReader{accC: reader(accC)}),
	}, func() tools.SpawnAgent { return &branchRecordingAgent{Agent: caller} })
	tool := csp.SpawnTool()
	n := 0
	spawn = func() (string, error) {
		n++
		params, _ := json.Marshal(map[string]any{"name": "x" + string(rune('a'+n)), "prompt": "task", "agent": "tgt"})
		res, err := tool.Execute(tools.WithSessionKey(context.Background(), callerSession), params)
		return res.Text, err
	}
	return accC, accT, low, runner, spawn
}

// TestCrossAgentSpawn_TargetUnderReserve_Refused (#3296 F3): a spawn as tgt
// whose account T is under the caller's reserve is refused, though the
// caller's own account C is fine.
func TestCrossAgentSpawn_TargetUnderReserve_Refused(t *testing.T) {
	_, accT, low, runner, spawn := crossAgentReserveSetup(t)
	low[accT] = true
	res, err := spawn()
	if err == nil {
		t.Fatalf("spawn as tgt with T under the reserve: started (%q), want refused", res)
	}
	if msg := err.Error(); !strings.Contains(msg, "allowance is running low") || !strings.Contains(msg, "tgt") {
		t.Errorf("refusal = %q, want tgt's allowance running low", msg)
	}
	if n := len(runner.ran()); n != 0 {
		t.Errorf("tgt batches run = %d, want none", n)
	}
}

// TestCrossAgentSpawn_TargetFine_StartsCheckedAgainstTarget (#3296 F3): a
// spawn as tgt whose account T is fine starts, though the caller's own
// account C is under the reserve, and its allowance was checked (no "not
// checked" note).
func TestCrossAgentSpawn_TargetFine_StartsCheckedAgainstTarget(t *testing.T) {
	accC, accT, low, runner, spawn := crossAgentReserveSetup(t)
	low[accC] = true
	res, err := spawn()
	if err != nil {
		t.Fatalf("spawn as tgt with T fine and the caller's C low: %v, want started", err)
	}
	if strings.Contains(res, "not checked") {
		t.Errorf("result = %q: the allowance was not checked, want it checked against T", res)
	}
	if got := runner.ran(); len(got) != 1 || got[0] != accT {
		t.Errorf("tgt batches ran on %v, want one on T", keys(got))
	}
}
