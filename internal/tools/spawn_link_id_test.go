package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// #2996 (Dick 2026-11-11, Q67): a finished spawn's row in Message info opens
// the transcript the app showed while it ran. The app keys that transcript by
// the running spawn's id (the subagent group key the observer sends), and
// Message info names the spawn by its link id, so the two must be one id.
// These tests make a ledger the live one, so none is parallel.

func TestSpawnLinkID_DirectSpawnStreamsUnderLinkID(t *testing.T) {
	l := liveLedger(t)
	s, obs, _ := newObservedDirectSpawner("lid1")
	s.deps.LiveTurn = func(string) string { return "lid1/c1@7" }
	ctx := WithSessionKey(context.Background(), "lid1/c1")
	ctx = s.recordSpawnLink(ctx, "lid1/c1", "d1", spawnModeRaw)
	if _, err := s.runDirect(ctx, s, spawnModeRaw, "d1", "p", "", func(context.Context) (ToolResult, error) {
		return TextResult("ok"), nil
	}); err != nil {
		t.Fatal(err)
	}
	spawns, err := l.TurnSpawns("lid1/c1@7")
	if err != nil {
		t.Fatal(err)
	}
	if len(spawns) != 1 || len(obs.started) != 1 {
		t.Fatalf("spawn links = %+v, observed starts = %+v: want one of each", spawns, obs.started)
	}
	if spawns[0].SpawnID != obs.started[0].ID {
		t.Errorf("spawn link id = %q, observed (transcript) id = %q: want the same id", spawns[0].SpawnID, obs.started[0].ID)
	}
}

func TestSpawnLinkID_BackgroundSpawnStreamsUnderLinkID(t *testing.T) {
	l := liveLedger(t)
	obs := &recordingSpawnObserver{}
	agent := &blockingSpawnAgent{running: make(chan struct{})}
	tracker := NewSpawnTracker()
	deps := SpawnDeps{
		Sessions:   &mockSessionBrancher{},
		AgentID:    "test",
		MaxInherit: 2,
		Notifier:   NewAsyncNotifier(func(_, _, _, _ string, _ *AgentOrigin) {}),
		Tracker:    tracker,
		Observer:   obs,
		LiveTurn:   func(string) string { return "test/imain@9" },
	}
	tool := NewSpawner(deps, func() SpawnAgent { return agent }).SpawnTool()
	ctx := WithSessionKey(context.Background(), "test/imain")
	params, _ := json.Marshal(map[string]string{"name": "lid2", "prompt": "p", "context": "clone"})
	if _, err := tool.Execute(ctx, params); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	select {
	case <-agent.running:
	case <-time.After(5 * time.Second):
		t.Fatal("spawn never ran")
	}
	t.Cleanup(func() { tracker.Stop("test/imain") })
	spawns, err := l.TurnSpawns("test/imain@9")
	if err != nil {
		t.Fatal(err)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(spawns) != 1 || len(obs.started) != 1 {
		t.Fatalf("spawn links = %+v, observed starts = %+v: want one of each", spawns, obs.started)
	}
	if spawns[0].SpawnID != obs.started[0].ID {
		t.Errorf("spawn link id = %q, observed (transcript) id = %q: want the same id", spawns[0].SpawnID, obs.started[0].ID)
	}
}
