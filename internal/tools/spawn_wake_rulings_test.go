package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"foci/internal/session"
)

// Dick's rulings on #3163 (2026-11-18 14:53 and after): a stopped parent is
// not woken, its children's reports pass up; a finished spawn stays known
// while its own spawns run; the wake queue is persisted ("Persist the
// queue"), so a restart resumes the ended parent with what was waiting.

// A parent spawn stopped by spawn_stop is not woken by a late child: the
// child's message and result go to the stopped spawn's own spawner (the
// grandparent), each headed by a note naming the child and the stopped spawn.
func TestChildReportToStoppedParentSpawnPassesUp(t *testing.T) {
	t.Parallel()
	agent := newTreeAgent()
	agent.parentGate = make(chan struct{}) // the parent's first turn holds until stopped
	agent.message = func(string) string { return "row 7 is wrong" }
	h := newWakeHarnessWith(t, 1, agent)
	<-h.agent.parentTurnCh
	parentID := h.s.deps.Tracker.List(wakeGrandparent)[0].ID
	gctx := WithSessionKey(t.Context(), wakeGrandparent)
	if _, err := h.s.StopTool().Execute(gctx, json.RawMessage(`{"id":"`+parentID+`"}`)); err != nil {
		t.Fatal(err)
	}
	if d := h.next(t, "the stopped parent's result"); d.session != wakeGrandparent || !strings.Contains(d.msg, "was stopped by") {
		t.Fatalf("first delivery = %q to %q, want the parent's stopped [SPAWN RESULT] at the grandparent", d.msg, d.session)
	}

	close(h.agent.release)
	var gotMsg, gotResult bool
	for !gotMsg || !gotResult {
		d := h.next(t, "the child's reports, passed up")
		if d.session != wakeGrandparent {
			t.Fatalf("delivery went to %q (%q), want the grandparent", d.session, d.msg)
		}
		if !strings.Contains(d.msg, "[FROM A SPAWN OF YOUR STOPPED SPAWN]") || !strings.Contains(d.msg, parentID) {
			t.Fatalf("delivery to the grandparent lacks the pass-up note naming the stopped parent %s:\n%s", parentID, d.msg)
		}
		switch {
		case strings.Contains(d.msg, "row 7 is wrong"):
			gotMsg = true
		case strings.Contains(d.msg, "child done on "+h.parentKey+"/b"):
			gotResult = true
		default:
			t.Fatalf("unexpected delivery to the grandparent (the stopped parent was woken?):\n%s", d.msg)
		}
	}
	if in := h.agent.allParentInput(); strings.Contains(in, "RESUMED BY YOUR SPAWN") {
		t.Errorf("the stopped parent was woken; its input:\n%s", in)
	}
}

// A finished spawn whose own spawn still runs (or holds a result for it) is
// not pruned by maxFinishedSpawns; once its spawns are done, it can be.
func TestFinishedSpawnWithRunningChildIsNotPruned(t *testing.T) {
	t.Parallel()
	tr := NewSpawnTracker()
	const gp, parentBranch = "prune/imain", "prune/imain/b1"
	base := time.Now().Add(-time.Hour)
	tr.mu.Lock()
	tr.spawns[gp] = map[string]*trackedSpawn{
		"spawn-p": {info: RunningSpawn{ID: "spawn-p", BranchKey: parentBranch}, ended: base},
	}
	for i := range maxFinishedSpawns + 5 {
		id := fmt.Sprintf("spawn-old%d", i)
		tr.spawns[gp][id] = &trackedSpawn{info: RunningSpawn{ID: id, BranchKey: gp + "/x" + id}, ended: base.Add(time.Duration(i+1) * time.Minute)}
	}
	child := &trackedSpawn{info: RunningSpawn{ID: "spawn-c", BranchKey: parentBranch + "/b2"}, running: true}
	tr.spawns[parentBranch] = map[string]*trackedSpawn{"spawn-c": child}
	tr.pruneFinished(gp)
	_, kept := tr.spawns[gp]["spawn-p"]
	tr.mu.Unlock()
	if !kept {
		t.Fatal("the oldest finished spawn was pruned while its own spawn still runs: that spawn's result would find no parent")
	}

	// Its child finished and its result went: it is an ordinary finished
	// spawn again, and the oldest.
	tr.mu.Lock()
	child.running = false
	tr.pruneFinished(gp)
	_, kept = tr.spawns[gp]["spawn-p"]
	n := len(tr.spawns[gp])
	tr.mu.Unlock()
	if kept || n != maxFinishedSpawns {
		t.Errorf("after its child ended: parent kept=%v, %d known, want pruned and %d known", kept, n, maxFinishedSpawns)
	}
}

// copyStore is a new store with m's rows: what the next process reads after
// a crash.
func copyStore(m *memSpawnStore) *memSpawnStore {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := newMemSpawnStore()
	for a, rows := range m.rows {
		out.rows[a] = map[string]string{}
		for k, v := range rows {
			out.rows[a][k] = v
		}
	}
	return out
}

// putAgent writes rec as agentID's row.
func (m *memSpawnStore) putAgent(t *testing.T, agentID string, rec spawnRecord) {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.SetAgentMetadata(agentID, spawnMetaPrefix+rec.ID, string(b))
}

// wakeRecord reads spawn id's persisted row of agent "wake".
func wakeRecord(t *testing.T, m *memSpawnStore, id string) (spawnRecord, bool) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.rows["wake"][spawnMetaPrefix+id]
	if !ok {
		return spawnRecord{}, false
	}
	var rec spawnRecord
	if err := json.Unmarshal([]byte(v), &rec); err != nil {
		t.Fatal(err)
	}
	return rec, true
}

func waitWake(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// restartWake is the next process on store: the same agent and branches, and
// the persisted spawns restored.
func restartWake(store *memSpawnStore, parentKey string) *wakeHarness {
	h := &wakeHarness{agent: newTreeAgent(), delivered: make(chan injected, 1024), parentKey: parentKey}
	h.agent.parentKey = parentKey
	newWakeSpawner(h, 1, store)
	h.s.RestoreSpawns()
	return h
}

// A child's result still undelivered in its record when foci restarts is
// redelivered by the next process. Its parent spawn had ended; it stayed
// persisted because its child still ran, so the redelivery takes the wake
// path: the parent is resumed with the result, and its new result reaches
// the grandparent.
func TestRestartRedeliveredChildResultWakesEndedParent(t *testing.T) {
	t.Parallel()
	store := newMemSpawnStore()
	h := newWakeHarnessStore(t, 1, newTreeAgent(), store)
	t.Cleanup(func() { close(h.agent.release) })
	<-h.agent.parentTurnCh
	h.waitParentEnded(t)
	parentID := h.s.deps.Tracker.List(wakeGrandparent)[0].ID
	child := h.s.deps.Tracker.List(h.parentKey)[0]
	// The parent's result has gone: its row is settled (kept, as its child
	// runs; the old code deleted it).
	waitWake(t, "the ended parent's row settled", func() bool {
		rec, ok := wakeRecord(t, store, parentID)
		return !ok || (!rec.Running && rec.Result == "")
	})

	// The crash: the child had finished and recorded its result, which had
	// not reached the parent yet.
	next := copyStore(store)
	rec, ok := wakeRecord(t, next, child.ID)
	if !ok {
		t.Fatal("no row for the running child")
	}
	rec.Running = false
	rec.Result = spawnResultHead(child.Label(), child.BranchKey, child.ID) + " completed:\n\nchild done on " + child.BranchKey
	next.putAgent(t, "wake", rec)

	h2 := restartWake(next, h.parentKey)
	d := h2.next(t, "the woken parent's result after the restart")
	if d.session != wakeGrandparent {
		t.Fatalf("delivery went to %q (%q), want the grandparent: the redelivered result took the old path into the ended parent's session", d.session, d.msg)
	}
	for _, want := range []string{"[SPAWN RESULT]", "[SYSTEM: RESUMED BY YOUR SPAWN]", "child done on " + child.BranchKey} {
		if !strings.Contains(d.msg, want) {
			t.Errorf("the parent's result after the restart lacks %q:\n%s", want, d.msg)
		}
	}
}

// The wake queue is persisted (Dick 2026-11-18: "Persist the queue"): a
// child's message and result that wait to wake an ended parent survive a
// crash, and the next process resumes the parent with them, in arrival order.
func TestRestartResumesEndedParentWithPersistedWakeQueue(t *testing.T) {
	t.Parallel()
	store := newMemSpawnStore()
	agent := newTreeAgent()
	agent.message = func(string) string { return "row 7 is wrong" }
	h := newWakeHarnessStore(t, 1, agent, store)
	t.Cleanup(func() { h.s.slots.setLimit(10) })
	<-h.agent.parentTurnCh
	h.waitParentEnded(t)
	parentID := h.s.deps.Tracker.List(wakeGrandparent)[0].ID
	// No slot frees up for the wake: the reports wait in the queue.
	h.s.slots.setLimit(0)
	close(h.agent.release)
	waitWake(t, "the child's message and result in the parent's wake queue", func() bool {
		tr := h.s.deps.Tracker
		tr.mu.Lock()
		defer tr.mu.Unlock()
		return len(tr.spawns[wakeGrandparent][parentID].wake) == 2
	})

	h2 := restartWake(copyStore(store), h.parentKey)
	d := h2.next(t, "the parent's result after the restart")
	if d.session != wakeGrandparent {
		t.Fatalf("delivery went to %q (%q), want the grandparent", d.session, d.msg)
	}
	if !strings.Contains(d.msg, "[SYSTEM: RESUMED BY YOUR SPAWN]") {
		t.Errorf("the parent's result does not show it was resumed by its spawn:\n%s", d.msg)
	}
	in := h2.agent.allParentInput()
	m, r := strings.Index(in, "row 7 is wrong"), strings.Index(in, "child done on ")
	if m < 0 || r < 0 || m > r {
		t.Errorf("the resumed parent's input has the message at %d and the result at %d, want both, message first:\n%s", m, r, in)
	}
	select {
	case d := <-h2.delivered:
		t.Errorf("a second delivery after the restart (%q to %q): a report was delivered twice", d.msg, d.session)
	case <-time.After(200 * time.Millisecond):
	}
}

// The ticket's chain: the agent cam spawns a spawn run as the agent codex
// (cross-agent, on codex's ixspawn root), and that spawn's own children run
// on codex. The parent's record is under cam (its caller's agent), the
// children's under codex. A child's result left undelivered by a crash is
// redelivered by codex's restore; when codex restores first, it must still
// find the ended parent and wake it (AdoptSpawns runs for every agent
// before any RestoreSpawns), not take the old path into the parent's
// branch session.
func TestRestartCrossAgentChainWakesParentWhateverRestoreOrder(t *testing.T) {
	t.Parallel()
	camID, codexID := "cam3163", "codex3163"
	parentKey := session.CrossAgentSpawnRoot(codexID) + "/b1795000626"
	camChat := camID + "/imain"
	store := newMemSpawnStore()
	now := time.Now()
	store.putAgent(t, camID, spawnRecord{ID: "spawn-parent", Parent: camChat, BranchKey: parentKey, Name: "review",
		Prompt: "review rows", Started: now.Add(-time.Hour), Character: true, Agent: codexID})
	childKey := parentKey + "/b1795000700"
	store.putAgent(t, codexID, spawnRecord{ID: "spawn-child", Parent: parentKey, BranchKey: childKey, Name: "kid0",
		Prompt: "check row 0", Started: now.Add(-30 * time.Minute),
		Result: spawnResultHead("kid0", childKey, "spawn-child") + " completed:\n\nchild done on " + childKey})

	delivered := make(chan injected, 16)
	notifier := func() *AsyncNotifier {
		return NewAsyncNotifier(func(sk, msg, replyTo, _ string, origin *AgentOrigin) {
			delivered <- injected{sk, msg, replyTo, origin}
		})
	}
	tr := NewSpawnTracker()
	tr.SetStore(store)
	agent := newTreeAgent()
	agent.parentKey = parentKey
	cam := NewSpawner(SpawnDeps{AgentID: camID, MaxInherit: 4, Notifier: notifier(), Tracker: tr},
		func() SpawnAgent { return newTreeAgent() })
	codex := NewSpawner(SpawnDeps{AgentID: codexID, MaxInherit: 4, Notifier: notifier(), Tracker: tr},
		func() SpawnAgent { return agent })
	agent.spawner = func() *Spawner { return codex }
	registerTestSpawner(t, cam)
	registerTestSpawner(t, codex)

	// The gateway's boot: each phase across every agent, codex first.
	order := []*Spawner{codex, cam}
	for _, s := range order {
		s.AdoptSpawns()
	}
	for _, s := range order {
		s.RestoreSpawns()
	}

	select {
	case d := <-delivered:
		if d.session != camChat {
			t.Fatalf("delivery went to %q (%q), want Cam's chat %q: the child's result took the old path into the ended parent's branch",
				d.session, d.msg, camChat)
		}
		for _, want := range []string{"[SPAWN RESULT]", "[SYSTEM: RESUMED BY YOUR SPAWN]", "child done on " + childKey} {
			if !strings.Contains(d.msg, want) {
				t.Errorf("the parent's result after the restart lacks %q:\n%s", want, d.msg)
			}
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no delivery after the restart")
	}
}
