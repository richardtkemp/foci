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

// The #3297 tests: a spawn as another agent (#2662) whose target has two
// accounts, T1 and T2. Its usage-hold gate, its reserve check and its run
// all use the target pool's pick, and its session is bound to it (Dick
// 2026-11-28 19:18: "Pick and bind"). The target's cross-agent root is stored
// on one account; the spawn runs on a session of its own under that root,
// so the root's account is not the one that counts. The caller's account
// plays no part.

// crossAgentPick is crossAgentPickSetup's result: a caller agent on account C (its session
// callerSession) and a target "tgt2" on accounts T1 and T2, its cross-agent
// root stored on rootOn. Each spawn tool's allowance reader is the production
// one (tools.AccountUsageReader) over a reader per account, reporting 95%
// used of the 5-hour window for an account in low and 10% otherwise; the
// caller keeps a 20% reserve. spawn starts a spawn as tgt2 from
// callerSession (background: a background character spawn); runner records
// the batches tgt2 runs, bg the branches it makes.
type crossAgentPick struct {
	tgt          *Agent
	accC, t1, t2 *account.Account
	low          map[*account.Account]bool
	mu           sync.Mutex
	runner       *accountBatchRunner
	bg           *branchRecordingAgent
	spawn        func(background bool) (string, error)
}

func crossAgentPickSetup(t *testing.T, rootOn string) *crossAgentPick {
	t.Helper()
	const (
		callerSession = "caller3297/c1"
		target        = "tgt2"
	)
	x := &crossAgentPick{low: map[*account.Account]bool{}}
	reg := usagehold.New()
	mkAcct := func(key string) *account.Account {
		return account.New(account.Options{Backend: "claude-code", Holds: reg, Login: &relogin.Gate{},
			Spec: delegator.AccountSpec{Key: key, Name: "/cc/" + key}})
	}
	x.accC, x.t1, x.t2 = mkAcct("C"), mkAcct("T1"), mkAcct("T2")
	caller := holdAgent(reg, "claude-code")
	caller.AgentID = "caller3297"
	caller.DelegatedManager.Accounts = account.NewPool(&bindingMap{m: map[string]string{
		callerSession + "\x00" + session.MetaKeyAccount: "C",
	}}, x.accC).ForAgent("caller3297")
	x.tgt = holdAgent(reg, "claude-code")
	x.tgt.AgentID = target
	root := session.CrossAgentSpawnRoot(target)
	x.tgt.DelegatedManager.Accounts = account.NewPool(&bindingMap{m: map[string]string{
		root + "\x00" + session.MetaKeyAccount: rootOn,
	}}, x.t1, x.t2).ForAgent(target)

	reader := func(acct *account.Account) tools.UsageReader {
		return func(context.Context) tools.UsageRead {
			x.mu.Lock()
			pct := 10
			if x.low[acct] {
				pct = 95
			}
			x.mu.Unlock()
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
	x.runner = &accountBatchRunner{a: x.tgt}
	x.bg = &branchRecordingAgent{Agent: x.tgt}
	branches := 0
	tsp := tools.NewSpawner(tools.SpawnDeps{
		AgentID:        target,
		MaxInherit:     2,
		Tracker:        tools.NewSpawnTracker(),
		Notifier:       notifier,
		ReservePercent: func() int { return 20 },
		Delegated:      delegated(x.runner),
		CharacterBranch: func(_ context.Context, parent, _ string) (string, error) {
			branches++
			if x.bg.beforeBranch != nil {
				x.bg.beforeBranch()
			}
			bk := parent + "/b17095968" + string(rune('0'+branches))
			_ = x.tgt.AccountFor(bk) // an early question binds it to the pick
			x.bg.branch = bk
			return bk, nil
		},
		Usage: tools.AccountUsageReader(x.tgt.AccountFor, map[*account.Account]tools.UsageReader{
			x.t1: reader(x.t1), x.t2: reader(x.t2)}),
	}, func() tools.SpawnAgent { return x.bg })
	tools.RegisterSpawner(target, tsp)
	t.Cleanup(func() { tools.UnregisterSpawner(target) })
	csp := tools.NewSpawner(tools.SpawnDeps{
		AgentID:        "caller3297",
		MaxInherit:     2,
		Tracker:        tools.NewSpawnTracker(),
		Notifier:       notifier,
		ReservePercent: func() int { return 20 },
		Delegated:      delegated(&accountBatchRunner{a: caller}),
		Usage:          tools.AccountUsageReader(caller.AccountFor, map[*account.Account]tools.UsageReader{x.accC: reader(x.accC)}),
	}, func() tools.SpawnAgent { return &branchRecordingAgent{Agent: caller} })
	tool := csp.SpawnTool()
	n := 0
	x.spawn = func(background bool) (string, error) {
		n++
		args := map[string]any{"name": "y" + string(rune('a'+n)), "prompt": "task", "agent": target}
		if background {
			args["background"] = true
		}
		params, _ := json.Marshal(args)
		res, err := tool.Execute(tools.WithSessionKey(context.Background(), callerSession), params)
		return res.Text, err
	}
	return x
}

// TestCrossAgentSpawn_RootsAccountHeldOtherOpen_RunsOnOpen (#3297): the
// target's root is on T1, which holds; T2 is open (and the caller's own C
// holds). A direct spawn as the target starts and runs on T2; a background
// one starts and its branch is on T2.
func TestCrossAgentSpawn_RootsAccountHeldOtherOpen_RunsOnOpen(t *testing.T) {
	x := crossAgentPickSetup(t, "T1")
	holdAt(x.t1, time.Hour)
	holdAt(x.accC, time.Hour)
	if got := x.tgt.AccountFor(session.CrossAgentSpawnRoot("tgt2")); got != x.t1 {
		t.Fatalf("setup: the target's root is on %s, want T1", got.Key())
	}
	if _, err := x.spawn(false); err != nil {
		t.Fatalf("direct spawn as tgt2 with the root's T1 held and T2 open: %v, want started on T2", err)
	}
	if got := x.runner.ran(); len(got) != 1 || got[0] != x.t2 {
		t.Errorf("tgt2 batches ran on %v, want one on T2", keys(got))
	}

	x.bg.block = make(chan struct{})
	t.Cleanup(func() { close(x.bg.block) })
	if _, err := x.spawn(true); err != nil {
		t.Fatalf("background spawn as tgt2 with the root's T1 held and T2 open: %v, want started on T2", err)
	}
	if x.bg.branch == "" {
		t.Fatal("no branch made")
	}
	if got := x.tgt.AccountFor(x.bg.branch); got != x.t2 {
		t.Errorf("background spawn's branch on %s, want T2", got.Key())
	}
}

// TestCrossAgentSpawn_RunsOnTheAccountItsGateApproved (#3297): a spawn as
// the target runs on the account its gate approved (T2, the root's T1
// held), though before its session is made T2 holds too with a later lift,
// so a second pick would take T1.
func TestCrossAgentSpawn_RunsOnTheAccountItsGateApproved(t *testing.T) {
	x := crossAgentPickSetup(t, "T1")
	holdAt(x.t1, time.Hour)
	x.runner.before = func() { holdAt(x.t2, 3*time.Hour) }
	if _, err := x.spawn(false); err != nil {
		t.Fatalf("direct spawn as tgt2: %v, want started", err)
	}
	if got := x.tgt.Accounts().Next(); got != x.t1 {
		t.Fatalf("setup: a second pick now takes %s, want T1", got.Key())
	}
	if got := x.runner.ran(); len(got) != 1 || got[0] != x.t2 {
		t.Errorf("tgt2 batches ran on %v, want one on T2 (the account its gate approved)", keys(got))
	}
}

// TestCrossAgentSpawn_BackgroundRunsOnTheAccountItsGateApproved (#3297): a
// background spawn as the target: its branch is on the account its gate
// approved (T2, the root's T1 held), though as the branch is made T2 holds
// too with a later lift and the branch is asked about once (a pick then
// takes T1). It is still on T2.
func TestCrossAgentSpawn_BackgroundRunsOnTheAccountItsGateApproved(t *testing.T) {
	x := crossAgentPickSetup(t, "T1")
	holdAt(x.t1, time.Hour)
	x.bg.beforeBranch = func() { holdAt(x.t2, 3*time.Hour) }
	if _, err := x.spawn(true); err != nil {
		t.Fatalf("background spawn as tgt2: %v, want started", err)
	}
	if x.bg.branch == "" {
		t.Fatal("no branch made")
	}
	if got := x.tgt.Accounts().Next(); got != x.t1 {
		t.Fatalf("setup: a pick now takes %s, want T1", got.Key())
	}
	if got := x.tgt.AccountFor(x.bg.branch); got != x.t2 {
		t.Errorf("background spawn's branch on %s, want T2 (the account its gate approved)", got.Key())
	}
}

// TestCrossAgentSpawn_AllTargetAccountsHeld_RefusedNamingBoth (#3297, #3220
// problem 4): with T1 and T2 held (the caller's C open), a spawn as the
// target is refused as the target's hold, naming both accounts.
func TestCrossAgentSpawn_AllTargetAccountsHeld_RefusedNamingBoth(t *testing.T) {
	x := crossAgentPickSetup(t, "T1")
	holdAt(x.t1, time.Hour)
	holdAt(x.t2, 2*time.Hour)
	_, err := x.spawn(false)
	if err == nil {
		t.Fatal("spawn as tgt2 with both its accounts held: started, want refused")
	}
	msg := err.Error()
	for _, want := range []string{"NOT started", "tgt2", "its account's hold, not yours", "/cc/T1", "/cc/T2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal = %q, want it to contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "/cc/C") {
		t.Errorf("refusal = %q names the caller's account", msg)
	}
	if n := len(x.runner.ran()); n != 0 {
		t.Errorf("tgt2 batches run = %d, want none", n)
	}
}

// TestCrossAgentSpawn_ReserveReadsTheAccountItRunsOn (#3297): the target's
// root is on T2, both accounts open; the pick is T1 (neither has figures:
// the first wins). With T2 under the caller's reserve the spawn starts,
// checked, and runs on T1; with T1 under it the spawn is refused.
func TestCrossAgentSpawn_ReserveReadsTheAccountItRunsOn(t *testing.T) {
	x := crossAgentPickSetup(t, "T2")
	if got := x.tgt.Accounts().Next(); got != x.t1 {
		t.Fatalf("setup: the pick is %s, want T1", got.Key())
	}
	x.mu.Lock()
	x.low[x.t2] = true
	x.mu.Unlock()
	res, err := x.spawn(false)
	if err != nil {
		t.Fatalf("spawn as tgt2 with the root's T2 low and the pick T1 fine: %v, want started", err)
	}
	if strings.Contains(res, "not checked") {
		t.Errorf("result = %q: the allowance was not checked, want it checked against T1", res)
	}
	if got := x.runner.ran(); len(got) != 1 || got[0] != x.t1 {
		t.Errorf("tgt2 batches ran on %v, want one on T1", keys(got))
	}

	x.mu.Lock()
	x.low[x.t2], x.low[x.t1] = false, true
	x.mu.Unlock()
	res, err = x.spawn(false)
	if err == nil {
		t.Fatalf("spawn as tgt2 with the pick T1 low: started (%q), want refused", res)
	}
	if msg := err.Error(); !strings.Contains(msg, "allowance is running low") || !strings.Contains(msg, "tgt2") {
		t.Errorf("refusal = %q, want tgt2's allowance running low", msg)
	}
	if n := len(x.runner.ran()); n != 1 {
		t.Errorf("tgt2 batches run = %d, want still 1", n)
	}
}
