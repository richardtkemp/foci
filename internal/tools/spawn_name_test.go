package tools

// Spawn names (#2721): every spawn call names its spawn; the name is unique
// among the spawns spawn_status lists in the calling session, is a second
// handle for spawn_send, spawn_stop and spawn_status, and labels the spawn in
// the agent's text and the user's.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// spawnTool runs the spawn tool on s from session with params.
func spawnTool(s *Spawner, session string, params map[string]any) (ToolResult, error) {
	raw, _ := json.Marshal(params)
	return s.SpawnTool().Execute(WithSessionKey(context.Background(), session), raw)
}

// A spawn call with no name, an empty one or one of only whitespace is
// refused in every mode, as another agent too, and starts nothing: no batch,
// no branch, nothing tracked. The tool's schema lists name as required, which
// the foci_spawn shell form is generated from. Fail-arm: before #2721 every
// one of these calls ran.
func TestSpawnName_Required(t *testing.T) {
	t.Parallel()
	runner := &recordingBatchRunner{}
	s := newDelegatedSpawner(runner, allBatchCaps)
	s.deps.Tracker = NewSpawnTracker()
	branched := false
	s.deps.CharacterBranch = func(context.Context, string, string) (string, error) {
		branched = true
		return "test/c1/b1", nil
	}
	s.deps.Notifier = NewAsyncNotifier(func(string, string, string, string, *AgentOrigin) {})
	cases := []map[string]any{
		{"context": "raw"},
		{"context": "character"},
		{"context": "character", "background": true},
		{"context": "clone"},
		{"context": "explore"},
		{},
		{"agent": "someone-else"},
	}
	for _, c := range cases {
		for _, name := range []any{nil, "", "   ", "\n\t "} {
			params := map[string]any{"prompt": "do the thing"}
			for k, v := range c {
				params[k] = v
			}
			if name != nil {
				params["name"] = name
			}
			_, err := spawnTool(s, "test/c1", params)
			if err == nil || !strings.Contains(err.Error(), "name is required") {
				t.Errorf("spawn %v: err = %v, want a name is required error", params, err)
			}
		}
	}
	if len(runner.reqs) != 0 || branched || len(s.deps.Tracker.List("test/c1")) != 0 {
		t.Fatalf("a refused spawn started something: %d batches, branched %v, tracked %v", len(runner.reqs), branched, s.deps.Tracker.List("test/c1"))
	}
	var schema struct {
		Required   []string                  `json:"required"`
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(s.SpawnTool().Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(schema.Required, "name") || schema.Properties["name"]["description"] == "" {
		t.Errorf("spawn schema = %+v, want name documented and required", schema)
	}
}

// The name "all" is refused (spawn_stop all means every spawn); leading and
// trailing whitespace is not part of a name.
func TestSpawnName_AllRefusedAndTrimmed(t *testing.T) {
	t.Parallel()
	runner := &recordingBatchRunner{}
	s := newDelegatedSpawner(runner, allBatchCaps)
	s.deps.Tracker = NewSpawnTracker()
	for _, name := range []string{"all", "  all "} {
		if _, err := spawnTool(s, "test/c1", map[string]any{"name": name, "prompt": "p", "context": "raw"}); err == nil || !strings.Contains(err.Error(), `"all"`) {
			t.Errorf("name %q: err = %v, want refused", name, err)
		}
	}
	if len(runner.reqs) != 0 {
		t.Fatalf("a spawn named all ran")
	}
	if _, err := spawnTool(s, "test/c1", map[string]any{"name": "  reviewer \n", "prompt": "p", "context": "raw"}); err != nil {
		t.Fatal(err)
	}
	if l := s.deps.Tracker.List("test/c1"); len(l) != 1 || l[0].Name != "reviewer" {
		t.Fatalf("tracked %+v, want one spawn named reviewer", l)
	}
}

// A name a finished spawn of the session still holds (spawn_status lists it)
// is refused, exactly and after trimming, with the holder's id; nothing runs.
// Another case, or another session, is free to use it. Fail-arm: before
// #2721 there were no names to clash.
func TestSpawnName_DuplicateOfFinishedRefused(t *testing.T) {
	t.Parallel()
	runner := &recordingBatchRunner{}
	s := newDelegatedSpawner(runner, allBatchCaps)
	s.deps.Tracker = NewSpawnTracker()
	if _, err := spawnTool(s, "test/c1", map[string]any{"name": "reviewer", "prompt": "p", "context": "raw"}); err != nil {
		t.Fatal(err)
	}
	first := s.deps.Tracker.List("test/c1")[0]
	for _, name := range []string{"reviewer", " reviewer "} {
		_, err := spawnTool(s, "test/c1", map[string]any{"name": name, "prompt": "p", "context": "explore"})
		if err == nil || !strings.Contains(err.Error(), first.ID) {
			t.Errorf("duplicate %q: err = %v, want refused naming %s", name, err, first.ID)
		}
	}
	if len(runner.reqs) != 1 {
		t.Fatalf("%d batches ran, want only the first", len(runner.reqs))
	}
	if _, err := spawnTool(s, "test/c1", map[string]any{"name": "Reviewer", "prompt": "p", "context": "raw"}); err != nil {
		t.Errorf("another case: %v, want allowed (names are case-sensitive)", err)
	}
	if _, err := spawnTool(s, "test/c2", map[string]any{"name": "reviewer", "prompt": "p", "context": "raw"}); err != nil {
		t.Errorf("another session: %v, want allowed", err)
	}
}

// A running spawn holds its name: a second spawn under it is refused and
// starts nothing.
func TestSpawnName_DuplicateOfRunningRefused(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(1)
	h := newFollowupHarness(agent)
	id := h.start(t, "long job")
	<-agent.started
	params, _ := json.Marshal(map[string]string{"name": "follow1", "prompt": "another", "context": "clone"})
	_, err := h.spawner.SpawnTool().Execute(h.ctx, params)
	if err == nil || !strings.Contains(err.Error(), id) {
		t.Fatalf("err = %v, want refused naming the running %s", err, id)
	}
	select {
	case i := <-agent.started:
		t.Fatalf("the refused spawn ran (turn %d)", i)
	case <-time.After(50 * time.Millisecond):
	}
	close(agent.gates[0])
	<-h.delivered
}

// A name is free again once its spawn is no longer listed: pruned beyond the
// finished spawns a session keeps, or forgotten by a restart.
func TestSpawnName_FreeAfterForgotten(t *testing.T) {
	t.Parallel()
	runner := &recordingBatchRunner{}
	s := newDelegatedSpawner(runner, allBatchCaps)
	s.deps.Tracker = NewSpawnTracker()
	run := func(name string) error {
		_, err := spawnTool(s, "test/c1", map[string]any{"name": name, "prompt": "p", "context": "raw"})
		return err
	}
	if err := run("keep"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // keep ends strictly first
	for i := 0; i < maxFinishedSpawns; i++ {
		if err := run(fmt.Sprintf("other %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := run("keep"); err != nil {
		t.Fatalf("keep after it was pruned: %v, want allowed", err)
	}
	if err := run("other 19"); err == nil {
		t.Fatal("a listed spawn's name was taken again")
	}
	s.deps.Tracker = NewSpawnTracker() // what a restart remembers of finished spawns: nothing
	if err := run("other 19"); err != nil {
		t.Fatalf("after a restart: %v, want allowed", err)
	}
}

// A spawn handed over to a depth node holds its name in that node's session,
// and a name that two handed-over spawns carry is ambiguous for lookup: the
// call is refused and lists both ids. Each of them is still reachable by id.
func TestSpawnName_HandedOverHoldsNameAndAmbiguity(t *testing.T) {
	agent := newScriptedSpawnAgent(2)
	h := newReparentHarness(t, agent, nil)
	kids := h.depth.fanout(t, depthRoot, "Alpha", "Beta")
	a := h.spawnFrom(t, kids[0], "dig into alpha")
	<-agent.started
	b := h.spawnFrom(t, kids[1], "dig into beta") // both are named persis1
	<-agent.started
	for i, k := range kids {
		if err := h.depth.report(t, k, fmt.Sprintf("leaf %d done", i)); err != nil {
			t.Fatal(err)
		}
	}

	_, err := h.status(t, depthRoot, "persis1")
	if err == nil || !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), b) {
		t.Fatalf("status by an ambiguous name: err = %v, want refused listing %s and %s", err, a, b)
	}
	if txt, err := h.status(t, depthRoot, a); err != nil || !strings.Contains(txt, a) {
		t.Fatalf("status by id = %q, %v", txt, err)
	}
	raw, _ := json.Marshal(map[string]any{"name": "persis1", "prompt": "p", "context": "clone"})
	if _, err := h.sp.spawner.SpawnTool().Execute(WithSessionKey(h.sp.ctx, depthRoot), raw); err == nil {
		t.Fatal("the root took a name its handed-over spawns hold")
	}
	close(agent.gates[0])
	close(agent.gates[1])
}

// spawn_status, spawn_send and spawn_stop find a spawn by its name, in the
// tool and through its positional shell argument, as by its id.
func TestSpawnName_LookupInAllThreeTools(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(1)
	h := newFollowupHarness(agent)
	id := h.start(t, "watch CI")
	<-agent.started

	raw, _ := json.Marshal(map[string]string{"id": "follow1"})
	res, err := h.spawner.StatusTool().Execute(h.ctx, raw)
	if err != nil || !strings.Contains(res.Text, "Spawn "+id) {
		t.Fatalf("spawn_status by name = %q, %v", res.Text, err)
	}
	if !slices.Contains(h.spawner.StatusTool().Positional, "id") {
		t.Error("spawn_status takes no positional id: foci_spawn_status <name> fails")
	}
	if txt := h.send(t, "follow1", "also check lint"); !strings.Contains(txt, id) {
		t.Fatalf("spawn_send by name = %q", txt)
	}
	raw, _ = json.Marshal(map[string]string{"id": "follow1"})
	res, err = h.spawner.StopTool().Execute(h.ctx, raw)
	if err != nil || !strings.Contains(res.Text, "Stopping spawn "+id) {
		t.Fatalf("spawn_stop by name = %q, %v", res.Text, err)
	}
	msg := <-h.delivered
	if !strings.Contains(msg, "follow1") || !strings.Contains(msg, "stopped by its parent agent") {
		t.Fatalf("result = %q, want the stopped follow1", msg)
	}
	raw, _ = json.Marshal(map[string]string{"id": "nobody"})
	if _, err := h.spawner.StatusTool().Execute(h.ctx, raw); err == nil || !strings.Contains(err.Error(), "no spawn") {
		t.Errorf("unknown name: err = %v, want no spawn", err)
	}
}

// When one spawn's name equals another spawn's id, the id wins.
func TestSpawnName_IDWinsOverName(t *testing.T) {
	t.Parallel()
	runner := &recordingBatchRunner{}
	s := newDelegatedSpawner(runner, allBatchCaps)
	s.deps.Tracker = NewSpawnTracker()
	if _, err := spawnTool(s, "test/c1", map[string]any{"name": "first", "prompt": "p", "context": "raw"}); err != nil {
		t.Fatal(err)
	}
	firstID := s.deps.Tracker.List("test/c1")[0].ID
	if _, err := spawnTool(s, "test/c1", map[string]any{"name": firstID, "prompt": "p", "context": "raw"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"id": firstID})
	res, err := s.StatusTool().Execute(WithSessionKey(context.Background(), "test/c1"), raw)
	if err != nil || !strings.Contains(res.Text, "Name: first") {
		t.Fatalf("status %s = %q, %v; want the spawn with that id (named first)", firstID, res.Text, err)
	}
}

// The agent sees the name: the start reply of a background spawn, every
// [SPAWN RESULT] form, and spawn_status's list and detail, which keep the
// prompt opening as its task. Fail-arm: before #2721 none of them could.
func TestSpawnName_InAgentText(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(1)
	h := newFollowupHarness(agent)
	params, _ := json.Marshal(map[string]string{"name": "ci  watcher", "prompt": "Watch the CI run\nand report", "context": "clone"})
	res, err := h.spawner.SpawnTool().Execute(h.ctx, params)
	if err != nil || !strings.Contains(res.Text, "Name: ci watcher\n") {
		t.Fatalf("start reply = %q, %v; want it to name the spawn", res.Text, err)
	}
	id := spawnIDRe.FindStringSubmatch(res.Text)[1]
	<-agent.started

	list, _ := h.spawner.StatusTool().Execute(h.ctx, json.RawMessage(`{}`))
	if !strings.Contains(list.Text, id+"  ci watcher  running") || !strings.Contains(list.Text, "Watch the CI run and report") {
		t.Errorf("spawn_status list = %q, want the name and the task", list.Text)
	}
	detail, _ := h.spawner.StatusTool().Execute(h.ctx, json.RawMessage(`{"id":"ci  watcher"}`))
	if !strings.Contains(detail.Text, "Name: ci watcher\n") || !strings.Contains(detail.Text, "Task: Watch the CI run and report") {
		t.Errorf("spawn_status detail = %q, want Name and Task", detail.Text)
	}
	close(agent.gates[0])
	if msg := <-h.delivered; !strings.Contains(msg, "[SPAWN RESULT] ci watcher — Branch ") || !strings.Contains(msg, "completed:") {
		t.Errorf("completed result = %q, want it named", msg)
	}
}

// The failed and stopped [SPAWN RESULT] forms name the spawn too.
func TestSpawnName_InFailedAndStoppedResults(t *testing.T) {
	t.Parallel()
	tr := NewSpawnTracker()
	s := NewSpawner(SpawnDeps{AgentID: "test", Tracker: tr}, func() SpawnAgent { return nil })
	tr.Add("test/c1", RunningSpawn{ID: "spawn-f1", Name: "builder", BranchKey: "test/c1/b1"}, func() {})
	if msg := s.resultMessage("test/c1", "spawn-f1", "test/c1/b1", false, fmt.Errorf("boom"), "", 0); !strings.HasPrefix(msg, "[SPAWN RESULT] builder — Branch test/c1/b1 (spawn spawn-f1) failed:") {
		t.Errorf("failed result = %q", msg)
	}
	if msg := s.resultMessage("test/c1", "spawn-f1", "test/c1/b1", true, nil, "", 0); !strings.HasPrefix(msg, "[SPAWN RESULT] builder — Branch test/c1/b1 (spawn spawn-f1) was stopped by") {
		t.Errorf("stopped result = %q", msg)
	}
}

// A restart keeps a spawn's name: the restart chat line and the observer's
// "(resumed after restart)" entry show it, and so does the resumed run's
// [SPAWN RESULT]. A spawn that cannot be resumed is reported lost under its
// name.
func TestSpawnName_SurvivesRestart(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(1)
	obs := &recordingSpawnObserver{}
	h := newPersistHarness(agent, func(d *SpawnDeps) { d.Observer = obs })
	rec := runningRecord("spawn-n1")
	rec.Name = "star counter"
	h.store.put(t, rec)

	h.spawner.RestoreSpawns()
	if n := <-h.notices; !strings.Contains(n, "spawn-n1 (star counter)") {
		t.Errorf("restart line = %q, want the spawn's name", n)
	}
	<-agent.started
	obs.mu.Lock()
	shown := obs.started[0]
	obs.mu.Unlock()
	if shown.Label() != "(resumed after restart) star counter" {
		t.Errorf("observer label = %q", shown.Label())
	}
	close(agent.gates[0])
	if d := h.nextDelivery(t); !strings.Contains(d.msg, "[SPAWN RESULT] star counter — Branch") {
		t.Errorf("resumed result = %q, want it named", d.msg)
	}

	lost := newPersistHarness(newScriptedSpawnAgent(0), func(d *SpawnDeps) { d.CanResume = func(string) bool { return false } })
	rec = runningRecord("spawn-n2")
	rec.Name = "moon counter"
	lost.store.put(t, rec)
	lost.spawner.RestoreSpawns()
	if d := lost.nextDelivery(t); !strings.Contains(d.msg, "[SPAWN RESULT] moon counter — Branch") || !strings.Contains(d.msg, "lost in a foci restart") {
		t.Errorf("lost report = %q, want it named", d.msg)
	}
	if n := <-lost.notices; !strings.Contains(n, "spawn-n2 (moon counter)") {
		t.Errorf("restart line = %q, want the lost spawn's name", n)
	}
}

// A spawn persisted before names existed (its record has no name) still
// resumes, and shows the one placeholder wherever its name would go; it can
// never be found by a name and never blocks one.
func TestSpawnName_UnnamedRecordGetsPlaceholder(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(1)
	obs := &recordingSpawnObserver{}
	h := newPersistHarness(agent, func(d *SpawnDeps) { d.Observer = obs })
	var legacy map[string]any
	b, _ := json.Marshal(runningRecord("spawn-old"))
	_ = json.Unmarshal(b, &legacy)
	delete(legacy, "name")
	b, _ = json.Marshal(legacy)
	_ = h.store.SetAgentMetadata("test", spawnMetaPrefix+"spawn-old", string(b))

	h.spawner.RestoreSpawns()
	if n := <-h.notices; !strings.Contains(n, "spawn-old ("+UnnamedSpawnLabel+")") {
		t.Errorf("restart line = %q, want the placeholder", n)
	}
	<-agent.started
	obs.mu.Lock()
	label := obs.started[0].Label()
	obs.mu.Unlock()
	if label != "(resumed after restart) "+UnnamedSpawnLabel {
		t.Errorf("observer label = %q", label)
	}
	list, _ := h.spawner.StatusTool().Execute(h.ctx, json.RawMessage(`{}`))
	if !strings.Contains(list.Text, "spawn-old  "+UnnamedSpawnLabel+"  running") {
		t.Errorf("spawn_status list = %q, want the placeholder", list.Text)
	}
	for _, ref := range []string{UnnamedSpawnLabel, ""} {
		raw, _ := json.Marshal(map[string]string{"id": ref})
		if res, err := h.spawner.StopTool().Execute(h.ctx, raw); err == nil {
			t.Errorf("spawn_stop %q found %q, want no spawn", ref, res.Text)
		}
	}
	close(agent.gates[0])
	if d := h.nextDelivery(t); !strings.Contains(d.msg, "[SPAWN RESULT] "+UnnamedSpawnLabel+" — Branch") {
		t.Errorf("result = %q, want the placeholder", d.msg)
	}
	params, _ := json.Marshal(map[string]any{"name": UnnamedSpawnLabel, "prompt": "p", "context": "clone"})
	if _, err := h.spawner.SpawnTool().Execute(h.ctx, params); err != nil {
		t.Errorf("a spawn named like the placeholder: %v, want allowed", err)
	}
	h.nextDelivery(t)
}

// Two spawn calls at once under one name: exactly one starts.
func TestSpawnName_ConcurrentCallsTakeOneName(t *testing.T) {
	t.Parallel()
	tr := NewSpawnTracker()
	release, err := tr.claimName("test/c1", []string{"test/c1"}, "twin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.claimName("test/c1", []string{"test/c1"}, "twin"); err == nil {
		t.Fatal("a second call claimed a name another call is starting under")
	}
	if r, err := tr.claimName("test/c2", []string{"test/c2"}, "twin"); err != nil {
		t.Fatalf("another session: %v", err)
	} else {
		r()
	}
	release()
	if r, err := tr.claimName("test/c1", []string{"test/c1"}, "twin"); err != nil {
		t.Fatalf("after release: %v", err)
	} else {
		r()
	}
}

// The user-facing label is the name on one line.
func TestSpawnName_LabelCollapsesWhitespace(t *testing.T) {
	t.Parallel()
	if got := (RunningSpawn{Name: "fix\n  the   parser"}).Label(); got != "fix the parser" {
		t.Errorf("Label = %q", got)
	}
	if got := (RunningSpawn{Description: "a prompt opening"}).Label(); got != UnnamedSpawnLabel {
		t.Errorf("unnamed Label = %q, want the placeholder", got)
	}
}
