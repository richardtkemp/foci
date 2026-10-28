package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// reparentHarness joins a depth tree and a spawner on one store, wired as the
// gateway wires them (#2624): the spawner routes through the depth manager,
// and each handover point releases what it held.
type reparentHarness struct {
	depth *depthHarness
	sp    *persistHarness
}

func newReparentHarness(t *testing.T, agent SpawnAgent, store *memSpawnStore) *reparentHarness {
	t.Helper()
	h := &reparentHarness{}
	h.sp = newPersistHarness(agent, nil)
	if store != nil {
		h.sp.store = store
		h.sp.tracker.SetStore(store)
	}
	h.depth = newDepthHarness(t, h.sp.store)
	h.sp.spawner.deps.Route = h.depth.m.SpawnRoute
	h.depth.m.deps.OnHandover = h.sp.spawner.ReleaseHeld
	return h
}

// spawnFrom starts a background clone spawn from session key.
func (h *reparentHarness) spawnFrom(t *testing.T, key, prompt string) string {
	t.Helper()
	h.sp.ctx = WithSessionKey(h.sp.ctx, key)
	return h.sp.spawn(t, prompt)
}

// waitResult waits until spawn id's result is recorded (delivered or held).
func waitResult(t *testing.T, store *memSpawnStore, id string) spawnRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rec, ok := store.record(t, id); ok && rec.Result != "" {
			return rec
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("spawn %s recorded no result", id)
	return spawnRecord{}
}

func (h *reparentHarness) status(t *testing.T, key, id string) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"id": id})
	res, err := h.sp.spawner.StatusTool().Execute(WithSessionKey(h.sp.ctx, key), raw)
	return res.Text, err
}

// A leaf may report while its spawn runs. The spawn's result is then held
// until the leaf's report reaches its parent's fan-in, and only after that
// fan-in is it delivered, to the parent, saying which leaf started it. The
// new owner can check on it with spawn_status.
func TestReportedLeafSpawnHeldUntilFanIn(t *testing.T) {
	agent := newScriptedSpawnAgent(1)
	h := newReparentHarness(t, agent, nil)
	kids := h.depth.fanout(t, depthRoot, "Alpha", "Beta")
	id := h.spawnFrom(t, kids[0], "dig into alpha")
	<-agent.started

	if err := h.depth.report(t, kids[0], "alpha settled"); err != nil {
		t.Fatalf("a leaf with a running spawn must be able to report: %v", err)
	}
	close(agent.gates[0])
	rec := waitResult(t, h.sp.store, id)
	if rec.Parent != kids[0] || !strings.Contains(rec.Result, "[SPAWN RESULT]") {
		t.Fatalf("record = %+v", rec)
	}
	// Held: the leaf has reported but the root's fan-in waits on Beta.
	h.sp.noDelivery(t)
	if _, err := h.status(t, depthRoot, id); err == nil {
		t.Error("the root owns the spawn before it received the leaf's report")
	}

	if err := h.depth.report(t, kids[1], "beta settled"); err != nil {
		t.Fatal(err)
	}
	if ds := h.depth.deliveries(); len(ds) != 1 || ds[0].trigger != "depth_results" {
		t.Fatalf("fan-in deliveries = %+v", ds)
	}
	d := h.sp.nextDelivery(t)
	if d.session != depthRoot {
		t.Fatalf("held result went to %s, want the root %s", d.session, depthRoot)
	}
	for _, want := range []string{"[REPARENTED SPAWN]", `"Alpha"`, kids[0], rec.Result} {
		if !strings.Contains(d.msg, want) {
			t.Errorf("delivered result lacks %q:\n%s", want, d.msg)
		}
	}
	if txt, err := h.status(t, depthRoot, id); err != nil || !strings.Contains(txt, id) {
		t.Errorf("spawn_status from the new owner = %q, %v", txt, err)
	}
	d.done(true)
	if _, ok := h.sp.store.record(t, id); ok {
		t.Error("record kept after the new owner took the result")
	}
}

// Ownership walks up node by node: a spawn whose leaf, and that leaf's
// parent, have both reported and been fanned in goes to the root chat.
func TestReparentedSpawnWalksToRoot(t *testing.T) {
	agent := newScriptedSpawnAgent(1)
	h := newReparentHarness(t, agent, nil)
	a := h.depth.fanout(t, depthRoot, "Alpha")[0]
	a1 := h.depth.fanout(t, a, "Alpha one")[0]
	id := h.spawnFrom(t, a1, "deep work")
	<-agent.started

	if err := h.depth.report(t, a1, "a1 done"); err != nil {
		t.Fatal(err)
	}
	if err := h.depth.report(t, a, "a done"); err != nil {
		t.Fatal(err)
	}
	if r := h.depth.m.SpawnRoute(a1); r.Owner != depthRoot || r.Hold {
		t.Fatalf("route = %+v, want the root, not held", r)
	}
	close(agent.gates[0])
	d := h.sp.nextDelivery(t)
	if d.session != depthRoot || !strings.Contains(d.msg, `"Alpha one"`) {
		t.Fatalf("delivered to %s: %q", d.session, d.msg)
	}
	d.done(true)
	_ = id
}

// A held result survives a restart: the next process restores it held, and
// delivers it after the fan-in the last process never reached.
func TestHeldSpawnResultSurvivesRestart(t *testing.T) {
	agent := newScriptedSpawnAgent(1)
	h := newReparentHarness(t, agent, nil)
	kids := h.depth.fanout(t, depthRoot, "Alpha", "Beta")
	id := h.spawnFrom(t, kids[0], "dig")
	<-agent.started
	if err := h.depth.report(t, kids[0], "alpha settled"); err != nil {
		t.Fatal(err)
	}
	close(agent.gates[0])
	waitResult(t, h.sp.store, id)
	h.sp.noDelivery(t)

	// The next process: spawns restore before the trees (main.go order).
	h2 := newReparentHarness(t, newScriptedSpawnAgent(0), h.sp.store)
	h2.sp.spawner.RestoreSpawns()
	h2.sp.noDelivery(t)
	h2.depth.m.Restore()
	h2.sp.noDelivery(t)

	if err := h2.depth.report(t, kids[1], "beta settled"); err != nil {
		t.Fatal(err)
	}
	d := h2.sp.nextDelivery(t)
	if d.session != depthRoot || !strings.Contains(d.msg, "[REPARENTED SPAWN]") {
		t.Fatalf("delivered to %s: %q", d.session, d.msg)
	}
	d.done(true)
	if _, ok := h2.sp.store.record(t, id); ok {
		t.Error("record kept after delivery")
	}
}

// A spawn of an open leaf is untouched: its result goes to the leaf.
func TestOpenLeafSpawnDeliversToLeaf(t *testing.T) {
	agent := newScriptedSpawnAgent(0)
	h := newReparentHarness(t, agent, nil)
	kids := h.depth.fanout(t, depthRoot, "Alpha")
	h.spawnFrom(t, kids[0], "quick")
	d := h.sp.nextDelivery(t)
	if d.session != kids[0] || strings.Contains(d.msg, "[REPARENTED SPAWN]") {
		t.Fatalf("delivered to %s: %q", d.session, d.msg)
	}
	d.done(true)
}
