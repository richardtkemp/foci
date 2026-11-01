package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/turnevent"
)

// autonomousHarness is a followupHarness on its own agent id, registered for
// SpawnBranchTurn, so it does not share the registry entry of other tests.
func newAutonomousHarness(t *testing.T, agentID string, agent SpawnAgent) (*followupHarness, string) {
	t.Helper()
	h := &followupHarness{tracker: NewSpawnTracker(), delivered: make(chan string, 8)}
	notifier := NewAsyncNotifier(func(sk, msg, _, _ string, _ *AgentOrigin) {
		h.delivered <- sk + "\n" + msg
	})
	h.spawner = NewSpawner(SpawnDeps{
		Sessions:   &mockSessionBrancher{},
		AgentID:    agentID,
		MaxInherit: 2,
		Notifier:   notifier,
		Tracker:    h.tracker,
	}, func() SpawnAgent { return agent })
	registerTestSpawner(t, h.spawner)
	parent := agentID + "/imain"
	h.ctx = WithSessionKey(context.Background(), parent)
	return h, parent
}

func (h *followupHarness) resultFor(t *testing.T, parent string) string {
	t.Helper()
	select {
	case msg := <-h.delivered:
		if !strings.HasPrefix(msg, parent+"\n") {
			t.Errorf("result delivered to %q, want the parent session %q", strings.SplitN(msg, "\n", 2)[0], parent)
		}
		return msg
	case <-time.After(10 * time.Second):
		t.Fatal("no [SPAWN RESULT] delivered")
		return ""
	}
}

func (h *followupHarness) noResult(t *testing.T) {
	t.Helper()
	select {
	case msg := <-h.delivered:
		t.Fatalf("unexpected delivery: %q", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestSpawnBranchTurn_FinishedSpawnReportsToSpawner is #2729: after a
// background spawn's turn ended, its Claude Code session started a run by
// itself (a background Bash command it had started finished). That run is
// the spawn's: it is reported to the spawner as one more [SPAWN RESULT], the
// spawn shows running while it runs, and it reaches no chat.
func TestSpawnBranchTurn_FinishedSpawnReportsToSpawner(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(0)
	h, parent := newAutonomousHarness(t, "auto2729a", agent)
	id := h.start(t, "run the suite")
	waitStarted(t, agent, 0)
	if msg := h.resultFor(t, parent); !strings.Contains(msg, "result 0: run the suite") {
		t.Fatalf("first result = %q", msg)
	}
	branchKey := parent + "/b1000000000"

	stopped := make(chan struct{}, 1)
	sink, ok := SpawnBranchTurn(branchKey, func() { stopped <- struct{}{} })
	if !ok || sink == nil {
		t.Fatalf("SpawnBranchTurn(%q) = %v, %v; want the spawn's branch claimed", branchKey, sink, ok)
	}
	if sink.DeliversToPlatform() {
		t.Error("the spawn's autonomous sink claims to deliver to a platform")
	}
	ctx := context.Background()
	sink.Emit(ctx, turnevent.TurnStart{})
	if st := h.status(t, id); !strings.Contains(st, "running") {
		t.Errorf("status during the autonomous run = %q, want running", st)
	}
	sink.Emit(ctx, turnevent.TextBlock{Text: "the suite passed", Phase: turnevent.PhaseIntermediate})
	h.noResult(t)
	sink.Emit(ctx, turnevent.TurnComplete{FinalText: "the suite passed"})

	msg := h.resultFor(t, parent)
	if !strings.Contains(msg, "[SPAWN RESULT]") || !strings.Contains(msg, id) || !strings.Contains(msg, "the suite passed") {
		t.Errorf("autonomous run's result = %q, want a [SPAWN RESULT] for %s with its answer", msg, id)
	}
	st := h.status(t, id)
	for _, want := range []string{"completed", spawnWokeNote, "text: the suite passed"} {
		if !strings.Contains(st, want) {
			t.Errorf("status lacks %q:\n%s", want, st)
		}
	}
	select {
	case <-stopped:
		t.Error("the run was interrupted though nobody stopped it")
	default:
	}
}

// TestSpawnBranchTurn_StopInterruptsRun: spawn_stop on a spawn whose backend
// is in a run it started by itself interrupts that run, and the result says
// it was stopped.
func TestSpawnBranchTurn_StopInterruptsRun(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(0)
	h, parent := newAutonomousHarness(t, "auto2729b", agent)
	id := h.start(t, "run the suite")
	waitStarted(t, agent, 0)
	h.resultFor(t, parent)

	stopped := make(chan struct{}, 1)
	sink, ok := SpawnBranchTurn(parent+"/b1000000000", func() { stopped <- struct{}{} })
	if !ok {
		t.Fatal("branch not claimed")
	}
	sink.Emit(context.Background(), turnevent.TurnStart{})
	params, _ := json.Marshal(map[string]string{"id": id})
	if res, err := h.spawner.StopTool().Execute(h.ctx, params); err != nil || !strings.Contains(res.Text, "Stopping") {
		t.Fatalf("spawn_stop = %q, %v", res.Text, err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("spawn_stop did not interrupt the autonomous run")
	}
	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "partial"})
	if msg := h.resultFor(t, parent); !strings.Contains(msg, "was stopped") {
		t.Errorf("result = %q, want it stopped", msg)
	}
}

// TestSpawnBranchTurn_QueuedMessageRunsAsFollowUp: a spawn_send that could
// not fold into the autonomous run is queued, and runs as a follow-up turn on
// the branch after it, which reports its own result.
func TestSpawnBranchTurn_QueuedMessageRunsAsFollowUp(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(0)
	h, parent := newAutonomousHarness(t, "auto2729c", agent)
	id := h.start(t, "run the suite")
	waitStarted(t, agent, 0)
	h.resultFor(t, parent)
	branchKey := parent + "/b1000000000"

	sink, _ := SpawnBranchTurn(branchKey, nil)
	sink.Emit(context.Background(), turnevent.TurnStart{})
	if got := h.send(t, id, "also lint it"); !strings.Contains(got, "Queued") {
		t.Fatalf("send during the autonomous run = %q, want queued", got)
	}
	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "the suite passed"})
	if msg := h.resultFor(t, parent); !strings.Contains(msg, "the suite passed") {
		t.Errorf("autonomous run's result = %q", msg)
	}
	waitStarted(t, agent, 1)
	if msg := h.resultFor(t, parent); !strings.Contains(msg, "result 1: "+spawnMessagePrefix+"also lint it") {
		t.Errorf("follow-up result = %q, want the queued message's turn", msg)
	}
	if _, keys := agent.turns(); len(keys) != 2 || keys[1] != branchKey {
		t.Errorf("turn keys = %v, want the follow-up on %q", keys, branchKey)
	}
}

// TestSpawnBranchTurn_RunningSpawnKeepsItsRun: a run the backend opens while
// the spawn's own run is still marked running reports nothing itself; the
// spawn's run delivers its one result.
func TestSpawnBranchTurn_RunningSpawnKeepsItsRun(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(1)
	h, parent := newAutonomousHarness(t, "auto2729d", agent)
	h.start(t, "run the suite")
	waitStarted(t, agent, 0)

	sink, ok := SpawnBranchTurn(parent+"/b1000000000", nil)
	if !ok {
		t.Fatal("branch of a running spawn not claimed: its run would reach a chat")
	}
	sink.Emit(context.Background(), turnevent.TurnStart{})
	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "side note"})
	h.noResult(t)
	close(agent.gates[0])
	if msg := h.resultFor(t, parent); !strings.Contains(msg, "result 0: run the suite") {
		t.Errorf("result = %q, want the spawn's own run's", msg)
	}
	h.noResult(t)
}

// TestSpawnBranchTurn_NotASpawnBranch: any other session's autonomous run is
// left to the agent.
func TestSpawnBranchTurn_NotASpawnBranch(t *testing.T) {
	t.Parallel()
	_, parent := newAutonomousHarness(t, "auto2729e", newScriptedSpawnAgent(0))
	for _, sk := range []string{parent, parent + "/b9999999999", "nobody/c1"} {
		if sink, ok := SpawnBranchTurn(sk, nil); ok || sink != nil {
			t.Errorf("SpawnBranchTurn(%q) claimed it", sk)
		}
	}
}
