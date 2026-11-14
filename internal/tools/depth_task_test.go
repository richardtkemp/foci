package tools

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/fap"
)

// #2998: a fan-out chooses per leaf whether it opens with a question to the
// user (the default, #2617) or a task to the leaf agent, which runs at once.

const depthTaskOrientation = "LEAF {branch_key}\n{opening_rule}\n{report_rule}"

func feedNode(t *testing.T, f depthFeedRec, leafID string) fap.DepthNode {
	t.Helper()
	for _, n := range f.nodes {
		if n.LeafID == leafID {
			return n
		}
	}
	t.Fatalf("leaf %s not in feed %+v", leafID, f.nodes)
	return fap.DepthNode{}
}

func TestDepthTaskLeafRunsAtOnce(t *testing.T) {
	h := newDepthHarness(t, nil)
	h.m.deps.OrientationTemplate = depthTaskOrientation
	var during []fap.DepthNode
	h.runner.mu.Lock()
	h.runner.during = func(tr depthTurn) {
		// What the app shows while the task turn runs.
		f := h.surface.lastFeed(t)
		during = append(during, f.nodes...)
	}
	h.runner.mu.Unlock()

	raw := json.RawMessage(`{"leaves": [{"title": "Ask", "question": "Which colour?"}, {"title": "Dig", "task": "Investigate the crash in X."}]}`)
	res, err := h.m.fanoutTool().Execute(depthCtx(depthRoot), raw)
	if err != nil {
		t.Fatal(err)
	}
	kids := h.node(depthRoot).Children
	q, task := kids[0], kids[1]

	// The question leaf opens as today. The task leaf shows the user nothing
	// at creation (#3095, Dick 2026-11-13): its first visible message is its
	// own reply to the task. (DepthSurface has no call that could show it.)
	if len(h.surface.prompts) != 1 || h.surface.prompts[0] != q+"|Which colour?" {
		t.Errorf("shown in leaves = %v, want only the question leaf's question", h.surface.prompts)
	}
	if !strings.Contains(h.branch.orients[1], "does NOT see it") {
		t.Errorf("task orientation does not tell the leaf the user cannot see the task: %q", h.branch.orients[1])
	}

	// Exactly one turn ran: the task leaf's, on its task.
	turns := h.runner.wait(t, 1)
	if turns[0].key != task || turns[0].text != "Investigate the crash in X." || turns[0].trigger != "depth_task" {
		t.Fatalf("task turn = %+v", turns[0])
	}
	h.noTurns(t)

	// While it ran the app showed it working, not waiting for the user.
	tl := h.node(task).LeafID
	var sawTask bool
	for _, n := range during {
		if n.LeafID == tl {
			sawTask = true
			if n.WaitingForUser {
				t.Errorf("task leaf shown waiting for the user during its task turn: %+v", n)
			}
		}
		if n.LeafID == h.node(q).LeafID && !n.WaitingForUser {
			t.Errorf("question leaf not shown waiting for the user: %+v", n)
		}
	}
	if !sawTask {
		t.Fatal("no feed showed the task leaf during its turn")
	}
	// Once the turn ended, it waits for the user like any idle leaf.
	if n := feedNode(t, h.surface.lastFeed(t), tl); !n.WaitingForUser {
		t.Errorf("task leaf after its turn = %+v, want waiting", n)
	}

	n := h.node(task)
	if n.Opening != depthOpenTask || !n.TaskStarted || !n.TaskDone || n.Prompt != "Investigate the crash in X." {
		t.Errorf("task node = %+v", n)
	}
	if h.node(q).Opening != "" {
		t.Errorf("question node opening = %q", h.node(q).Opening)
	}

	// The forks: the question ends on the question; the task ends on a ready
	// line, with no fallback (its task is its first user message).
	qo, to := h.branch.openings[0], h.branch.openings[1]
	if qo.AgentText != "Which colour?" || qo.Fallback == "" {
		t.Errorf("question opening = %+v", qo)
	}
	if to.AgentText != depthTaskReady || to.Fallback != "" || to.CallMarker != depthCallMarker || !strings.Contains(to.ToolResult, `leaf "Dig"`) {
		t.Errorf("task opening = %+v", to)
	}
	// Each orientation says how its leaf opened.
	if o := h.branch.orients[0]; !strings.Contains(o, "your own message to the user") || strings.Contains(o, "{opening_rule}") {
		t.Errorf("question orientation = %q", o)
	}
	if o := h.branch.orients[1]; !strings.Contains(o, "TASK for you") || strings.Contains(o, "{opening_rule}") {
		t.Errorf("task orientation = %q", o)
	}
	if !strings.Contains(res.Text, "Dig ("+tl+") — task") || !strings.Contains(res.Text, "Ask ("+h.node(q).LeafID+") — question") {
		t.Errorf("fan-out result = %q", res.Text)
	}
}

// A nested fan-out from a leaf gets the same choice.
func TestDepthTaskNested(t *testing.T) {
	h := newDepthHarness(t, nil)
	kids := h.fanout(t, depthRoot, "Parent")
	raw := json.RawMessage(`{"leaves": [{"title": "Sub", "task": "Check Y."}]}`)
	if _, err := h.m.fanoutTool().Execute(depthCtx(kids[0]), raw); err != nil {
		t.Fatal(err)
	}
	grand := h.node(kids[0]).Children
	if turns := h.runner.wait(t, 1); turns[0].key != grand[0] || turns[0].text != "Check Y." {
		t.Fatalf("nested task turn = %+v", turns[0])
	}
	for _, p := range h.surface.prompts {
		if strings.HasPrefix(p, grand[0]+"|") {
			t.Errorf("nested task leaf showed %q, want nothing (#3095)", p)
		}
	}
}

func TestDepthTaskRefusals(t *testing.T) {
	h := newDepthHarness(t, nil)
	raw := json.RawMessage(`{"leaves": [{"title": "Both", "question": "q?", "task": "do t"}]}`)
	if _, err := h.m.fanoutTool().Execute(depthCtx(depthRoot), raw); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("question and task: err = %v", err)
	}
	// A manager that cannot start a leaf's turn says so instead of falling
	// back to a question, and makes no leaf.
	h.m.deps.RunTurn = nil
	raw = json.RawMessage(`{"leaves": [{"title": "T", "task": "do t"}]}`)
	if _, err := h.m.fanoutTool().Execute(depthCtx(depthRoot), raw); err == nil || !strings.Contains(err.Error(), "cannot start a leaf's turn") {
		t.Errorf("no RunTurn: err = %v", err)
	}
	if h.branch.n != 0 {
		t.Errorf("forks made despite the refusal: %d", h.branch.n)
	}
	// Questions still work without it.
	h.fanout(t, depthRoot, "Q")
}

// The task survives a restart: one never started runs then; one cut off
// mid-turn is resumed with its conversation kept, the task not shown again;
// one that finished runs nothing.
func TestDepthTaskRestore(t *testing.T) {
	store := newMemSpawnStore()
	h1 := newDepthHarness(t, store)
	h1.runner.mu.Lock()
	h1.runner.refuse = true // shutting down: the task turns never start
	h1.runner.mu.Unlock()
	raw := json.RawMessage(`{"leaves": [{"title": "Never", "task": "task N"}, {"title": "Cut", "task": "task C"}, {"title": "Done", "task": "task D"}, {"title": "Ask", "question": "q?"}]}`)
	if _, err := h1.m.fanoutTool().Execute(depthCtx(depthRoot), raw); err != nil {
		t.Fatal(err)
	}
	h1.runner.wait(t, 3)
	kids := h1.node(depthRoot).Children
	never, cut, done, ask := kids[0], kids[1], kids[2], kids[3]
	if n := h1.node(never); n.TaskStarted || n.TaskDone {
		t.Fatalf("refused task turn marked started: %+v", n)
	}
	// Cut: started, then foci died mid-turn. Done: ran to the end.
	h1.m.mu.Lock()
	h1.m.nodes[cut].TaskStarted = true
	h1.m.nodes[done].TaskStarted, h1.m.nodes[done].TaskDone = true, true
	recs := snap(h1.m.nodes[cut], h1.m.nodes[done])
	h1.m.mu.Unlock()
	h1.m.persist(recs...)

	h2 := newDepthHarness(t, store)
	var taken []string
	h2.m.deps.TakeUndelivered = func(k string) []string {
		taken = append(taken, k)
		if k == cut {
			return []string{"unconsumed input"}
		}
		return nil
	}
	h2.m.Restore()
	turns := h2.runner.wait(t, 2)
	h2.noTurns(t)
	byKey := map[string]depthTurn{}
	for _, tr := range turns {
		byKey[tr.key] = tr
	}
	if tr := byKey[never]; tr.text != "task N" || tr.trigger != "depth_task" {
		t.Errorf("never-started task after restart = %+v", tr)
	}
	tr := byKey[cut]
	if !strings.HasPrefix(tr.text, "[SYSTEM: FOCI RESTARTED]") || !strings.Contains(tr.text, "task C") || !strings.HasSuffix(tr.text, "unconsumed input") {
		t.Errorf("resumed task turn = %+v", tr)
	}
	if _, ok := byKey[done]; ok {
		t.Error("a finished task ran again")
	}
	if _, ok := byKey[ask]; ok {
		t.Error("a question leaf ran a turn at restore")
	}
	if len(taken) != 1 || taken[0] != cut {
		t.Errorf("TakeUndelivered calls = %v, want the resumed leaf only", taken)
	}
	if len(h2.surface.prompts) != 0 {
		t.Errorf("restore showed openings again: %v", h2.surface.prompts)
	}
	if n := h2.node(cut); n.TaskResumes != 1 || !n.TaskDone {
		t.Errorf("resumed node = %+v", n)
	}

	// A task interrupted by restart after restart stops being resumed.
	h2.m.mu.Lock()
	h2.m.nodes[cut].TaskDone = false
	h2.m.nodes[cut].TaskResumes = maxDepthTaskResumes
	recs = snap(h2.m.nodes[cut])
	h2.m.mu.Unlock()
	h2.m.persist(recs...)
	h3 := newDepthHarness(t, store)
	h3.m.Restore()
	h3.noTurns(t)
}

// #3099 (like #3071): a resumed task leaf that waits for a slot keeps its
// unconsumed inputs in its persisted record, so a second restart in that wait
// still delivers them, once.
func TestDepthTaskResumeKeepsInputsAcrossSecondRestart(t *testing.T) {
	store := newMemSpawnStore()
	h1 := newDepthHarness(t, store)
	h1.runner.mu.Lock()
	h1.runner.refuse = true
	h1.runner.mu.Unlock()
	raw := json.RawMessage(`{"leaves": [{"title": "Cut", "task": "task C"}]}`)
	if _, err := h1.m.fanoutTool().Execute(depthCtx(depthRoot), raw); err != nil {
		t.Fatal(err)
	}
	h1.runner.wait(t, 1)
	cut := h1.node(depthRoot).Children[0]
	h1.m.mu.Lock()
	h1.m.nodes[cut].TaskStarted = true
	recs := snap(h1.m.nodes[cut])
	h1.m.mu.Unlock()
	h1.m.persist(recs...)

	// First restart: the only slot is held, so the resume waits. The
	// backend's unconsumed inputs leave the delivery store here.
	h2 := newDepthHarness(t, store)
	h2.m.deps.MaxConcurrent = func() int { return 1 }
	h2.m.tasksRunning = 1
	h2.m.deps.TakeUndelivered = func(k string) []string {
		if k == cut {
			return []string{"input one", "input two"}
		}
		return nil
	}
	h2.m.Restore()
	h2.noTurns(t)

	// Second restart in that wait: the delivery store is now empty.
	h3 := newDepthHarness(t, store)
	h3.m.deps.TakeUndelivered = func(string) []string { return nil }
	h3.m.Restore()
	turns := h3.runner.wait(t, 1)
	h3.noTurns(t)
	tr := turns[0]
	if tr.key != cut || !strings.HasPrefix(tr.text, "[SYSTEM: FOCI RESTARTED]") {
		t.Fatalf("resume turn = %+v", tr)
	}
	for _, in := range []string{"input one", "input two"} {
		if c := strings.Count(tr.text, in); c != 1 {
			t.Errorf("%q appears %d times in the resume turn, want 1: %q", in, c, tr.text)
		}
	}
	if i, j := strings.Index(tr.text, "input one"), strings.Index(tr.text, "input two"); i > j {
		t.Errorf("inputs out of order: %q", tr.text)
	}
	if n := h3.node(cut); len(n.ResumeInputs) != 0 || n.TaskResumes != 2 {
		t.Errorf("node after the resume took its slot = %+v", n)
	}
}

// gatedRunner holds each turn until released, to test the concurrency bound.
type gatedRunner struct {
	mu      sync.Mutex
	started chan string
	dones   map[string]func(bool)
}

func (g *gatedRunner) run(key, _, _ string, started func(), done func(bool)) {
	if started != nil {
		started()
	}
	g.mu.Lock()
	g.dones[key] = done
	g.mu.Unlock()
	g.started <- key
}

func (g *gatedRunner) finish(key string) {
	g.mu.Lock()
	d := g.dones[key]
	g.mu.Unlock()
	d(true)
}

// Task turns run within MaxConcurrent; the waiting ones show as working, and a
// leaf cancelled while it waits runs nothing.
func TestDepthTaskMaxConcurrent(t *testing.T) {
	h := newDepthHarness(t, nil)
	g := &gatedRunner{started: make(chan string, 10), dones: map[string]func(bool){}}
	h.m.deps.RunTurn = g.run
	h.m.deps.MaxConcurrent = func() int { return 1 }
	raw := json.RawMessage(`{"leaves": [{"title": "A", "task": "a"}, {"title": "B", "task": "b"}, {"title": "C", "task": "c"}]}`)
	if _, err := h.m.fanoutTool().Execute(depthCtx(depthRoot), raw); err != nil {
		t.Fatal(err)
	}
	kids := h.node(depthRoot).Children
	next := func() string {
		t.Helper()
		select {
		case k := <-g.started:
			return k
		case <-time.After(10 * time.Second):
			t.Fatal("no task turn started")
		}
		return ""
	}
	none := func() {
		t.Helper()
		select {
		case k := <-g.started:
			t.Fatalf("task turn %s started past the bound", k)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if k := next(); k != kids[0] {
		t.Fatalf("first task turn on %s", k)
	}
	none()
	for _, n := range h.surface.lastFeed(t).nodes {
		if n.WaitingForUser {
			t.Errorf("leaf %s shown waiting while its task is pending", n.Title)
		}
	}
	// B is cancelled while it waits: it never runs, and C takes the slot.
	if err := h.m.Dismiss(depthRoot, h.node(kids[1]).LeafID); err != nil {
		t.Fatal(err)
	}
	g.finish(kids[0])
	if k := next(); k != kids[2] {
		t.Fatalf("second task turn on %s, want C", k)
	}
	none()
	g.finish(kids[2])
	none()
	h.m.mu.Lock()
	running, waiting, queued := h.m.tasksRunning, len(h.m.tasksWaiting), len(h.m.queued)
	h.m.mu.Unlock()
	if running != 0 || waiting != 0 || queued != 0 {
		t.Errorf("after all tasks: running %d waiting %d queued %d", running, waiting, queued)
	}
}
