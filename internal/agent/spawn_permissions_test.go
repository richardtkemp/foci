package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/tools"
)

// #2684: a spawn's asks go up to its spawner instead of being decided
// unattended — unless its spawner's turn was unattended, it is a cross-agent
// spawn, or no tracked spawn runs on the session.

// trackSpawnBranch registers a background spawn on branchKey in the
// process-wide tracker, as Spawner.launchBackground does, and stops it when
// the test ends. Each test uses its own keys, so parallel tests do not meet.
func trackSpawnBranch(t *testing.T, parent, branchKey string, spawnerUnattended bool) {
	t.Helper()
	tools.Spawns.Add(parent, tools.RunningSpawn{
		ID: fmt.Sprintf("sp%d", time.Now().UnixNano()), BranchKey: branchKey,
		Description: "build the thing", SpawnerUnattended: spawnerUnattended,
	}, func() {})
	t.Cleanup(func() { tools.Spawns.Stop(parent) })
}

// TestSpawnBranch_AttendedSpawnerPrompts proves a background character
// spawn started by an attended turn launches WITHOUT SkipPermissions and its
// turns are attended, so its asks reach the prompt funcs (the spawner's chat).
// Fail-arm: before #2684 every spawn session launched with SkipPermissions.
func TestSpawnBranch_AttendedSpawnerPrompts(t *testing.T) {
	t.Parallel()
	be := &batchTurnBackend{}
	be.completeTurns()
	a := newAccessTestAgent(t, be, bothBatchCaps)
	const parent, bk = "helen/c26841", "helen/c26841/b1700000001"
	trackSpawnBranch(t, parent, bk, false)
	a.PrepareCharacterSpawnBranch(bk, "")
	if a.DelegatedManager.isUnattendedSession(bk) {
		t.Fatal("an attended spawner's character spawn counts as unattended: its asks would be denied instead of reaching the spawner")
	}
	if a.turnUnattended(&TurnState{SessionKey: bk, Trigger: "spawn"}) {
		t.Fatal("an attended spawn's turn is unattended")
	}
	if _, err := a.DelegatedManager.Get(context.Background(), bk); err != nil {
		t.Fatalf("Get: %v", err)
	}
	opts := be.startedOpts()
	if len(opts) != 1 {
		t.Fatalf("started %d times, want 1", len(opts))
	}
	if opts[0].SkipPermissions {
		t.Error("an attended spawner's spawn launched with SkipPermissions: nobody would ever be asked")
	}
	if opts[0].BatchAccess != delegator.BatchAccessParent {
		t.Errorf("launched with access %s, want parent", opts[0].BatchAccess)
	}
}

// TestSpawnBranch_UnattendedCases proves the spawns that keep today's
// unattended behaviour: one whose spawner's turn was unattended, and a
// cross-agent spawn's branch (no chat of its agent may show its asks).
func TestSpawnBranch_UnattendedCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, parent, bk  string
		spawnerUnattended bool
	}{
		{"spawner unattended", "helen/c26842", "helen/c26842/b1700000002", true},
		{"cross-agent", "clutch/c26843", "helen/ixspawn/b1700000003", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			be := &batchTurnBackend{}
			be.completeTurns()
			a := newAccessTestAgent(t, be, bothBatchCaps)
			trackSpawnBranch(t, c.parent, c.bk, c.spawnerUnattended)
			a.PrepareCharacterSpawnBranch(c.bk, "")
			if !a.DelegatedManager.isUnattendedSession(c.bk) {
				t.Fatal("session is not unattended")
			}
			if _, err := a.DelegatedManager.Get(context.Background(), c.bk); err != nil {
				t.Fatalf("Get: %v", err)
			}
			if opts := be.startedOpts(); len(opts) != 1 || !opts[0].SkipPermissions {
				t.Errorf("launch opts = %+v, want one launch with SkipPermissions", opts)
			}
		})
	}
}

// TestUnattendedTriggers_NoSpawnPurposes pins that a spawn purpose no longer
// makes its turn unattended by itself (#2684): the session decides.
func TestUnattendedTriggers_NoSpawnPurposes(t *testing.T) {
	for _, p := range []string{delegator.BatchPurposeSpawnRaw, delegator.BatchPurposeSpawnCharacter, delegator.BatchPurposeSpawnExplore} {
		if isUnattendedTrigger(p) {
			t.Errorf("%s is an unattended trigger: every spawn's asks would be denied", p)
		}
	}
	if !isUnattendedTrigger(delegator.BatchPurposeConsolidation) {
		t.Error("consolidation is no longer unattended")
	}
}

// TestTurnUnattended_CountsUnattendedTurns proves TurnUnattended reports an
// unattended turn in flight on the session, and only while it runs.
func TestTurnUnattended_CountsUnattendedTurns(t *testing.T) {
	t.Parallel()
	a := &Agent{AgentID: "helen"}
	const sk = "helen/c26844"
	attended := a.markUnattendedTurn(sk, false)
	if a.TurnUnattended(sk) {
		t.Fatal("an attended turn reads unattended")
	}
	r1 := a.markUnattendedTurn(sk, true)
	r2 := a.markUnattendedTurn(sk, true)
	if !a.TurnUnattended(sk) {
		t.Fatal("an unattended turn in flight is not reported")
	}
	r1()
	r1() // a release is idempotent
	if !a.TurnUnattended(sk) {
		t.Fatal("one of two unattended turns ended and the other is no longer reported")
	}
	r2()
	attended()
	if a.TurnUnattended(sk) {
		t.Fatal("no turn in flight, still unattended")
	}
}

// TestClearSessionPrompts_TakesDownUnanswered proves the prompts a session
// showed are taken down when its backend closes (a direct spawn's batch ends)
// or ClearSessionPrompts is called (a background spawn's run ends), and an
// answered prompt is not.
func TestClearSessionPrompts_TakesDownUnanswered(t *testing.T) {
	t.Parallel()
	be := &batchTurnBackend{}
	be.completeTurns()
	a := newAccessTestAgent(t, be, bothBatchCaps)
	const sk = "helen/c26845/b1700000005"
	if _, err := a.DelegatedManager.Get(context.Background(), sk); err != nil {
		t.Fatalf("Get: %v", err)
	}
	var mu sync.Mutex
	fired := map[string]string{}
	listen := func(id string) func(string) {
		return func(reason string) { mu.Lock(); fired[id] = reason; mu.Unlock() }
	}
	a.DelegatedManager.RegisterPromptCancelListener(sk, "req-answered", listen("req-answered"))
	a.DelegatedManager.RegisterPromptCancelListener(sk, "req-open", listen("req-open"))
	a.DelegatedManager.ForgetPrompt(sk, "req-answered")

	a.DelegatedManager.ResetSession(sk)

	mu.Lock()
	open, openFired := fired["req-open"]
	_, answeredFired := fired["req-answered"]
	mu.Unlock()
	if !openFired || open != PromptEndedReason {
		t.Errorf("open prompt fired with %q (fired=%v), want %q", open, openFired, PromptEndedReason)
	}
	if answeredFired {
		t.Error("an answered prompt was taken down")
	}
	if n := a.DelegatedManager.ClearSessionPrompts(sk); n != 0 {
		t.Errorf("a second clear took down %d prompts, want 0", n)
	}
}

// controlBackend records the control requests sent to it.
type controlBackend struct {
	*batchTurnBackend
	mu   sync.Mutex
	sent []delegator.ControlRequest
}

func (c *controlBackend) SendControl(_ context.Context, req delegator.ControlRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, req)
	return nil
}

// TestSpawnBranch_InheritsSpawnerMode proves a spawn starts in the permission
// mode its spawner switched to (#2523 ruling: in auto mode nobody is asked),
// and that a spawner with no override sends nothing (the spawn already starts
// in the agent's default mode).
func TestSpawnBranch_InheritsSpawnerMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"auto", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
			t.Parallel()
			inner := &batchTurnBackend{}
			inner.completeTurns()
			be := &controlBackend{batchTurnBackend: inner}
			a := newAccessTestAgent(t, be, bothBatchCaps)
			parent := "helen/c26846" + mode
			bk := parent + "/b1700000006"
			a.DelegatedManager.SpawnerPermissionMode = func(sk string) string {
				if sk == parent {
					return mode
				}
				return "default"
			}
			trackSpawnBranch(t, parent, bk, false)
			a.PrepareCharacterSpawnBranch(bk, "")
			if _, err := a.DelegatedManager.Get(context.Background(), bk); err != nil {
				t.Fatalf("Get: %v", err)
			}
			be.mu.Lock()
			defer be.mu.Unlock()
			if mode == "" {
				if len(be.sent) != 0 {
					t.Errorf("sent %v, want nothing", be.sent)
				}
				return
			}
			if len(be.sent) != 1 {
				t.Fatalf("sent %d control requests, want 1", len(be.sent))
			}
			if r, ok := be.sent[0].(*delegator.SetPermissionModeRequest); !ok || r.Mode != mode {
				t.Errorf("sent %#v, want set_permission_mode %q", be.sent[0], mode)
			}
		})
	}
}

// TestCloneBranch_UnattendedSpawnerTurnUnattended proves requirement 5 for a
// clone: its branch is no batch (it prompts as an ordinary session), so only
// the tracker knows its spawner's turn was unattended. Its turns must then be
// unattended (rules, else deny) rather than prompting a chat. A clone of an
// attended turn keeps prompting.
// Fail-arm: with only the batch spec consulted, the unattended clone's turn
// was attended.
func TestCloneBranch_UnattendedSpawnerTurnUnattended(t *testing.T) {
	t.Parallel()
	be := &batchTurnBackend{}
	a := newAccessTestAgent(t, be, bothBatchCaps)
	const parent, unatt, att = "helen/c26845", "helen/c26845/b1700000005", "helen/c26845/b1700000006"
	trackSpawnBranch(t, parent, unatt, true)
	trackSpawnBranch(t, parent, att, false)
	if !a.turnUnattended(&TurnState{SessionKey: unatt, Trigger: "spawn"}) {
		t.Error("a clone of an unattended turn has an attended turn: its asks would reach a chat")
	}
	if a.turnUnattended(&TurnState{SessionKey: att, Trigger: "spawn"}) {
		t.Error("a clone of an attended turn has an unattended turn: its asks would be denied")
	}
}
