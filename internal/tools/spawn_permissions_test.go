package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/platform"
)

// sessionProbeRunner is a BatchRunner that mints its batch session key as the
// manager does (OnSession, before the turn) and records what SpawnOfSession
// and BranchOwner say about it while the batch runs.
type sessionProbeRunner struct {
	s        *Spawner
	key      string
	seen     SpawnSession
	seenOK   bool
	owner    string
	ownerOK  bool
	afterKey string
}

func (r *sessionProbeRunner) RunBatch(_ context.Context, req delegator.BatchRequest) (string, error) {
	if req.OnSession != nil {
		req.OnSession(r.key)
	}
	r.seen, r.seenOK = SpawnOfSession(r.key)
	r.owner, r.ownerOK = r.s.BranchOwner(r.key)
	return "done", nil
}

// TestDirectSpawn_SessionNamesItsSpawner proves a direct delegated spawn's
// batch session is known as a spawn while its call waits (#2684): the
// gateway names it on a prompt (SpawnOfSession) and routes its prompts to the
// session that started it (BranchOwner), and records whether that session's
// turn was unattended. After the call returns the session is no spawn's.
// Fail-arm: before #2684 BranchOwner knew only background branches, so a
// direct spawn's prompt had no owner.
func TestDirectSpawn_SessionNamesItsSpawner(t *testing.T) {
	t.Parallel()
	for _, unattended := range []bool{false, true} {
		t.Run(fmt.Sprintf("unattended=%v", unattended), func(t *testing.T) {
			t.Parallel()
			parent := fmt.Sprintf("test/c2684%v", unattended)
			runner := &sessionProbeRunner{key: parent + "/b1"}
			s := newDelegatedSpawner(runner, allBatchCaps)
			runner.s = s
			s.deps.Tracker = NewSpawnTracker()
			var asked []string
			s.deps.TurnUnattended = func(sk string) bool { asked = append(asked, sk); return unattended }
			if _, err := s.SpawnTool().Execute(WithSessionKey(context.Background(), parent), json.RawMessage(`{"prompt":"run go build"}`)); err != nil {
				t.Fatalf("spawn: %v", err)
			}
			if !runner.seenOK || !runner.seen.Direct || runner.seen.Parent != parent || runner.seen.ID == "" || runner.seen.Description == "" {
				t.Fatalf("SpawnOfSession while running = %+v, %v; want the direct spawn of %s", runner.seen, runner.seenOK, parent)
			}
			if runner.seen.SpawnerUnattended != unattended {
				t.Errorf("SpawnerUnattended = %v, want %v", runner.seen.SpawnerUnattended, unattended)
			}
			if len(asked) != 1 || asked[0] != parent {
				t.Errorf("TurnUnattended asked about %v, want [%s]", asked, parent)
			}
			if !runner.ownerOK || runner.owner != parent {
				t.Errorf("BranchOwner while running = %q, %v; want %s", runner.owner, runner.ownerOK, parent)
			}
			if _, ok := SpawnOfSession(runner.key); ok {
				t.Error("the batch session is still a spawn's after its call returned")
			}
		})
	}
}

// clearRecorder records SpawnDeps.ClearPrompts calls.
type clearRecorder struct {
	mu   sync.Mutex
	keys []string
}

func (c *clearRecorder) clear(sk string) { c.mu.Lock(); c.keys = append(c.keys, sk); c.mu.Unlock() }

func (c *clearRecorder) got() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.keys...)
}

// TestBackgroundSpawn_RecordsSpawnerAndClearsPrompts proves a background
// spawn records, at launch, whether the turn that started it was unattended
// (#2684), and that when its run ends the prompts its branch left unanswered
// are taken down (ClearPrompts).
func TestBackgroundSpawn_RecordsSpawnerAndClearsPrompts(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	agent := &gatedSpawnAgent{gate: gate}
	h := newFollowupHarness(agent)
	h.spawner.deps.TurnUnattended = func(sk string) bool { return sk == "test/imain" }
	cr := &clearRecorder{}
	h.spawner.deps.ClearPrompts = cr.clear
	h.start(t, "watch the build")
	running := h.tracker.Running("test/imain")
	if len(running) != 1 || !running[0].SpawnerUnattended {
		t.Fatalf("running = %+v, want one spawn with SpawnerUnattended", running)
	}
	if got := cr.got(); len(got) != 0 {
		t.Fatalf("prompts cleared while the spawn runs: %v", got)
	}
	close(gate)
	select {
	case <-h.delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("no spawn result")
	}
	if got := cr.got(); len(got) != 1 || got[0] != running[0].BranchKey {
		t.Errorf("ClearPrompts calls = %v, want one for %s", got, running[0].BranchKey)
	}
}

// gatedSpawnAgent holds each run until gate closes.
type gatedSpawnAgent struct{ gate chan struct{} }

func (a *gatedSpawnAgent) HandleMessage(ctx context.Context, _ string, _ []string, _ []platform.Attachment) error {
	select {
	case <-a.gate:
	case <-ctx.Done():
	}
	return nil
}

// TestSpawnRecord_KeepsSpawnerUnattended proves the flag survives a restart:
// it is in the persisted record and back in the restored spawn.
func TestSpawnRecord_KeepsSpawnerUnattended(t *testing.T) {
	t.Parallel()
	ts := &trackedSpawn{info: RunningSpawn{ID: "spawn-1", BranchKey: "test/c1/b1", SpawnerUnattended: true}, running: true}
	rec := ts.record("test/c1")
	if rec == nil || !rec.SpawnerUnattended {
		t.Fatalf("record = %+v, want SpawnerUnattended", rec)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var back spawnRecord
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !recInfo(back).SpawnerUnattended {
		t.Error("restored spawn lost SpawnerUnattended: after a restart it would prompt")
	}
}
