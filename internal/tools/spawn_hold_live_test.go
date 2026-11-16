package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/turnevent"
)

// #3143: a usage hold's lift and a spawn whose branch still has work in
// flight. Rulings (Dick 2026-11-16): Q1 "End it; only in-flight work counts",
// Q2 cold spawns still end, Q3 "just allow it" (a run the backend starts by
// itself during the hold runs, and its result reaches the spawner).

// liveHoldAgent is a holdSpawnAgent that reports in-flight work on its
// session (SpawnLiveWorkReporter).
type liveHoldAgent struct {
	*holdSpawnAgent
	live atomic.Bool
}

func (a *liveHoldAgent) SessionLiveWork(string) bool { return a.live.Load() }

// waitUntil polls cond until it holds; the deadline is only a hang guard.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// holdLiveHarness is a persistHarness running one background spawn on a
// holdSpawnAgent, with the branch close hooks recorded.
type holdLiveHarness struct {
	*persistHarness
	id        string
	closed    chan string
	closedNow chan string
}

func startHoldLive(t *testing.T, ag SpawnAgent, started chan struct{}) *holdLiveHarness {
	t.Helper()
	hl := &holdLiveHarness{closed: make(chan string, 4), closedNow: make(chan string, 4)}
	hl.persistHarness = newPersistHarness(ag, func(d *SpawnDeps) {
		d.CloseBranch = func(bk string) { hl.closed <- bk }
		d.CloseBranchNow = func(bk string) { hl.closedNow <- bk }
	})
	hl.id = hl.spawn(t, "deploy the fallback")
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the spawn's first step never started")
	}
	return hl
}

// pause starts the hold and waits until the spawn is paused.
func (hl *holdLiveHarness) pause(t *testing.T, ag *holdSpawnAgent) {
	t.Helper()
	ag.startHold()
	waitUntil(t, "the spawn to pause", ag.anyPaused)
}

// branchRun opens a run on the spawn's branch that the spawn did not start
// (as Agent.OpenAutonomousTurn or an injection turn would) and returns its
// sink.
func (hl *holdLiveHarness) branchRun(t *testing.T, stop func()) turnevent.Sink {
	t.Helper()
	info, ok := hl.tracker.spawnInfo("test/imain", hl.id)
	if !ok {
		t.Fatal("spawn not tracked")
	}
	sink := hl.spawner.autonomousSink("test/imain", info, stop)
	sink.Emit(context.Background(), turnevent.TurnStart{})
	return sink
}

func (hl *holdLiveHarness) logHas(want string) bool {
	snap, ok := hl.tracker.Lookup("test/imain", hl.id, 0)
	if !ok {
		return false
	}
	for _, e := range snap.Log {
		if strings.Contains(e.Text, want) {
			return true
		}
	}
	return false
}

func (hl *holdLiveHarness) running(t *testing.T) bool {
	t.Helper()
	_, ok := hl.tracker.runningSpawn("test/imain", hl.id)
	return ok
}

// spawnResult waits for the next [SPAWN RESULT] delivered to the parent,
// skipping other deliveries (send_to_spawner messages).
func (hl *holdLiveHarness) spawnResult(t *testing.T) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case d := <-hl.deliveries:
			if strings.Contains(d.msg, "[SPAWN RESULT]") {
				d.done(true)
				return d.msg
			}
		case <-deadline:
			t.Fatal("no [SPAWN RESULT] delivered")
			return ""
		}
	}
}

const tookOverNote = "took the spawn over"

// D1/D3: a run its backend started by itself during the hold (Q3) is live at
// the lift. The spawn is not reported ended, though its cache is cold: it
// stays running, send_to_spawner reaches the spawner, it gets no resume turn
// and its process is not closed, and the run's answer is its [SPAWN RESULT].
func TestSpawn_UsageHoldLift_RunOnBranchTakesOver(t *testing.T) {
	t.Parallel()
	ag := newHoldSpawnAgent(false) // cold at the lift
	hl := startHoldLive(t, ag, ag.started)
	hl.pause(t, ag)
	stopped := make(chan struct{}, 1)
	sink := hl.branchRun(t, func() { stopped <- struct{}{} })
	ag.liftHold()

	waitUntil(t, "the run to take the spawn over", func() bool { return hl.logHas(tookOverNote) })
	hl.noDelivery(t)
	if !hl.running(t) {
		t.Fatal("the spawn reads ended while the run on its branch goes on")
	}
	res, err := sendToSpawner(hl.spawner, WithSessionKey(context.Background(), branchKeyOf(t, hl)), "deploy at step 3")
	if err != nil || !strings.Contains(res.Text, "Sent to your spawner") {
		t.Fatalf("send_to_spawner during the run = %q, %v; want it sent", res.Text, err)
	}
	if d := hl.nextDelivery(t); !strings.Contains(d.msg, "deploy at step 3") {
		t.Fatalf("spawner got %q, want the spawn's message", d.msg)
	}

	sink.Emit(context.Background(), turnevent.TextBlock{Text: "deploy done", Phase: turnevent.PhaseIntermediate})
	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "deploy done"})
	msg := hl.spawnResult(t)
	if !strings.Contains(msg, "completed") || !strings.Contains(msg, "deploy done") || strings.Contains(msg, "cold") {
		t.Errorf("result = %q, want the run's answer, completed", msg)
	}
	if hl.running(t) {
		t.Error("the spawn still runs after the run that took it over ended")
	}
	if calls := ag.Calls(); len(calls) != 1 {
		t.Errorf("turns = %q, want no resume turn beside the run", calls)
	}
	hl.noDelivery(t)
	select {
	case bk := <-hl.closedNow:
		t.Errorf("the branch %s was force-closed under its running run", bk)
	default:
	}
	select {
	case <-stopped:
		t.Error("the run was interrupted though nobody stopped it")
	default:
	}
}

// branchKeyOf is the spawn's branch.
func branchKeyOf(t *testing.T, hl *holdLiveHarness) string {
	t.Helper()
	info, ok := hl.tracker.spawnInfo("test/imain", hl.id)
	if !ok {
		t.Fatal("spawn not tracked")
	}
	return info.BranchKey
}

// D1: cold at the lift, but input waits on the branch (Q1: "input queued"
// counts). The spawn is resumed, not ended: the resume line runs, and its
// turn reports the result.
func TestSpawn_UsageHoldLift_ColdWithQueuedInputResumes(t *testing.T) {
	t.Parallel()
	ag := &liveHoldAgent{holdSpawnAgent: newHoldSpawnAgent(false)}
	hl := startHoldLive(t, ag, ag.started)
	hl.pause(t, ag.holdSpawnAgent)
	ag.live.Store(true)
	ag.liftHold()

	msg := hl.spawnResult(t)
	if !strings.Contains(msg, "completed") || strings.Contains(msg, "cold") {
		t.Errorf("result = %q, want the resumed run's completed result", msg)
	}
	calls := ag.Calls()
	if len(calls) != 2 || len(calls[1]) == 0 || calls[1][0] != UsageResumeLine {
		t.Fatalf("turns = %q, want the first step, then the resume line", calls)
	}
}

// D1, Q1: cold at the lift with nothing in flight. The spawn ends with the
// gone-cold result, and its process is closed outright (background work in it
// does not keep it), not left for the idle reaper.
func TestSpawn_UsageHoldLift_ColdIdleClosesProcess(t *testing.T) {
	t.Parallel()
	ag := &liveHoldAgent{holdSpawnAgent: newHoldSpawnAgent(false)}
	hl := startHoldLive(t, ag, ag.started)
	hl.pause(t, ag.holdSpawnAgent)
	ag.liftHold()

	msg := hl.spawnResult(t)
	if !strings.Contains(msg, "not resumed") || !strings.Contains(msg, "cold") {
		t.Errorf("result = %q, want the gone-cold result", msg)
	}
	select {
	case bk := <-hl.closedNow:
		if bk != branchKeyOf(t, hl) {
			t.Errorf("closed %q, want the spawn's branch", bk)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the gone-cold spawn's process was not closed outright")
	}
	select {
	case bk := <-hl.closed:
		t.Errorf("the branch %s also went to the idle-only close", bk)
	default:
	}
	if calls := ag.Calls(); len(calls) != 1 {
		t.Errorf("turns = %q, want no resume turn", calls)
	}
}

// Q3: a run its backend started by itself during the hold that also ended
// during it. At the lift the spawn, cold and idle, ends; its result carries
// what that run ended with, so the answer still reaches the spawner.
func TestSpawn_UsageHoldLift_ColdResultCarriesRunMeanwhile(t *testing.T) {
	t.Parallel()
	ag := newHoldSpawnAgent(false)
	hl := startHoldLive(t, ag, ag.started)
	hl.pause(t, ag)
	sink := hl.branchRun(t, nil)
	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "the background deploy finished: green"})
	hl.noDelivery(t)
	ag.liftHold()

	msg := hl.spawnResult(t)
	if !strings.Contains(msg, "cold") || !strings.Contains(msg, "the background deploy finished: green") {
		t.Errorf("result = %q, want the gone-cold result with the run's answer", msg)
	}
}

// D3 outside a hold: the spawn's own run ends while a run its backend started
// by itself goes on. The spawn's answer is delivered, the spawn stays
// running (send_to_spawner and spawn_status treat it as live), and the run's
// answer follows as one more [SPAWN RESULT].
func TestSpawnBranchTurn_RunOutlivingSpawnTakesItOver(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(1)
	hl := &holdLiveHarness{persistHarness: newPersistHarness(agent, nil)}
	hl.id = hl.spawn(t, "run the suite")
	waitStarted(t, agent, 0)
	sink := hl.branchRun(t, nil)
	close(agent.gates[0])

	if msg := hl.spawnResult(t); !strings.Contains(msg, "result 0: run the suite") {
		t.Errorf("first result = %q, want the spawn's own answer", msg)
	}
	waitUntil(t, "the run to take the spawn over", func() bool { return hl.logHas(tookOverNote) })
	if !hl.running(t) {
		t.Fatal("the spawn reads ended while the run on its branch goes on")
	}
	params, _ := json.Marshal(map[string]string{"id": hl.id})
	if st, err := hl.spawner.StatusTool().Execute(hl.ctx, params); err != nil || !strings.Contains(st.Text, "running") {
		t.Errorf("spawn_status = %q, %v; want running", st.Text, err)
	}
	if _, err := sendToSpawner(hl.spawner, WithSessionKey(context.Background(), branchKeyOf(t, hl)), "still at it"); err != nil {
		t.Errorf("send_to_spawner during the run: %v", err)
	}
	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "lint passed too"})
	if msg := hl.spawnResult(t); !strings.Contains(msg, "lint passed too") {
		t.Errorf("second result = %q, want the run's answer", msg)
	}
	if hl.running(t) {
		t.Error("the spawn still runs after the run that took it over ended")
	}
}

// D4: the spawn a run took over at the lift is still running in its persisted
// record, so a foci restart while that run goes on resumes the spawn (#2589)
// instead of leaving it ended with no report.
func TestSpawn_UsageHoldLift_TakenOverSpawnResumedAfterRestart(t *testing.T) {
	t.Parallel()
	ag := newHoldSpawnAgent(false)
	hl := startHoldLive(t, ag, ag.started)
	hl.pause(t, ag)
	hl.branchRun(t, nil)
	ag.liftHold()
	waitUntil(t, "the run to take the spawn over", func() bool { return hl.logHas(tookOverNote) })
	if rec, ok := hl.store.record(t, hl.id); !ok || !rec.Running {
		t.Fatalf("persisted record = %+v (ok %v), want it running", rec, ok)
	}

	// The restart: a new process reads the same store.
	next := newScriptedSpawnAgent(0)
	h2 := newPersistHarness(next, nil)
	h2.store = hl.store
	h2.tracker.SetStore(hl.store)
	h2.spawner.RestoreSpawns()
	waitStarted(t, next, 0)
	calls, _ := next.turns()
	if len(calls[0]) == 0 || !strings.Contains(calls[0][0], "FOCI RESTARTED") {
		t.Fatalf("resume turn = %q, want the restart notice", calls[0])
	}
}
