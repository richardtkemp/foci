package tools

import (
	"slices"
	"strings"
	"sync"
	"testing"
)

// ownerEventObserver records every SpawnObserver event with the session it
// was told for, as "<kind> <session> <id>[ <text>]", in order (#3206).
type ownerEventObserver struct {
	mu     sync.Mutex
	events []string
}

func (o *ownerEventObserver) add(kind, session, id, text string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	e := kind + " " + session + " " + id
	if text != "" {
		e += " " + text
	}
	o.events = append(o.events, e)
}

func (o *ownerEventObserver) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.events)
}

func (o *ownerEventObserver) SpawnStarted(parent string, s RunningSpawn, prompt string) {
	o.add("started", parent, s.ID, prompt)
}
func (o *ownerEventObserver) SpawnText(parent, id, text string) { o.add("text", parent, id, text) }
func (o *ownerEventObserver) SpawnTool(parent, id, _ string)    { o.add("tool", parent, id, "") }
func (o *ownerEventObserver) SpawnPrompt(parent, id, m string)  { o.add("prompt", parent, id, m) }
func (o *ownerEventObserver) SpawnModel(parent, id, m string)   { o.add("model", parent, id, m) }
func (o *ownerEventObserver) SpawnRetry(parent, id string, _ int64, _ int) {
	o.add("retry", parent, id, "")
}
func (o *ownerEventObserver) SpawnEnded(parent, id string) { o.add("ended", parent, id, "") }

// eventsFor keeps the events of one kind, as "<session> [text]".
func eventsFor(events []string, kind, id string) []string {
	var out []string
	for _, e := range events {
		rest, ok := strings.CutPrefix(e, kind+" ")
		if !ok {
			continue
		}
		session, after, _ := strings.Cut(rest, " ")
		gotID, text, _ := strings.Cut(after, " ")
		if gotID != id {
			continue
		}
		out = append(out, strings.TrimSpace(session+" "+text))
	}
	return out
}

// newOwnerHarness is newReparentHarness with an observer on the spawner.
func newOwnerHarness(t *testing.T, agent SpawnAgent, store *memSpawnStore, obs SpawnObserver) *reparentHarness {
	t.Helper()
	h := &reparentHarness{}
	h.sp = newPersistHarness(agent, func(d *SpawnDeps) { d.Observer = obs })
	if store != nil {
		h.sp.store = store
		h.sp.tracker.SetStore(store)
	}
	h.depth = newDepthHarness(t, h.sp.store)
	h.sp.spawner.deps.Route = h.depth.m.SpawnRoute
	h.depth.m.deps.OnHandover = h.sp.spawner.Reown
	return h
}

func runningIDs(tr *SpawnTracker, session string) []string {
	var ids []string
	for _, r := range tr.Running(session) {
		ids = append(ids, r.ID)
	}
	return ids
}

// #3206: once a reported leaf's spawn has passed to its parent, every surface
// shows it there and not in the leaf: the [running] line's list
// (SpawnTracker.Running), spawn_status's list, the app (its entry moves from
// the leaf's chat to the parent's), /stop and the app's per-item stop.
// During the hold (report sent, fan-in not taken) all of them still show it
// in the leaf, as #2624's results do.
func TestHandedOverSpawnFollowsOwnerOnEverySurface(t *testing.T) {
	agent := newScriptedSpawnAgent(1)
	obs := &ownerEventObserver{}
	h := newOwnerHarness(t, agent, nil, obs)
	tr := h.sp.tracker
	kids := h.depth.fanout(t, depthRoot, "Alpha", "Beta")
	id := h.spawnFrom(t, kids[0], "dig into alpha")
	<-agent.started

	check := func(phase, owner, other string) {
		t.Helper()
		if got := runningIDs(tr, owner); !slices.Equal(got, []string{id}) {
			t.Errorf("%s: Running(%s) = %v, want [%s]", phase, owner, got, id)
		}
		if got := runningIDs(tr, other); len(got) != 0 {
			t.Errorf("%s: Running(%s) = %v, want none", phase, other, got)
		}
		if l := h.sp.spawner.listForCaller(owner); len(l) != 1 || l[0].ID != id {
			t.Errorf("%s: spawn_status list of %s = %+v", phase, owner, l)
		}
		if l := h.sp.spawner.listForCaller(other); len(l) != 0 {
			t.Errorf("%s: spawn_status list of %s = %+v, want none", phase, other, l)
		}
	}
	check("open leaf", kids[0], depthRoot)

	if err := h.depth.report(t, kids[0], "alpha settled"); err != nil {
		t.Fatal(err)
	}
	check("held", kids[0], depthRoot)
	if got := eventsFor(obs.snapshot(), "started", id); len(got) != 1 || !strings.HasPrefix(got[0], kids[0]+" ") {
		t.Fatalf("held: started events = %q, want only the leaf's", got)
	}

	if err := h.depth.report(t, kids[1], "beta settled"); err != nil {
		t.Fatal(err)
	}
	check("handed over", depthRoot, kids[0])
	l := h.sp.spawner.listForCaller(depthRoot)
	if l[0].Parent != kids[0] || !strings.Contains(l[0].Description, `(from depth leaf "Alpha"`) {
		t.Errorf("handed-over listing = %+v, want it keyed under the leaf and saying so", l[0])
	}

	ev := obs.snapshot()
	started := eventsFor(ev, "started", id)
	if len(started) != 2 || !strings.HasPrefix(started[1], depthRoot+" ↪ Handed over from depth leaf \"Alpha\"") || !strings.Contains(started[1], "dig into alpha") {
		t.Fatalf("started events = %q, want the leaf's then the root's with its origin and task", started)
	}
	if got := eventsFor(ev, "ended", id); !slices.Equal(got, []string{kids[0]}) {
		t.Errorf("ended events = %q, want the leaf's entry closed", got)
	}
	if texts := eventsFor(ev, "text", id); len(texts) == 0 || texts[len(texts)-1] != kids[0]+" "+spawnHandedOverNote {
		t.Errorf("text events = %q, want the leaf's view closed with the hand-over note", texts)
	}

	// /stop in the leaf no longer reaches it; the root's per-item stop does.
	if n := tr.Stop(kids[0]); n != 0 {
		t.Errorf("Stop(leaf) stopped %d, want 0", n)
	}
	if tr.StopOne(kids[0], id) {
		t.Error("StopOne(leaf) stopped the handed-over spawn")
	}
	if !tr.StopOne(depthRoot, id) {
		t.Fatal("StopOne(root) did not stop the spawn the root owns")
	}
	d := h.sp.nextDelivery(t)
	if d.session != depthRoot || !strings.Contains(d.msg, "stopped") {
		t.Fatalf("delivered to %s: %q", d.session, d.msg)
	}
	d.done(true)
	if got := eventsFor(obs.snapshot(), "ended", id); !slices.Equal(got, []string{kids[0], depthRoot}) {
		t.Errorf("ended events = %q, want its end shown in the root", got)
	}
}

// #3206: spawn_stop all and a spawn name's uniqueness follow the owner too.
func TestHandedOverSpawnStopAllAndNames(t *testing.T) {
	agent := newScriptedSpawnAgent(1)
	h := newOwnerHarness(t, agent, nil, &ownerEventObserver{})
	kids := h.depth.fanout(t, depthRoot, "Alpha")
	id := h.spawnFrom(t, kids[0], "dig")
	<-agent.started
	if err := h.depth.report(t, kids[0], "done"); err != nil {
		t.Fatal(err)
	}
	if got := runningIDs(h.sp.tracker, depthRoot); !slices.Equal(got, []string{id}) {
		t.Fatalf("Running(root) = %v, want the handed-over spawn", got)
	}
	// persistHarness.spawn names every spawn "persis1".
	if _, err := h.sp.spawner.claimSpawnName(depthRoot, "persis1"); err == nil {
		t.Error("the root could claim the name of a spawn it owns")
	}
	if release, err := h.sp.spawner.claimSpawnName(kids[0], "persis1"); err != nil {
		t.Errorf("the reported leaf cannot reuse the name of a spawn it handed over: %v", err)
	} else {
		release()
	}
	res, err := h.sp.spawner.stop(kids[0], "all")
	if err != nil || !strings.Contains(res.Text, "No running spawns") {
		t.Errorf("spawn_stop all from the leaf = %q, %v, want nothing to stop", res.Text, err)
	}
	res, err = h.sp.spawner.stop(depthRoot, "all")
	if err != nil || !strings.Contains(res.Text, "Stopping 1 spawn") {
		t.Fatalf("spawn_stop all from the root = %q, %v", res.Text, err)
	}
	h.sp.nextDelivery(t).done(true)
}

// #3206 with #2624's restart guarantee: after a restart, a resumed spawn of a
// reported leaf is shown where the route holds it (the leaf, until the
// parent's fan-in), and moves to the parent at the handover.
func TestRestoredSpawnShownWithItsOwner(t *testing.T) {
	agent := newScriptedSpawnAgent(1)
	h := newOwnerHarness(t, agent, nil, &ownerEventObserver{})
	kids := h.depth.fanout(t, depthRoot, "Alpha", "Beta")
	id := h.spawnFrom(t, kids[0], "dig")
	<-agent.started
	if err := h.depth.report(t, kids[0], "alpha settled"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(agent.gates[0]) })

	// The next process: spawns restore before the trees (main.go order).
	agent2 := newScriptedSpawnAgent(1)
	t.Cleanup(func() { close(agent2.gates[0]) })
	obs2 := &ownerEventObserver{}
	h2 := newOwnerHarness(t, agent2, h.sp.store, obs2)
	h2.sp.spawner.RestoreSpawns()
	<-agent2.started
	if got := eventsFor(obs2.snapshot(), "started", id); len(got) != 1 || !strings.HasPrefix(got[0], kids[0]+" ") {
		t.Fatalf("restored: started events = %q, want the leaf's (held)", got)
	}
	h2.depth.m.Restore()
	if got := runningIDs(h2.sp.tracker, kids[0]); !slices.Equal(got, []string{id}) {
		t.Errorf("restored, held: Running(leaf) = %v", got)
	}
	if err := h2.depth.report(t, kids[1], "beta settled"); err != nil {
		t.Fatal(err)
	}
	if got := runningIDs(h2.sp.tracker, depthRoot); !slices.Equal(got, []string{id}) {
		t.Errorf("after the fan-in: Running(root) = %v", got)
	}
	got := eventsFor(obs2.snapshot(), "started", id)
	if len(got) != 2 || !strings.HasPrefix(got[1], depthRoot+" ") {
		t.Errorf("started events = %q, want it moved to the root", got)
	}
}

// #3206: a session that is not a depth node keeps every surface on itself,
// as before ownership was tracked.
func TestNonDepthSpawnOwnedByItsStarter(t *testing.T) {
	agent := newScriptedSpawnAgent(1)
	obs := &ownerEventObserver{}
	h := newOwnerHarness(t, agent, nil, obs)
	const sk = "test/c42"
	id := h.spawnFrom(t, sk, "plain")
	<-agent.started
	h.sp.spawner.Reown()
	if got := runningIDs(h.sp.tracker, sk); !slices.Equal(got, []string{id}) {
		t.Errorf("Running(%s) = %v", sk, got)
	}
	close(agent.gates[0])
	d := h.sp.nextDelivery(t)
	if d.session != sk || strings.Contains(d.msg, "[REPARENTED SPAWN]") {
		t.Fatalf("delivered to %s: %q", d.session, d.msg)
	}
	d.done(true)
	for _, e := range obs.snapshot() {
		if !strings.Contains(e, " "+sk+" ") {
			t.Errorf("event %q not told for the starter %s", e, sk)
		}
	}
}

// #3206: with depth mode off a leaf's report hands its spawns over at once
// (#2628), with no fan-in to announce it: the report itself re-reads the
// owners, so the root shows the spawn running before any result arrives.
// Turning depth back on before the root's fan-in holds it in the leaf again.
func TestDepthOffReportMovesSpawnAtOnce(t *testing.T) {
	agent := newScriptedSpawnAgent(1)
	h := newOwnerHarness(t, agent, nil, &ownerEventObserver{})
	kids := h.depth.fanout(t, depthRoot, "Alpha", "Beta")
	id := h.spawnFrom(t, kids[0], "dig")
	<-agent.started
	h.depth.off.Store(true)
	h.depth.m.OnDisable()
	if err := h.depth.report(t, kids[0], "alpha settled"); err != nil {
		t.Fatal(err)
	}
	if got := runningIDs(h.sp.tracker, depthRoot); !slices.Equal(got, []string{id}) {
		t.Errorf("depth off, reported: Running(root) = %v, want the spawn", got)
	}
	h.depth.off.Store(false)
	h.depth.m.OnEnable()
	if got := runningIDs(h.sp.tracker, kids[0]); !slices.Equal(got, []string{id}) {
		t.Errorf("depth on again, fan-in pending: Running(leaf) = %v, want the spawn held there", got)
	}
	close(agent.gates[0])
	waitResult(t, h.sp.store, id)
	h.sp.noDelivery(t)
}
