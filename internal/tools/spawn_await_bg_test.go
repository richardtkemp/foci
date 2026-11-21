package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/turnevent"
)

// awaitHarness is an autonomousHarness whose branch backend reports the
// background work the test sets (SpawnDeps.BackgroundWork), and records the
// branches closed at once (CloseBranchNow) (#2728).
type awaitHarness struct {
	*followupHarness
	mu       sync.Mutex
	work     string
	closedAt []string
}

func (h *awaitHarness) setWork(w string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.work = w
}

func (h *awaitHarness) closedNow() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.closedAt...)
}

func newAwaitHarness(t *testing.T, agentID string, agent SpawnAgent) (*awaitHarness, string) {
	t.Helper()
	ah := &awaitHarness{followupHarness: &followupHarness{tracker: NewSpawnTracker(), delivered: make(chan string, 8), obs: &recordingSpawnObserver{}}}
	notifier := NewAsyncNotifier(func(sk, msg, _, _ string, _ *AgentOrigin) {
		ah.delivered <- sk + "\n" + msg
	})
	ah.spawner = NewSpawner(SpawnDeps{
		Sessions:   &mockSessionBrancher{},
		AgentID:    agentID,
		MaxInherit: 2,
		Notifier:   notifier,
		Tracker:    ah.tracker,
		Observer:   ah.obs,
		BackgroundWork: func(string) string {
			ah.mu.Lock()
			defer ah.mu.Unlock()
			return ah.work
		},
		CloseBranchNow: func(bk string) {
			ah.mu.Lock()
			defer ah.mu.Unlock()
			ah.closedAt = append(ah.closedAt, bk)
		},
	}, func() SpawnAgent { return agent })
	ah.spawner.awaitPoll = 5 * time.Millisecond
	registerTestSpawner(t, ah.spawner)
	parent := agentID + "/imain"
	ah.ctx = WithSessionKey(context.Background(), parent)
	return ah, parent
}

// waitAwaiting waits until spawn_status shows id waiting on its background
// work: its run has ended and been kept running.
func (h *awaitHarness) waitAwaiting(t *testing.T, id string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := h.status(t, id)
		if strings.Contains(st, "waits on work it started in the background") {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("spawn never waited on its background work; status:\n%s", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (o *recordingSpawnObserver) counts() (started, ended int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.started), len(o.ended)
}

// TestSpawnAwait_TurnEndWithBackgroundWorkWithholdsResult is #2728: a spawn
// whose turn ends while a background command it started still runs delivers
// no [SPAWN RESULT] (its text is interim) and stays running in spawn_status
// and the app. When its backend wakes on the command's end, that run's answer
// is its one result.
func TestSpawnAwait_TurnEndWithBackgroundWorkWithholdsResult(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(0)
	h, parent := newAwaitHarness(t, "await2728a", agent)
	h.setWork("1 background command: make test-mpp")
	id := h.start(t, "run the suite")
	waitStarted(t, agent, 0)

	st := h.waitAwaiting(t, id)
	if !strings.Contains(st, "running") || !strings.Contains(st, "make test-mpp") {
		t.Errorf("status while waiting = %q, want running and the work named", st)
	}
	h.noResult(t)
	if _, ended := h.obs.counts(); ended != 0 {
		t.Errorf("the app was told the spawn ended (%d) while its background work runs", ended)
	}

	// The command ends: the backend wakes on the branch.
	sink, ok := SpawnBranchTurn(parent+"/b1000000000", nil)
	if !ok {
		t.Fatal("the woken run on the waiting spawn's branch was not claimed")
	}
	ctx := context.Background()
	sink.Emit(ctx, turnevent.TurnStart{})
	h.setWork("")
	sink.Emit(ctx, turnevent.TurnComplete{FinalText: "the suite passed: 412 tests"})

	msg := h.resultFor(t, parent)
	if !strings.Contains(msg, "the suite passed: 412 tests") {
		t.Errorf("result = %q, want the woken run's answer", msg)
	}
	if strings.Contains(msg, "result 0:") {
		t.Errorf("result = %q carries the interim text of the turn that ended", msg)
	}
	h.noResult(t)
	if st := h.status(t, id); !strings.Contains(st, "completed") {
		t.Errorf("status after the hand-back = %q, want completed", st)
	}
	started, ended := h.obs.counts()
	if started != 1 || ended != 1 {
		t.Errorf("app saw %d starts and %d ends, want one entry from start to hand-back", started, ended)
	}
}

// TestSpawnAwait_WorkEndsWithoutWake: the work ends and nothing wakes the
// spawn (its process closed, the work was killed). Its kept text goes as the
// result, with a note that the work ended with no answer after it.
func TestSpawnAwait_WorkEndsWithoutWake(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(0)
	h, parent := newAwaitHarness(t, "await2728b", agent)
	h.setWork("1 background command: sleep 600")
	id := h.start(t, "run the suite")
	waitStarted(t, agent, 0)
	h.waitAwaiting(t, id)
	h.noResult(t)

	h.setWork("")
	msg := h.resultFor(t, parent)
	if !strings.Contains(msg, "result 0: run the suite") || !strings.Contains(msg, spawnAwaitEndedNote) {
		t.Errorf("result = %q, want the kept text and the no-answer note", msg)
	}
	if st := h.status(t, id); !strings.Contains(st, "completed") {
		t.Errorf("status = %q, want completed", st)
	}
}

// TestSpawnAwait_StopEndsWork: spawn_stop on a waiting spawn closes its
// branch's backend at once (which ends the work) and reports it stopped.
func TestSpawnAwait_StopEndsWork(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(0)
	h, parent := newAwaitHarness(t, "await2728c", agent)
	h.setWork("1 background command: sleep 600")
	id := h.start(t, "run the suite")
	waitStarted(t, agent, 0)
	h.waitAwaiting(t, id)

	params, _ := json.Marshal(map[string]string{"id": id})
	if res, err := h.spawner.StopTool().Execute(h.ctx, params); err != nil || !strings.Contains(res.Text, "Stopping") {
		t.Fatalf("spawn_stop = %q, %v", res.Text, err)
	}
	msg := h.resultFor(t, parent)
	if !strings.Contains(msg, "was stopped") {
		t.Errorf("result = %q, want it stopped", msg)
	}
	if got := h.closedNow(); len(got) != 1 || got[0] != parent+"/b1000000000" {
		t.Errorf("branches closed at once = %v, want its branch (its work ends with it)", got)
	}
	if st := h.status(t, id); !strings.Contains(st, "stopped") {
		t.Errorf("status = %q, want stopped", st)
	}
}

// TestSpawnAwait_SendTakesOverAndWaitsAgain: a spawn_send to a waiting spawn
// runs as a new turn at once (the work goes on); that turn ends with the
// work still running, so it waits again, and its own text is the result
// once the work ends. The first turn's interim text is never delivered.
func TestSpawnAwait_SendTakesOverAndWaitsAgain(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(0)
	h, parent := newAwaitHarness(t, "await2728d", agent)
	h.setWork("1 background command: sleep 600")
	id := h.start(t, "run the suite")
	waitStarted(t, agent, 0)
	h.waitAwaiting(t, id)

	if got := h.send(t, id, "also lint it"); !strings.Contains(got, "waiting on work it started in the background") {
		t.Errorf("send to the waiting spawn = %q", got)
	}
	waitStarted(t, agent, 1)
	h.waitAwaiting(t, id)
	h.noResult(t)

	h.setWork("")
	msg := h.resultFor(t, parent)
	if !strings.Contains(msg, "result 1: "+spawnMessagePrefix+"also lint it") {
		t.Errorf("result = %q, want the follow-up turn's text", msg)
	}
	if strings.Contains(msg, "result 0:") {
		t.Errorf("result = %q carries the first turn's interim text", msg)
	}
	h.noResult(t)
	started, ended := h.obs.counts()
	if started != 1 || ended != 1 {
		t.Errorf("app saw %d starts and %d ends, want one entry", started, ended)
	}
}

// TestSpawnAwait_WokenRunWaitsAgain: the run the backend wakes for ends its
// turn with work still running (it started more): it delivers nothing yet,
// and the spawn keeps waiting (the #3038 run of "Waiting." results).
func TestSpawnAwait_WokenRunWaitsAgain(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(0)
	h, parent := newAwaitHarness(t, "await2728e", agent)
	h.setWork("1 background command: sleep 600")
	id := h.start(t, "run the suite")
	waitStarted(t, agent, 0)
	h.waitAwaiting(t, id)

	sink, _ := SpawnBranchTurn(parent+"/b1000000000", nil)
	sink.Emit(context.Background(), turnevent.TurnStart{})
	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "Old event; waiting."})
	h.waitAwaiting(t, id)
	h.noResult(t)

	h.setWork("")
	msg := h.resultFor(t, parent)
	if !strings.Contains(msg, "Old event; waiting.") || !strings.Contains(msg, spawnAwaitEndedNote) {
		t.Errorf("result = %q, want the woken run's text and the no-answer note", msg)
	}
	h.noResult(t)
}

// TestSpawnAwait_NoWorkDeliversAtOnce: a turn that ends with no background
// work delivers its result as before.
func TestSpawnAwait_NoWorkDeliversAtOnce(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(0)
	h, parent := newAwaitHarness(t, "await2728f", agent)
	h.start(t, "run the suite")
	waitStarted(t, agent, 0)
	if msg := h.resultFor(t, parent); !strings.Contains(msg, "result 0: run the suite") || strings.Contains(msg, spawnAwaitEndedNote) {
		t.Errorf("result = %q, want the turn's text alone", msg)
	}
}

// TestSpawnAwait_FociBackgroundToolCall: foci's own background work on the
// branch (a background tool call's pending result) holds the result too
// (Dick 2026-11-01 16:23), and its end releases it.
func TestSpawnAwait_FociBackgroundToolCall(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(0)
	h, parent := newAwaitHarness(t, "await2728g", agent)
	branchKey := parent + "/b1000000000"
	h.spawner.deps.Notifier.MarkPending(branchKey)
	id := h.start(t, "fetch it")
	waitStarted(t, agent, 0)
	if st := h.waitAwaiting(t, id); !strings.Contains(st, "foci background tool call") {
		t.Errorf("status = %q, want the tool call named", st)
	}
	h.noResult(t)
	h.spawner.deps.Notifier.MarkDone(branchKey)
	if msg := h.resultFor(t, parent); !strings.Contains(msg, "result 0: fetch it") {
		t.Errorf("result = %q", msg)
	}
}
