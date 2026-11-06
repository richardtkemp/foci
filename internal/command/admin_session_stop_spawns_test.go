package command

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/platform"
	"foci/internal/tools"
)

type forkOnce struct{}

func (forkOnce) ForkSession(_ context.Context, parent string, _ tools.BranchOptions) (string, bool, error) {
	return parent + "/b1", true, nil
}
func (forkOnce) SessionPath(key string) (string, error) { return "/tmp/x-" + key, nil }

// untilCancelled runs a spawn until its context is cancelled.
type untilCancelled struct{ running chan struct{} }

func (a untilCancelled) HandleMessage(ctx context.Context, _ string, _ []string, _ []platform.Attachment) error {
	close(a.running)
	<-ctx.Done()
	return ctx.Err()
}

func TestStopSubagents_StopsBackgroundSpawns(t *testing.T) {
	// /stop subagents stops the session's background foci spawns, which the
	// app lists as subagents, on an API agent with no delegated backend (#2139).
	done := make(chan string, 1)
	ag := untilCancelled{running: make(chan struct{})}
	tool := tools.NewSpawner(tools.SpawnDeps{
		Sessions:   forkOnce{},
		MaxInherit: 1,
		Notifier:   tools.NewAsyncNotifier(func(_, msg, _, _ string, _ *tools.AgentOrigin) { done <- msg }),
		Tracker:    tools.Spawns,
	}, func() tools.SpawnAgent { return ag }).SpawnTool()
	ctx := tools.WithSessionKey(context.Background(), "agent:stoptest:main")
	params, _ := json.Marshal(map[string]string{"name": "worker", "prompt": "work", "context": "clone"})
	if _, err := tool.Execute(ctx, params); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	<-ag.running

	resp, err := StopCommand().Execute(ctx, Request{Args: "subagents"}, CommandContext{})
	if err != nil {
		t.Fatalf("/stop subagents: %v", err)
	}
	if resp.Text != "Stopping 1 subagent." {
		t.Errorf("reply = %q, want one subagent stopped", resp.Text)
	}
	select {
	case msg := <-done:
		if !strings.Contains(msg, "stopped by the user") {
			t.Errorf("spawn result = %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("spawn was not stopped")
	}
}

// TestStopItem_StopsOnlyThatSpawn pins #2901: /stop item names one entry of the
// app's running list, keeps its id's case, and leaves the other spawn running.
func TestStopItem_StopsOnlyThatSpawn(t *testing.T) {
	const sk = "agent:stopitemtest:main"
	cancelled := map[string]bool{}
	for _, id := range []string{"Spawn-A", "Spawn-B"} {
		tools.Spawns.Add(sk, tools.RunningSpawn{ID: id}, func() { cancelled[id] = true })
	}
	t.Cleanup(func() { tools.Spawns.Stop(sk) })
	ctx := tools.WithSessionKey(context.Background(), sk)

	resp, err := StopCommand().Execute(ctx, Request{Args: "item Spawn-A"}, CommandContext{})
	if err != nil {
		t.Fatalf("/stop item: %v", err)
	}
	if resp.Text != "Stopping it." {
		t.Errorf("reply = %q", resp.Text)
	}
	if !cancelled["Spawn-A"] || cancelled["Spawn-B"] {
		t.Errorf("cancelled = %v, want only Spawn-A", cancelled)
	}

	resp, err = StopCommand().Execute(ctx, Request{Args: "item Spawn-A"}, CommandContext{})
	if err != nil {
		t.Fatalf("/stop item again: %v", err)
	}
	if resp.Text != "That is no longer running." {
		t.Errorf("second stop reply = %q", resp.Text)
	}
}

// TestStopItem_WithoutAnIdIsRejected: "item" alone must not fall through to a
// wider stop.
func TestStopItem_WithoutAnIdIsRejected(t *testing.T) {
	ctx := tools.WithSessionKey(context.Background(), "agent:stopitemtest2:main")
	if _, err := StopCommand().Execute(ctx, Request{Args: "item"}, CommandContext{}); err == nil {
		t.Fatal("/stop item with no id must be an error")
	}
}
