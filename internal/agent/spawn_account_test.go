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
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/usagehold"
)

// The #3292 tests: ONE agent with two accounts, A and B (twoAccountAgent);
// the spawner's session multiOnA is stored on A. Dick 2026-11-27 16:14: "It
// doesn't matter about the hold on the parent's account. If there is a
// valid target, spawns should be permitted".

// accountBatchRunner is a batch runner that, like DelegatedManager.RunBatch,
// names the batch's own session (OnSession) and records the account that
// session is on. before runs ahead of OnSession, while the gate has already
// passed.
type accountBatchRunner struct {
	a      *Agent
	before func()

	mu    sync.Mutex
	accts []*account.Account
}

func (r *accountBatchRunner) RunBatch(_ context.Context, req delegator.BatchRequest) (string, error) {
	if r.before != nil {
		r.before()
	}
	key, _ := session.NewBatchKey(req.OwnerSessionKey, "multi")
	if req.OnSession != nil {
		req.OnSession(key)
	}
	r.mu.Lock()
	r.accts = append(r.accts, r.a.AccountFor(key))
	r.mu.Unlock()
	return "done", nil
}

func (r *accountBatchRunner) ran() []*account.Account {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*account.Account(nil), r.accts...)
}

// branchRecordingAgent is the agent with its turns replaced by no-ops, which
// records the background character branches its spawn tool makes.
type branchRecordingAgent struct {
	*Agent
	beforeBranch func() // runs as the branch is made, after the gate
	branch       string // the last branch made
	// block, when set, keeps each turn running until it is closed or the
	// turn's ctx ends (a running spawn, for spawn_send).
	block chan struct{}
}

func (b *branchRecordingAgent) HandleMessage(ctx context.Context, _ string, _ []string, _ []platform.Attachment) error {
	if b.block != nil {
		select {
		case <-b.block:
		case <-ctx.Done():
		}
	}
	return nil
}

func keys(accts []*account.Account) []string {
	out := make([]string, len(accts))
	for i, a := range accts {
		out[i] = a.Key()
	}
	return out
}

// spawnAccountSetup is a two-account delegated agent and its spawn tool,
// whose batches run on runner and whose background character spawns run on
// branch (made by the agent, then asked about once, as a first use would).
func spawnAccountSetup(t *testing.T) (a *Agent, accA, accB *account.Account, runner *accountBatchRunner, spawn func(mode string) (string, error)) {
	a, accA, accB, runner, spawn, _, _ = spawnAccountSetupBG(t)
	return
}

func spawnAccountSetupBG(t *testing.T) (a *Agent, accA, accB *account.Account, runner *accountBatchRunner, spawn func(mode string) (string, error), bg *branchRecordingAgent, sp *tools.Spawner) {
	t.Helper()
	reg := usagehold.New()
	a = holdAgent(reg, "claude-code")
	a.AgentID = "multi"
	accA, accB = twoAccountAgent(a, reg)
	a.Accounts().ForAgent("multi")
	runner = &accountBatchRunner{a: a}
	bg = &branchRecordingAgent{Agent: a}
	branches := 0
	sp = tools.NewSpawner(tools.SpawnDeps{
		AgentID:    "multi",
		MaxInherit: 2,
		Tracker:    tools.NewSpawnTracker(),
		Notifier:   tools.NewAsyncNotifier(func(string, string, string, string, *tools.AgentOrigin) {}),
		CharacterBranch: func(_ context.Context, parent, _ string) (string, error) {
			branches++
			if bg.beforeBranch != nil {
				bg.beforeBranch()
			}
			bk := parent + "/b17095968" + string(rune('0'+branches))
			_ = a.AccountFor(bk) // an early question binds it to the pick
			bg.branch = bk
			return bk, nil
		},
		Delegated: &tools.DelegatedSpawn{
			Spec: delegator.Spec{Caps: map[delegator.Capability]delegator.Support{
				delegator.CapBranch:        delegator.Yes(),
				delegator.CapIsolatedBatch: delegator.Yes(),
				delegator.CapReadOnlyBatch: delegator.Yes(),
			}},
			Runner: func() tools.BatchRunner { return runner },
		},
	}, func() tools.SpawnAgent { return bg })
	tool := sp.SpawnTool()
	n := 0
	spawn = func(mode string) (string, error) {
		n++
		args := map[string]any{"name": "s" + string(rune('a'+n)), "prompt": "task", "context": mode}
		if mode == "character-bg" {
			args["context"], args["background"] = "character", true
		}
		params, _ := json.Marshal(args)
		res, err := tool.Execute(tools.WithSessionKey(context.Background(), multiOnA), params)
		return res.Text, err
	}
	return a, accA, accB, runner, spawn, bg, sp
}

func holdAt(acct *account.Account, lift time.Duration) {
	acct.ObserveUsage(usagehold.Event{Window: "five_hour", Util: 0.99, HasUtil: true, ResetAt: time.Now().Add(lift)})
}

// TestSpawn_SpawnersAccountHeldOtherOpen_Starts (#3292): with the spawner's
// account A held and B open, a raw, character and explore spawn each starts
// and runs on B.
func TestSpawn_SpawnersAccountHeldOtherOpen_Starts(t *testing.T) {
	a, accA, accB, runner, spawn := spawnAccountSetup(t)
	holdAt(accA, time.Hour)
	if r := a.UsageHoldRefusal(multiOnA); r == "" {
		t.Fatal("setup: the spawner's account A is not held")
	}
	for _, mode := range []string{"raw", "character", "explore"} {
		if _, err := spawn(mode); err != nil {
			t.Errorf("%s spawn with A held and B open: %v, want it started on B", mode, err)
		}
	}
	got := runner.ran()
	if len(got) != 3 {
		t.Fatalf("spawns run = %d, want 3", len(got))
	}
	for i, acct := range got {
		if acct != accB {
			t.Errorf("spawn %d ran on %s, want the open account B", i, acct.Key())
		}
	}
}

// TestSpawn_AllAccountsHeld_RefusedNamingEach (#3292): with A and B both
// held, a character spawn is refused, and the reason names both accounts.
func TestSpawn_AllAccountsHeld_RefusedNamingEach(t *testing.T) {
	_, accA, accB, runner, spawn := spawnAccountSetup(t)
	holdAt(accA, time.Hour)
	holdAt(accB, 2*time.Hour)
	_, err := spawn("character")
	if err == nil {
		t.Fatal("character spawn with every account held: started, want refused")
	}
	msg := err.Error()
	if !strings.Contains(msg, "NOT started") || !strings.Contains(msg, "/cc/A") || !strings.Contains(msg, "/cc/B") {
		t.Errorf("refusal = %q, want NOT started naming both accounts (/cc/A, /cc/B)", msg)
	}
	if n := len(runner.ran()); n != 0 {
		t.Errorf("spawns run = %d, want none", n)
	}
}

// TestSpawn_CloneFromHeldAccount_Refused (#3292, #3246 ruling: a transcript
// fork must use the same account): a clone from the spawner's held account A
// is refused though B is open, naming A.
func TestSpawn_CloneFromHeldAccount_Refused(t *testing.T) {
	_, accA, _, runner, spawn := spawnAccountSetup(t)
	holdAt(accA, time.Hour)
	_, err := spawn("clone")
	if err == nil {
		t.Fatal("clone from held A: started, want refused")
	}
	if msg := err.Error(); !strings.Contains(msg, "NOT started") || !strings.Contains(msg, "/cc/A") || strings.Contains(msg, "/cc/B") {
		t.Errorf("refusal = %q, want NOT started naming A only", msg)
	}
	if n := len(runner.ran()); n != 0 {
		t.Errorf("spawns run = %d, want none", n)
	}
}

// TestSpawn_RunsOnTheAccountItsGateApproved (#3292): the account the gate
// approved is the one the spawn runs on. The gate approves B (A held); then,
// before the spawn's session is made, B holds too with a later lift than A,
// so a second pick would take A. The spawn still runs on B.
func TestSpawn_RunsOnTheAccountItsGateApproved(t *testing.T) {
	a, accA, accB, runner, spawn := spawnAccountSetup(t)
	holdAt(accA, time.Hour)
	runner.before = func() { holdAt(accB, 3*time.Hour) }
	if _, err := spawn("character"); err != nil {
		t.Fatalf("character spawn: %v", err)
	}
	if got := a.Accounts().Next(); got != accA {
		t.Fatalf("setup: a second pick now takes %s, want A", got.Key())
	}
	if got := runner.ran(); len(got) != 1 || got[0] != accB {
		t.Errorf("spawn ran on %v, want B (the account its gate approved)", keys(got))
	}
}

// TestSpawn_BackgroundCharacterRunsOnTheAccountItsGateApproved (#3292): a
// background character spawn's branch is on the account its gate approved
// (B, with A held). As the branch is made B holds too, with a later lift
// than A, and the branch is asked about once (a pick then takes A): it is
// still on B, stored so. (Its turn then waits for B's lift, as the hold of
// the account it runs on decides.)
func TestSpawn_BackgroundCharacterRunsOnTheAccountItsGateApproved(t *testing.T) {
	a, accA, accB, _, spawn, bg, _ := spawnAccountSetupBG(t)
	holdAt(accA, time.Hour)
	bg.beforeBranch = func() { holdAt(accB, 3*time.Hour) }
	if _, err := spawn("character-bg"); err != nil {
		t.Fatalf("background character spawn with A held and B open: %v, want started", err)
	}
	if bg.branch == "" {
		t.Fatal("no branch made")
	}
	if got := a.Accounts().Next(); got != accA {
		t.Fatalf("setup: a pick now takes %s, want A", got.Key())
	}
	if got := a.AccountFor(bg.branch); got != accB {
		t.Errorf("background character spawn's branch on %s, want B", got.Key())
	}
}
