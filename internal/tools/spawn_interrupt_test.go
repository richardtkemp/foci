package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/platform"
	"foci/internal/turnevent"
)

// delegatedLikeSpawnAgent stands in for a delegated (CC) agent: its turn does
// NOT watch ctx — it ends only when the backend turn is interrupted, as
// Agent.HandleMessage on a delegated transport waits for the backend's
// completion and never for ctx (#2821).
type delegatedLikeSpawnAgent struct {
	started chan string

	mu          sync.Mutex
	interrupted []string
	release     chan struct{}
	once        sync.Once
}

func newDelegatedLikeSpawnAgent() *delegatedLikeSpawnAgent {
	return &delegatedLikeSpawnAgent{started: make(chan string, 4), release: make(chan struct{})}
}

func (a *delegatedLikeSpawnAgent) HandleMessage(ctx context.Context, sk string, _ []string, _ []platform.Attachment) error {
	a.started <- sk
	select {
	case <-a.release:
	case <-time.After(30 * time.Second): // a test that never interrupts fails, not hangs
	}
	turnevent.Emit(ctx, turnevent.TurnComplete{FinalText: "interrupted"})
	return nil
}

func (a *delegatedLikeSpawnAgent) InterruptTurn(sk string) error {
	a.mu.Lock()
	a.interrupted = append(a.interrupted, sk)
	a.mu.Unlock()
	a.once.Do(func() { close(a.release) })
	return nil
}

func (a *delegatedLikeSpawnAgent) interrupts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.interrupted...)
}

func (a *delegatedLikeSpawnAgent) waitStarted(t *testing.T) string {
	t.Helper()
	select {
	case sk := <-a.started:
		return sk
	case <-time.After(10 * time.Second):
		t.Fatal("the spawn's turn never started")
		return ""
	}
}

// #2821: spawn_stop on a background spawn whose agent is delegated interrupts
// the backend turn on the spawn's branch, so the spawn ends and its
// [SPAWN RESULT] says it was stopped. Fail-arm: before #2821 the stop only
// cancelled the spawn's ctx, which a delegated turn ignores, so the turn ran
// on and no result came.
func TestSpawnStop_InterruptsDelegatedTurn(t *testing.T) {
	t.Parallel()
	agent := newDelegatedLikeSpawnAgent()
	h := newFollowupHarness(agent)
	id := h.start(t, "a long clone")
	branchKey := agent.waitStarted(t)
	out, err := h.spawner.StopTool().Execute(h.ctx, json.RawMessage(`{"id":"`+id+`"}`))
	if err != nil || !strings.Contains(out.Text, "Stopping spawn "+id) {
		t.Fatalf("spawn_stop = %q, %v", out.Text, err)
	}
	if msg := h.result(t); !strings.Contains(msg, "was stopped by its parent agent (spawn_stop)") {
		t.Fatalf("result = %q, want stopped by its parent agent", msg)
	}
	if got := agent.interrupts(); len(got) != 1 || got[0] != branchKey {
		t.Errorf("interrupted %v, want exactly the spawn's branch %s", got, branchKey)
	}
}

// A legacy timeout must not stop a long-running background spawn. Explicit
// stop still interrupts exactly its delegated branch and reports it stopped.
func TestSpawnNoRunTimeout_StillStoppable(t *testing.T) {
	t.Parallel()
	agent := newDelegatedLikeSpawnAgent()
	h := newFollowupHarness(agent)
	params := json.RawMessage(`{"name":"timeout1","prompt":"long clone","context":"clone","timeout":1}`)
	if _, err := h.spawner.SpawnTool().Execute(h.ctx, params); err != nil {
		t.Fatal(err)
	}
	branchKey := agent.waitStarted(t)
	time.Sleep(1200 * time.Millisecond)
	if got := agent.interrupts(); len(got) != 0 {
		t.Fatalf("spawn interrupted by elapsed time: %v", got)
	}
	list := h.tracker.List("test/imain")
	if len(list) != 1 || !list[0].Running {
		t.Fatalf("spawn did not stay running: %+v", list)
	}
	if _, err := h.spawner.StopTool().Execute(h.ctx, json.RawMessage(`{"id":"timeout1"}`)); err != nil {
		t.Fatal(err)
	}
	if msg := h.result(t); !strings.Contains(msg, "stopped") {
		t.Errorf("result = %q, want stopped", msg)
	}
	if got := agent.interrupts(); len(got) != 1 || got[0] != branchKey {
		t.Errorf("interrupts = %v, want %s", got, branchKey)
	}
}

// A spawn whose turn ends on its own is not interrupted afterwards.
func TestSpawnCompleted_NoInterrupt(t *testing.T) {
	t.Parallel()
	agent := newDelegatedLikeSpawnAgent()
	close(agent.release) // the turn ends at once
	agent.once.Do(func() {})
	h := newFollowupHarness(agent)
	h.start(t, "a quick clone")
	agent.waitStarted(t)
	if msg := h.result(t); strings.Contains(msg, "stopped") {
		t.Fatalf("result = %q, want a normal completion", msg)
	}
	if got := agent.interrupts(); len(got) != 0 {
		t.Errorf("interrupted %v after the turn ended on its own, want none", got)
	}
}
