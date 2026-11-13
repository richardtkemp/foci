package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// chainBatchRunner runs each delegated batch by calling hook with its
// (registered) session key and request, as RunBatch registers the key
// (OnSession) before the batch's backend launches.
type chainBatchRunner struct {
	mu   sync.Mutex
	n    int
	pfx  string
	hook func(key string, req delegator.BatchRequest) (string, error)
}

func (r *chainBatchRunner) RunBatch(_ context.Context, req delegator.BatchRequest) (string, error) {
	r.mu.Lock()
	r.n++
	key := fmt.Sprintf("%s/b%d", r.pfx, r.n)
	r.mu.Unlock()
	if req.OnSession != nil {
		req.OnSession(key)
	}
	return r.hook(key, req)
}

// TestSpawnDepthChainsThroughDelegatedBatches proves #3061 on the delegated
// direct path, end to end: a character spawn of a chat is a depth-1 batch
// whose exec bridge has the spawn tools (it may spawn under the default limit
// 2); its own spawn is a depth-2 batch whose bridge has none; and a spawn call
// from it anyway (a bridge built before the limit was lowered) is refused,
// naming its depth and the limit.
func TestSpawnDepthChainsThroughDelegatedBatches(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	for _, n := range []string{"spawn", "spawn_status", "read"} {
		reg.Register(&Tool{Name: n})
	}
	type seen struct {
		depth  int
		bridge bool // the batch's exec bridge has foci_spawn
	}
	var mu sync.Mutex
	var got []seen
	var nestedErr error
	runner := &chainBatchRunner{pfx: "depthchain/c1"}
	s := newDelegatedSpawner(runner, allBatchCaps)
	s.deps.AgentID = "depthchain"
	s.deps.Tracker = NewSpawnTracker()
	runner.hook = func(key string, req delegator.BatchRequest) (string, error) {
		may := SessionMaySpawn(key)
		mu.Lock()
		got = append(got, seen{SessionSpawnDepth(key), RegistryForAccess(reg, req.Access, may).Get("spawn") != nil})
		level := len(got)
		mu.Unlock()
		if level <= 2 {
			// The batch calls foci_spawn over its exec bridge: only its
			// session key comes with the call.
			ctx := WithSessionKey(context.Background(), key)
			res, err := s.SpawnTool().Execute(ctx, json.RawMessage(fmt.Sprintf(`{"name":"lvl%d","prompt":"level %d"}`, level+1, level+1)))
			if err != nil {
				mu.Lock()
				nestedErr = err
				mu.Unlock()
				return "stopped at " + key, nil
			}
			return res.Text, nil
		}
		return "leaf", nil
	}
	if _, err := s.SpawnTool().Execute(WithSessionKey(context.Background(), "depthchain/c1"), json.RawMessage(`{"name":"lvl1","prompt":"level 1"}`)); err != nil {
		t.Fatalf("spawn from the chat: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("batches run = %+v, want 2 (the depth-2 batch's spawn refused)", got)
	}
	if got[0].depth != 1 || !got[0].bridge {
		t.Errorf("first batch = %+v, want depth 1 with the spawn tools", got[0])
	}
	if got[1].depth != 2 || got[1].bridge {
		t.Errorf("second batch = %+v, want depth 2 without the spawn tools", got[1])
	}
	if nestedErr == nil || !strings.Contains(nestedErr.Error(), "depth 2") || !strings.Contains(nestedErr.Error(), "limit is 2") {
		t.Errorf("spawn from the depth-2 batch: err = %v, want refused naming depth 2 and limit 2", nestedErr)
	}
}

// TestBackgroundSpawnDepthRecorded proves a background spawn's depth is its
// spawner's plus one, tracked and persisted with it (#3061): a character
// spawn of the chat is depth 1, its own background spawn depth 2 (no longer
// refused as "a background spawn cannot start another"), and the depth-2
// branch may start none.
func TestBackgroundSpawnDepthRecorded(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(2)
	cb := &characterBranches{}
	h := newPersistHarness(agent, func(d *SpawnDeps) {
		d.CharacterBranch = cb.create
		d.PrepareCharacterBranch = cb.prepare
	})
	start := func(ctx context.Context, name string) (string, error) {
		res, err := h.spawner.SpawnTool().Execute(ctx, json.RawMessage(`{"name":"`+name+`","prompt":"p","background":true}`))
		if err != nil {
			return "", err
		}
		return spawnIDRe.FindStringSubmatch(res.Text)[1], nil
	}
	id1, err := start(h.ctx, "bgdepth1")
	if err != nil {
		t.Fatal(err)
	}
	<-agent.started
	branch1 := "test/imain/bchar"
	id2, err := start(WithSessionKey(context.Background(), branch1), "bgdepth2")
	if err != nil {
		t.Fatalf("background spawn from a depth-1 background spawn: %v, want allowed", err)
	}
	<-agent.started
	for id, want := range map[string]int{id1: 1, id2: 2} {
		if rec, ok := h.store.record(t, id); !ok || rec.Depth != want {
			t.Errorf("record %s = %+v (ok %v), want depth %d", id, rec, ok, want)
		}
	}
	branch2 := branch1 + "/bchar"
	if d := spawnDepthIn(h.tracker, branch2); d != 2 {
		t.Errorf("depth of %s = %d, want 2", branch2, d)
	}
	if _, err := start(WithSessionKey(context.Background(), branch2), "bgdepth3"); err == nil || !strings.Contains(err.Error(), "spawn depth limit is 2") {
		t.Errorf("spawn from the depth-2 branch: err = %v, want refused by the limit", err)
	}
	close(agent.gates[0])
	close(agent.gates[1])
	h.nextDelivery(t).done(true)
	h.nextDelivery(t).done(true)
}

// TestAPICharacterOneShotGetsSpawnBelowLimit proves an API character
// one-shot is offered spawn only while it may spawn (#3061: "API spawns
// should be able to spawn, but governed by depth limit"): its depth comes
// from its spawnLink, as runDirect and a restart's resume set it.
func TestAPICharacterOneShotGetsSpawnBelowLimit(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	for _, n := range []string{"spawn", "spawn_send", "read"} {
		reg.Register(&Tool{Name: n})
	}
	s := NewSpawner(SpawnDeps{AgentID: "oneshotdepth", Registry: reg, FallbackModel: "anthropic/claude-haiku-4-5", FallbackFormat: "anthropic"}, func() SpawnAgent { return nil })
	for depth, want := range map[int]bool{1: true, 2: false} {
		ctx := withSpawnLink(WithSessionKey(context.Background(), "oneshotdepth/c1"), spawnLink{s: s, parent: "oneshotdepth/c1", id: "spawn-x", info: RunningSpawn{ID: "spawn-x", Direct: true, Depth: depth}})
		_, p, err := s.apiOneShotRun(ctx, &oneShotRecord{Mode: spawnModeCharacter}, "task")
		if err != nil {
			t.Fatal(err)
		}
		if (p.tools["spawn"] != nil) != want || p.tools["spawn_send"] != nil {
			t.Errorf("depth %d: one-shot has spawn = %v (want %v), spawn_send = %v (want false)", depth, p.tools["spawn"] != nil, want, p.tools["spawn_send"] != nil)
		}
	}
}

// TestSessionToolDefsFollowSpawnDepth proves an API turn on a session at the
// limit is not shown the spawn tools, and one below it is (#3061).
func TestSessionToolDefsFollowSpawnDepth(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	for _, n := range []string{"spawn", "spawn_status", "read"} {
		reg.Register(&Tool{Name: n, Parameters: json.RawMessage(`{"type":"object","properties":{}}`)})
	}
	// Spawns is the tracker the gateway shares; the keys are this test's own.
	Spawns.add("tooldefdepth/c1", RunningSpawn{ID: "spawn-tdd1", BranchKey: "tooldefdepth/c1/b1", Depth: 1}, func() {})
	Spawns.add("tooldefdepth/c1", RunningSpawn{ID: "spawn-tdd2", BranchKey: "tooldefdepth/c1/b2", Depth: 2}, func() {})
	names := func(key string) map[string]bool {
		out := map[string]bool{}
		for _, d := range reg.ToolDefsForSession(key) {
			out[d.Name()] = true
		}
		return out
	}
	for key, want := range map[string]bool{"tooldefdepth/c1": true, "tooldefdepth/c1/b1": true, "tooldefdepth/c1/b2": false} {
		got := names(key)
		if got["spawn"] != want || got["spawn_status"] != want || !got["read"] {
			t.Errorf("%s: tools = %v, want spawn tools %v and read", key, got, want)
		}
	}
}

// TestMaxSpawnDepthLive proves the limit applies to the next spawn call
// without a restart, and a value below 1 counts as 1 (#3061). Not parallel:
// it changes the process-wide limit, and restores it before any parallel
// test of the package runs.
func TestMaxSpawnDepthLive(t *testing.T) {
	defer SetMaxSpawnDepth(DefaultMaxSpawnDepth)
	tr := NewSpawnTracker()
	tr.add("livedepth/c1", RunningSpawn{ID: "spawn-l2", BranchKey: "livedepth/c1/b2", Depth: 2}, func() {})
	s := NewSpawner(SpawnDeps{AgentID: "livedepth", Tracker: tr}, func() SpawnAgent { return nil })
	ctx := WithSessionKey(context.Background(), "livedepth/c1/b2")
	if err := s.checkSpawnDepth(ctx); err == nil {
		t.Fatal("a depth-2 spawn may spawn at the default limit 2")
	}
	SetMaxSpawnDepth(4)
	if err := s.checkSpawnDepth(ctx); err != nil {
		t.Errorf("limit raised to 4: %v, want allowed", err)
	}
	SetMaxSpawnDepth(0)
	if MaxSpawnDepth() != 1 {
		t.Errorf("limit 0 reads %d, want 1", MaxSpawnDepth())
	}
	if err := s.checkSpawnDepth(WithSessionKey(context.Background(), "livedepth/c1")); err != nil {
		t.Errorf("a chat at limit 1: %v, want allowed", err)
	}
	if err := s.checkSpawnDepth(ctx); err == nil || !strings.Contains(err.Error(), "depth 2") || !strings.Contains(err.Error(), "limit is 1") {
		t.Errorf("depth-2 spawn at limit 1: err = %v, want refused naming both", err)
	}
}

// TestRestoredRecordWithoutDepthIsASpawn proves a record written before
// depths existed restores as a depth-1 spawn, not a depth-0 root.
func TestRestoredRecordWithoutDepthIsASpawn(t *testing.T) {
	t.Parallel()
	if d := recInfo(runningRecord("spawn-old")).Depth; d != 1 {
		t.Errorf("legacy record depth = %d, want 1", d)
	}
	rec := runningRecord("spawn-new")
	rec.Depth = 2
	if d := recInfo(rec).Depth; d != 2 {
		t.Errorf("record depth = %d, want 2", d)
	}
}
