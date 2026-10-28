package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/fap"
	"foci/internal/session"
)

// --- fakes ---

type depthFeedRec struct {
	root, state string
	nodes       []fap.DepthNode
}

type fakeDepthSurface struct {
	mu       sync.Mutex
	bound    map[string]bool
	leaves   map[string]string // branch → root
	minted   int               // leaf conversations minted (RegisterLeaf with no conversation id)
	prompts  []string
	feeds    []depthFeedRec
	busyKeys map[string]bool
}

func newFakeDepthSurface(roots ...string) *fakeDepthSurface {
	s := &fakeDepthSurface{bound: map[string]bool{}, leaves: map[string]string{}, busyKeys: map[string]bool{}}
	for _, r := range roots {
		s.bound[r] = true
	}
	return s
}

func (s *fakeDepthSurface) RootBound(k string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bound[k]
}
func (s *fakeDepthSurface) RegisterLeaf(root, branch, _, _, conv string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.bound[root] {
		return "", fmt.Errorf("root %s not bound", root)
	}
	s.leaves[branch] = root
	if conv == "" {
		s.minted++
		conv = "conv:" + branch
	}
	return conv, nil
}
func (s *fakeDepthSurface) UnregisterLeaf(b string) { s.mu.Lock(); delete(s.leaves, b); s.mu.Unlock() }
func (s *fakeDepthSurface) PushFeed(root, state string, nodes []fap.DepthNode) {
	s.mu.Lock()
	s.feeds = append(s.feeds, depthFeedRec{root, state, nodes})
	s.mu.Unlock()
}
func (s *fakeDepthSurface) ShowQuestion(b, text string) {
	s.mu.Lock()
	s.prompts = append(s.prompts, b+"|"+text)
	s.mu.Unlock()
}
func (s *fakeDepthSurface) LeafBusy(k string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busyKeys[k]
}

func (s *fakeDepthSurface) lastFeed(t *testing.T) depthFeedRec {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.feeds) == 0 {
		t.Fatal("no feed pushed")
	}
	return s.feeds[len(s.feeds)-1]
}

type fakeDepthBrancher struct {
	mu sync.Mutex
	n  int
	// orients records each fork's orientation template, openings its opening.
	orients  []string
	openings []*session.BranchOpening
}

func (b *fakeDepthBrancher) ForkSession(_ context.Context, parent string, opts BranchOptions) (string, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if opts.BranchType != "depth" || !opts.NoResetHook {
		return "", false, fmt.Errorf("unexpected branch options %+v", opts)
	}
	b.n++
	b.orients = append(b.orients, opts.OrientationTemplate)
	b.openings = append(b.openings, opts.Opening)
	return fmt.Sprintf("%s/b%d", parent, b.n), true, nil
}
func (b *fakeDepthBrancher) SessionPath(string) (string, error) { return "", nil }

type depthTurn struct{ key, text, trigger string }

type fakeDepthRunner struct {
	ch     chan depthTurn
	mu     sync.Mutex
	during func(depthTurn) // runs inside the turn, between started and done
	refuse bool
}

func newFakeDepthRunner() *fakeDepthRunner { return &fakeDepthRunner{ch: make(chan depthTurn, 100)} }

func (r *fakeDepthRunner) run(key, text, trigger string, started func(), done func(bool)) {
	t := depthTurn{key, text, trigger}
	r.mu.Lock()
	refuse, during := r.refuse, r.during
	r.mu.Unlock()
	if refuse {
		done(false)
		r.ch <- t
		return
	}
	if started != nil {
		started()
	}
	if during != nil {
		during(t)
	}
	done(true)
	r.ch <- t
}

func (r *fakeDepthRunner) wait(t *testing.T, n int) []depthTurn {
	t.Helper()
	var out []depthTurn
	for len(out) < n {
		select {
		case x := <-r.ch:
			out = append(out, x)
		case <-time.After(10 * time.Second):
			t.Fatalf("waited for %d turns, got %d: %+v", n, len(out), out)
		}
	}
	return out
}

type depthDelivery struct{ target, msg, trigger string }

type depthHarness struct {
	m        *DepthManager
	store    *memSpawnStore
	surface  *fakeDepthSurface
	branch   *fakeDepthBrancher
	runner   *fakeDepthRunner
	mu       sync.Mutex
	delivers []depthDelivery
	refuse   bool // fan-in delivery refused (shutdown)
	cancels  []string
	stops    []string
	approve  func(sk, id, text string, cb func(bool)) error
	resume   map[string]bool
	off      atomic.Bool // [depth] enabled = false (#2603)
}

const depthRoot = "test/c1"

func newDepthHarness(t *testing.T, store *memSpawnStore) *depthHarness {
	t.Helper()
	if store == nil {
		store = newMemSpawnStore()
	}
	h := &depthHarness{store: store, surface: newFakeDepthSurface(depthRoot), branch: &fakeDepthBrancher{}, runner: newFakeDepthRunner(), resume: map[string]bool{}}
	notifier := NewAsyncNotifier(func(string, string, string, string, *AgentOrigin) {}).
		WithDeliverThen(func(target, msg, trigger string, done func(bool)) {
			h.mu.Lock()
			h.delivers = append(h.delivers, depthDelivery{target, msg, trigger})
			refuse := h.refuse
			h.mu.Unlock()
			done(!refuse)
		})
	h.m = NewDepthManager(DepthDeps{
		AgentID:             "test",
		Enabled:             func() bool { return !h.off.Load() },
		Store:               store,
		Surface:             h.surface,
		Sessions:            h.branch,
		CanBranch:           func() bool { return true },
		SetNoCompact:        func(string, bool) {},
		OrientationTemplate: "LEAF {branch_key}\n{report_rule}",
		RunTurn:             h.runner.run,
		Notifier:            notifier,
		CancelAsk:           func(k string) { h.mu.Lock(); h.cancels = append(h.cancels, k); h.mu.Unlock() },
		StopTurn:            func(k string) { h.mu.Lock(); h.stops = append(h.stops, k); h.mu.Unlock() },
		CostUSD:             func(string) (float64, bool) { return 0.25, true },
		ContextTokens:       func(string) int { return 4321 },
		CanResume: func(k string) bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			v, ok := h.resume[k]
			return !ok || v
		},
		Approve: func(sk, id, text string, cb func(bool)) error {
			if h.approve != nil {
				return h.approve(sk, id, text, cb)
			}
			return fmt.Errorf("no approver")
		},
	})
	return h
}

func (h *depthHarness) deliveries() []depthDelivery {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]depthDelivery(nil), h.delivers...)
}

func depthCtx(key string) context.Context { return WithSessionKey(context.Background(), key) }

func (h *depthHarness) fanout(t *testing.T, caller string, leaves ...string) []string {
	t.Helper()
	var specs []map[string]string
	for _, l := range leaves {
		specs = append(specs, map[string]string{"title": l, "prompt": "discuss " + l})
	}
	raw, _ := json.Marshal(map[string]any{"leaves": specs, "orientation": "one line"})
	res, err := h.m.fanoutTool().Execute(depthCtx(caller), raw)
	if err != nil {
		t.Fatalf("fanout from %s: %v", caller, err)
	}
	if !strings.Contains(res.Text, fmt.Sprintf("waiting on %d leaves", len(leaves))) {
		t.Fatalf("fanout reply = %q", res.Text)
	}
	h.m.mu.Lock()
	keys := append([]string(nil), h.m.nodes[caller].Children...)
	h.m.mu.Unlock()
	return keys
}

// noTurns fails if any turn ran or is queued (#2617: a fan-out runs none).
func (h *depthHarness) noTurns(t *testing.T) {
	t.Helper()
	select {
	case x := <-h.runner.ch:
		t.Fatalf("a turn ran: %+v", x)
	case <-time.After(50 * time.Millisecond):
	}
}

func (h *depthHarness) node(key string) depthNode {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if n := h.m.nodes[key]; n != nil {
		return *n
	}
	return depthNode{}
}

func (h *depthHarness) report(t *testing.T, key, text string) error {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"text": text})
	_, err := h.m.reportTool().Execute(depthCtx(key), raw)
	return err
}

// --- tests ---

// The core loop: fan out, every leaf runs its first turn on its own branch
// with its prompt shown in its item, the leaves report once, and the parent
// gets ONE [DEPTH RESULTS] turn carrying every report (§2, §3.2, §3.4).
func TestDepthFanoutReportFanIn(t *testing.T) {
	h := newDepthHarness(t, nil)
	kids := h.fanout(t, depthRoot, "Alpha", "Beta")
	if len(kids) != 2 {
		t.Fatalf("children = %v", kids)
	}
	for _, k := range kids {
		if h.surface.leaves[k] != depthRoot {
			t.Errorf("leaf %s routed to %q, want the root conversation", k, h.surface.leaves[k])
		}
		// #2611: each leaf has its own conversation, recorded on the node and
		// named in the feed.
		if c := h.node(k).ConvID; c != "conv:"+k {
			t.Errorf("leaf %s conversation = %q, want its own", k, c)
		}
	}
	for _, n := range h.surface.lastFeed(t).nodes {
		if n.ConversationID == "" {
			t.Errorf("feed node %s has no conversation id", n.LeafID)
		}
	}
	if !strings.Contains(h.branch.orients[0], "one line") || strings.Contains(h.branch.orients[0], "{report_rule}") {
		t.Errorf("leaf orientation = %q: the call's orientation must fill {report_rule}", h.branch.orients[0])
	}
	if len(h.surface.prompts) != 2 || h.surface.prompts[0] != kids[0]+"|discuss Alpha" {
		t.Errorf("questions shown = %v", h.surface.prompts)
	}
	// #2617: no leaf turn runs at creation; each fork ends on its question.
	h.noTurns(t)
	for i, o := range h.branch.openings {
		want := []string{"discuss Alpha", "discuss Beta"}[i]
		if o == nil || o.AgentText != want || o.CallMarker != "depth_fanout" ||
			!strings.Contains(o.ToolResult, "2 leaves") || !strings.Contains(o.Fallback, want) {
			t.Errorf("fork %d opening = %+v", i, o)
		}
	}
	f := h.surface.lastFeed(t)
	if f.state != depthFanned || len(f.nodes) != 2 || f.nodes[0].Title != "Alpha" || f.nodes[0].Level != 1 {
		t.Fatalf("feed = %+v", f)
	}
	if !f.nodes[0].WaitingForUser || f.nodes[0].CostText != "$0.25" {
		t.Errorf("idle leaf feed row = %+v, want waiting with a cost", f.nodes[0])
	}

	if err := h.report(t, depthRoot, "x"); err == nil || !strings.Contains(err.Error(), "root") {
		t.Errorf("root report err = %v", err)
	}
	if err := h.report(t, kids[0], "alpha is fine"); err != nil {
		t.Fatal(err)
	}
	if err := h.report(t, kids[0], "again"); err == nil || !strings.Contains(err.Error(), "already reported") {
		t.Errorf("second report err = %v", err)
	}
	if len(h.deliveries()) != 0 {
		t.Fatal("fan-in before every leaf reported")
	}
	if err := h.report(t, kids[1], "beta needs work"); err != nil {
		t.Fatal(err)
	}
	ds := h.deliveries()
	if len(ds) != 1 || ds[0].target != depthRoot || ds[0].trigger != "depth_results" {
		t.Fatalf("deliveries = %+v", ds)
	}
	for _, want := range []string{"[DEPTH RESULTS] All 2 leaves", "## Alpha (level 1", "alpha is fine", "## Beta", "beta needs work", "$0.25"} {
		if !strings.Contains(ds[0].msg, want) {
			t.Errorf("results message lacks %q:\n%s", want, ds[0].msg)
		}
	}
	root := h.node(depthRoot)
	if root.State != depthActive || root.Results != "" {
		t.Errorf("root after fan-in = %s results=%q", root.State, root.Results)
	}
	if f := h.surface.lastFeed(t); f.state != depthActive || f.nodes[0].State != depthReported || f.nodes[0].ReportPreview != "alpha is fine" {
		t.Errorf("final feed = %+v", f)
	}
	// A done leaf the user writes to again becomes a side thread.
	h.m.LeafUserMessage(kids[0])
	if h.node(kids[0]).State != depthSide {
		t.Errorf("reopened leaf state = %s, want side", h.node(kids[0]).State)
	}
}

// Ack = report now (NQ1): the pending ask is cancelled, one [DEPTH COMPLETE] turn
// runs, and a turn that ends without depth_report reports its final text.
func TestDepthAckAutoReports(t *testing.T) {
	h := newDepthHarness(t, nil)
	kids := h.fanout(t, depthRoot, "Only")
	h.m.LeafText(kids[0], "old message before the ack")

	h.runner.mu.Lock()
	h.runner.during = func(tr depthTurn) {
		if tr.trigger == "depth_ack" {
			h.m.LeafText(tr.key, "my summary")
		}
	}
	h.runner.mu.Unlock()
	if err := h.m.Ack(depthRoot, h.node(kids[0]).LeafID); err != nil {
		t.Fatal(err)
	}
	turns := h.runner.wait(t, 1)
	if turns[0].text != DepthAckText {
		t.Fatalf("ack turn = %+v", turns[0])
	}
	if len(h.cancels) != 1 || h.cancels[0] != kids[0] {
		t.Errorf("CancelAsk calls = %v", h.cancels)
	}
	n := h.node(kids[0])
	if n.State != depthReported || !strings.HasPrefix(n.Report, "my summary") || !strings.Contains(n.Report, "auto-reported") {
		t.Fatalf("after ack: %s %q", n.State, n.Report)
	}
	if len(h.deliveries()) != 1 {
		t.Fatalf("fan-in deliveries = %d", len(h.deliveries()))
	}
	// Idempotent on a done leaf.
	if err := h.m.Ack(depthRoot, n.LeafID); err != nil {
		t.Errorf("second ack: %v", err)
	}
}

// An ack turn with nothing said after the ack still closes the leaf.
func TestDepthAckWithoutSummary(t *testing.T) {
	h := newDepthHarness(t, nil)
	kids := h.fanout(t, depthRoot, "Only")
	h.m.LeafText(kids[0], "said before the ack")
	h.m.mu.Lock()
	h.m.nodes[kids[0]].LastLineAt = time.Now().Add(-time.Hour) // well before the ack
	h.m.mu.Unlock()
	if err := h.m.Ack(depthRoot, h.node(kids[0]).LeafID); err != nil {
		t.Fatal(err)
	}
	h.runner.wait(t, 1)
	if r := h.node(kids[0]).Report; r != "(completed by the user; the leaf gave no summary)" {
		t.Errorf("report = %q", r)
	}
}

// Dismiss on a node that fanned out ends its unreported descendants first,
// stops their turns, and counts as reported for its own parent (Q3).
func TestDepthDismissNested(t *testing.T) {
	h := newDepthHarness(t, nil)
	kids := h.fanout(t, depthRoot, "Parent")
	grand := h.fanout(t, kids[0], "G1", "G2")
	if h.node(grand[0]).Level != 2 || h.node(kids[0]).State != depthFanned {
		t.Fatalf("nested fan-out: level %d, parent %s", h.node(grand[0]).Level, h.node(kids[0]).State)
	}
	if h.surface.leaves[grand[0]] != depthRoot {
		t.Errorf("nested leaf routed to %q, want the tree root", h.surface.leaves[grand[0]])
	}
	if err := h.m.Ack(depthRoot, h.node(kids[0]).LeafID); err == nil {
		t.Error("ack on a fanned node accepted")
	}
	if err := h.m.Dismiss(depthRoot, h.node(kids[0]).LeafID); err != nil {
		t.Fatal(err)
	}
	for _, k := range append(grand, kids[0]) {
		if s := h.node(k).State; s != depthDismissed {
			t.Errorf("%s state = %s, want dismissed", k, s)
		}
		if n := h.node(k); n.reportLine() != "(cancelled by the user)" {
			t.Errorf("%s report line = %q, want the cancelled wording", k, n.reportLine())
		}
	}
	if len(h.stops) != 3 {
		t.Errorf("StopTurn calls = %v, want the node and both children", h.stops)
	}
	ds := h.deliveries()
	if len(ds) != 1 || ds[0].target != depthRoot || !strings.Contains(ds[0].msg, "## Parent — cancelled by the user") {
		t.Fatalf("deliveries = %+v", ds)
	}
}

// A fan-out from level 5 waits for the user's approval; approved, it runs,
// denied, the requester stays a leaf (ruling 14:46).
func TestDepthApprovalGate(t *testing.T) {
	for _, approve := range []bool{true, false} {
		t.Run(fmt.Sprint(approve), func(t *testing.T) {
			h := newDepthHarness(t, nil)
			deep := depthRoot + "/deep"
			h.m.mu.Lock()
			h.m.nodes[depthRoot] = &depthNode{Key: depthRoot, Tree: depthRoot, State: depthFanned, Children: []string{deep}}
			h.m.nodes[deep] = &depthNode{Key: deep, LeafID: "leaf-deep", Tree: depthRoot, Parent: depthRoot, Level: 5, Title: "Deep", State: depthActive}
			h.m.mu.Unlock()
			var cb func(bool)
			var asked string
			h.approve = func(sk, id, text string, c func(bool)) error { asked, cb = sk+"|"+text, c; return nil }

			raw := json.RawMessage(`{"leaves": [{"title": "Six", "prompt": "go deeper"}]}`)
			res, err := h.m.fanoutTool().Execute(depthCtx(deep), raw)
			if err != nil || !strings.Contains(res.Text, "pending the user's approval") {
				t.Fatalf("deep fanout = %q, %v", res.Text, err)
			}
			if cb == nil || !strings.HasPrefix(asked, deep+"|") || !strings.Contains(asked, "fan out again into 1 leaves") {
				t.Fatalf("approval prompt = %q", asked)
			}
			if h.node(deep).Approval == nil {
				t.Fatal("pending approval not recorded")
			}
			cb(approve)
			if approve {
				turns := h.runner.wait(t, 1) // the requester's notice; the new leaf runs nothing (#2617)
				notice := turns[0]
				if notice.key != deep || !strings.Contains(notice.text, "approved") {
					t.Errorf("requester notice = %+v", notice)
				}
				n := h.node(deep)
				if n.State != depthFanned || len(n.Children) != 1 || h.node(n.Children[0]).Level != 6 {
					t.Errorf("after approval: %s children=%v", n.State, n.Children)
				}
			} else {
				turns := h.runner.wait(t, 1)
				if turns[0].key != deep || !strings.Contains(turns[0].text, "denied") {
					t.Errorf("deny notice = %+v", turns[0])
				}
				if h.node(deep).State != depthActive {
					t.Errorf("denied requester state = %s", h.node(deep).State)
				}
			}
			if h.node(deep).Approval != nil {
				t.Error("approval still pending after the answer")
			}
		})
	}
}

// A reset of the root drops every node of its tree: no fan-in, leaf views
// gone, the feed shows them dropped (Dick's reset ruling, §3.9).
func TestDepthResetDropsTree(t *testing.T) {
	h := newDepthHarness(t, nil)
	kids := h.fanout(t, depthRoot, "A", "B")
	if err := h.report(t, kids[0], "done"); err != nil {
		t.Fatal(err)
	}
	h.m.OnReset(kids[1]) // not a root: nothing happens
	if h.node(kids[1]).State != depthActive {
		t.Fatal("reset of a leaf touched the tree")
	}
	h.m.OnReset(depthRoot)
	if s := h.node(kids[1]).State; s != depthDropped {
		t.Errorf("open leaf after reset = %s", s)
	}
	if s := h.node(kids[0]).State; s != depthReported {
		t.Errorf("reported leaf after reset = %s, want kept", s)
	}
	if len(h.surface.leaves) != 0 {
		t.Errorf("leaf views left after reset: %v", h.surface.leaves)
	}
	if len(h.deliveries()) != 0 {
		t.Error("a reset ran the fan-in")
	}
	if root := h.node(depthRoot); root.State != depthActive || len(root.Children) != 0 {
		t.Errorf("root after reset = %+v", root)
	}
	if f := h.surface.lastFeed(t); f.state != depthActive || f.nodes[1].State != depthDropped {
		t.Errorf("feed after reset = %+v", f)
	}
}

// A restart rebuilds the leaf views, runs no leaf turn (a leaf waits for the
// user, #2617), marks a leaf whose branch cannot resume lost, and delivers the
// fan-in the last process could not (§3.5).
func TestDepthRestore(t *testing.T) {
	store := newMemSpawnStore()
	h1 := newDepthHarness(t, store)
	kids := h1.fanout(t, depthRoot, "A", "B", "C")
	if err := h1.report(t, kids[0], "a done"); err != nil {
		t.Fatal(err)
	}
	h2 := newDepthHarness(t, store)
	h2.resume[kids[1]] = false // B's branch is gone
	h2.m.Restore()
	if len(h2.surface.leaves) != 3 {
		t.Errorf("restored leaf views = %v", h2.surface.leaves)
	}
	// The restored views reuse each leaf's stored conversation (#2611).
	if h2.surface.minted != 0 {
		t.Errorf("restore minted %d leaf conversation(s), want 0 (reuse the stored ids)", h2.surface.minted)
	}
	if s := h2.node(kids[1]).State; s != depthLost {
		t.Errorf("unresumable leaf = %s, want lost", s)
	}
	h2.noTurns(t)
	if len(h2.deliveries()) != 0 {
		t.Fatal("fan-in with C still open")
	}

	// C reports; the delivery is refused (shutting down) and kept.
	h2.mu.Lock()
	h2.refuse = true
	h2.mu.Unlock()
	if err := h2.report(t, kids[2], "c done"); err != nil {
		t.Fatal(err)
	}
	if d := h2.deliveries(); len(d) != 1 || !strings.Contains(d[0].msg, "## B — lost") {
		t.Fatalf("deliveries = %+v", d)
	}
	if h2.node(depthRoot).Results == "" {
		t.Fatal("refused results not kept for the next process")
	}

	h3 := newDepthHarness(t, store)
	h3.m.Restore()
	d := h3.deliveries()
	if len(d) != 1 || d[0].target != depthRoot || !strings.Contains(d[0].msg, "c done") {
		t.Fatalf("redelivery = %+v", d)
	}
	if h3.node(depthRoot).Results != "" {
		t.Error("delivered results not cleared")
	}
}

// Refusals: a session with no app conversation cannot start a tree; a done
// leaf cannot fan out; a fanned node cannot fan out again.
func TestDepthFanoutRefusals(t *testing.T) {
	h := newDepthHarness(t, nil)
	raw := json.RawMessage(`{"leaves": [{"title": "A", "prompt": "p"}]}`)
	if _, err := h.m.fanoutTool().Execute(depthCtx("test/c999"), raw); err == nil || !strings.Contains(err.Error(), "app conversation") {
		t.Errorf("unbound root err = %v", err)
	}
	if _, err := h.m.fanoutTool().Execute(depthCtx(depthRoot), json.RawMessage(`{"leaves": [{"title": "A"}]}`)); err == nil {
		t.Error("a leaf with no prompt accepted")
	}
	kids := h.fanout(t, depthRoot, "A")
	if _, err := h.m.fanoutTool().Execute(depthCtx(depthRoot), raw); err == nil || !strings.Contains(err.Error(), "already waiting") {
		t.Errorf("fanned root err = %v", err)
	}
	if err := h.report(t, kids[0], "r"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.fanoutTool().Execute(depthCtx(kids[0]), raw); err == nil || !strings.Contains(err.Error(), "outside the tree") {
		t.Errorf("reported leaf err = %v", err)
	}
	// The shell form: --leaves as a JSON string.
	if l, err := parseDepthLeaves(json.RawMessage(`"[{\"title\":\"T\",\"prompt\":\"P\"}]"`)); err != nil || len(l) != 1 || l[0].Title != "T" {
		t.Errorf("string leaves = %+v, %v", l, err)
	}
}

// The claims board lists claims and points out overlap; it never refuses.
func TestDepthClaims(t *testing.T) {
	h := newDepthHarness(t, nil)
	kids := h.fanout(t, depthRoot, "A", "B")
	run := func(key, args string) string {
		t.Helper()
		res, err := h.m.claimsTool().Execute(depthCtx(key), json.RawMessage(args))
		if err != nil {
			t.Fatalf("claims %s: %v", args, err)
		}
		return res.Text
	}
	if got := run(kids[0], `{"text": "list"}`); got != "No claims yet." {
		t.Errorf("empty list = %q", got)
	}
	if got := run(kids[0], `{"text": "add the caching layer"}`); got != "Claimed." {
		t.Errorf("first claim = %q", got)
	}
	got := run(kids[1], `{"action": "add", "text": "cache invalidation in the caching layer"}`)
	if !strings.Contains(got, "[A] the caching layer") {
		t.Errorf("overlap = %q", got)
	}
	if got := run(depthRoot, `{}`); !strings.Contains(got, "[B] cache invalidation") {
		t.Errorf("list = %q", got)
	}
	if _, err := h.m.claimsTool().Execute(depthCtx("test/c42"), json.RawMessage(`{}`)); err == nil {
		t.Error("claims outside a tree accepted")
	}
	st, _ := h.m.statusTool().Execute(depthCtx(kids[0]), nil)
	if !strings.Contains(st.Text, "- A [") || !strings.Contains(st.Text, "(you)") {
		t.Errorf("status = %q", st.Text)
	}
}

// [depth] enabled is live (#2603): off hides the four tools from the model's
// listings and refuses every call, on brings them back, with no rebuild.
func TestDepthLiveGate(t *testing.T) {
	h := newDepthHarness(t, nil)
	tools := h.m.Tools()
	h.off.Store(true)
	params := map[string]json.RawMessage{
		"depth_fanout": json.RawMessage(`{"leaves": [{"title": "A", "prompt": "p"}]}`),
		"depth_report": json.RawMessage(`{"text": "r"}`),
		"depth_claims": json.RawMessage(`{"action": "list"}`),
		"depth_status": json.RawMessage(`{}`),
	}
	reg := NewRegistry()
	for _, tl := range tools {
		reg.Register(tl)
		if tl.Available == nil || tl.Available() {
			t.Errorf("%s available while depth mode is off", tl.Name)
		}
		if _, err := tl.Execute(depthCtx(depthRoot), params[tl.Name]); err == nil || !strings.Contains(err.Error(), "depth mode is off") {
			t.Errorf("%s while off: err = %v", tl.Name, err)
		}
	}
	if d := reg.ToolDefs(); len(d) != 0 {
		t.Errorf("ToolDefs while off = %d tools", len(d))
	}
	if e := reg.ExportedTools(); len(e) != 0 {
		t.Errorf("ExportedTools while off = %+v", e)
	}
	if n := reg.ExportedNames(); len(n) != 4 {
		t.Errorf("ExportedNames (shell functions, auto-approve) must keep the gated tools: %v", n)
	}
	if h.node(depthRoot).Key != "" {
		t.Error("a refused fan-out left a node behind")
	}

	h.off.Store(false)
	for _, tl := range tools {
		if !tl.Available() {
			t.Errorf("%s unavailable once depth mode is back on", tl.Name)
		}
	}
	if d := reg.ToolDefs(); len(d) != 4 {
		t.Errorf("ToolDefs once on = %d tools", len(d))
	}
	h.fanout(t, depthRoot, "A")
}

// A level-5 approval answered after depth mode was turned off does not fan
// out: the requester hears it was refused (#2603).
func TestDepthApprovalAfterTurnedOff(t *testing.T) {
	h := newDepthHarness(t, nil)
	deep := depthRoot + "/deep"
	h.m.mu.Lock()
	h.m.nodes[depthRoot] = &depthNode{Key: depthRoot, Tree: depthRoot, State: depthFanned, Children: []string{deep}}
	h.m.nodes[deep] = &depthNode{Key: deep, LeafID: "leaf-deep", Tree: depthRoot, Parent: depthRoot, Level: 5, Title: "Deep", State: depthActive}
	h.m.mu.Unlock()
	var cb func(bool)
	h.approve = func(sk, id, text string, c func(bool)) error { cb = c; return nil }
	if _, err := h.m.fanoutTool().Execute(depthCtx(deep), json.RawMessage(`{"leaves": [{"title": "Six", "prompt": "go"}]}`)); err != nil || cb == nil {
		t.Fatalf("deep fanout: %v (asked=%v)", err, cb != nil)
	}
	h.off.Store(true)
	cb(true)
	turns := h.runner.wait(t, 1)
	if turns[0].key != deep || !strings.Contains(turns[0].text, "turned off") {
		t.Errorf("notice = %+v", turns[0])
	}
	if n := h.node(deep); n.State != depthActive || len(n.Children) != 0 || n.Approval != nil {
		t.Errorf("after approval while off: %s children=%v approval=%v", n.State, n.Children, n.Approval)
	}
}

// Off at startup, the persisted trees stay untouched (as before #2603); turning
// depth mode on restores them then, once, and never before the startup point.
func TestDepthRestoreWaitsForEnable(t *testing.T) {
	store := newMemSpawnStore()
	h1 := newDepthHarness(t, store)
	kids := h1.fanout(t, depthRoot, "A")

	h2 := newDepthHarness(t, store)
	h2.off.Store(true)
	h2.m.OnEnable() // a config edit before the startup restore point: no-op
	if h2.node(kids[0]).Key != "" {
		t.Fatal("restored before the startup restore point")
	}
	h2.m.Restore()
	if h2.node(kids[0]).Key != "" || len(h2.surface.leaves) != 0 {
		t.Fatal("restored while depth mode is off")
	}
	h2.off.Store(false)
	h2.m.OnEnable()
	if h2.node(kids[0]).State != depthActive || len(h2.surface.leaves) != 1 {
		t.Fatalf("after enable: node=%+v leaves=%v", h2.node(kids[0]), h2.surface.leaves)
	}
	h2.m.OnEnable() // idempotent
	if len(h2.surface.leaves) != 1 {
		t.Errorf("second enable re-registered leaves: %v", h2.surface.leaves)
	}

	// Enabled at startup: Restore runs at once, as before.
	h3 := newDepthHarness(t, store)
	h3.m.Restore()
	if h3.node(kids[0]).State != depthActive {
		t.Error("enabled startup restore did not load the tree")
	}
}

// Each fan-out from the root is its own tree (#2614): a later fan-out's
// status, claims board and feed do not show the nodes of an earlier, finished
// one from the same root chat.
func TestDepthEachRootFanoutIsItsOwnTree(t *testing.T) {
	h := newDepthHarness(t, nil)
	first := h.fanout(t, depthRoot, "Philosophy", "Parenting")
	if _, err := h.m.claimsTool().Execute(depthCtx(first[0]), json.RawMessage(`{"text": "add the trolley problem"}`)); err != nil {
		t.Fatal(err)
	}
	for _, k := range first {
		if err := h.report(t, k, "done "+k); err != nil {
			t.Fatal(err)
		}
	}
	second := h.fanout(t, depthRoot, "Being a leaf", "Orientation")

	f := h.surface.lastFeed(t)
	if len(f.nodes) != 2 || f.nodes[0].Title != "Being a leaf" || f.nodes[1].Title != "Orientation" {
		t.Errorf("feed of the second fan-out = %+v, want only its two leaves", f.nodes)
	}
	st, _ := h.m.statusTool().Execute(depthCtx(second[0]), nil)
	if strings.Contains(st.Text, "Philosophy") || strings.Contains(st.Text, "Parenting") || !strings.Contains(st.Text, "Being a leaf") {
		t.Errorf("status from the second fan-out shows the first:\n%s", st.Text)
	}
	if res, err := h.m.claimsTool().Execute(depthCtx(second[0]), json.RawMessage(`{}`)); err != nil || res.Text != "No claims yet." {
		t.Errorf("claims of the second fan-out = %q, %v; want the first's board gone", res.Text, err)
	}
	if h.node(second[0]).TreeID == "" || h.node(second[0]).TreeID == h.node(first[0]).TreeID {
		t.Errorf("tree ids: first %q, second %q; want distinct", h.node(first[0]).TreeID, h.node(second[0]).TreeID)
	}
	// A nested fan-out stays in its parent's tree.
	nested := h.fanout(t, second[0], "Deeper")
	if h.node(nested[0]).TreeID != h.node(second[0]).TreeID {
		t.Errorf("nested leaf tree %q, want its parent's %q", h.node(nested[0]).TreeID, h.node(second[0]).TreeID)
	}
	if f := h.surface.lastFeed(t); len(f.nodes) != 3 {
		t.Errorf("feed with a nested leaf = %+v, want 3 nodes", f.nodes)
	}
	// The earlier tree's records are kept (until retention), not deleted.
	if h.node(first[0]).State != depthReported {
		t.Errorf("first tree's leaf = %+v, want kept as reported", h.node(first[0]))
	}
}

// Records from before #2614 carry no tree id: a restore puts the root's current
// fan-out (the descendants of its Children) in a tree of its own, so earlier
// fan-outs drop out of status, claims and the feed.
func TestDepthRestoreSplitsLegacyTree(t *testing.T) {
	store := newMemSpawnStore()
	h1 := newDepthHarness(t, store)
	old := time.Now().Add(-2 * time.Hour)
	cur := time.Now().Add(-time.Hour)
	recs := []depthNode{
		{Key: depthRoot, Tree: depthRoot, State: depthFanned, Children: []string{"test/c1/b3"}, Created: old, Updated: cur,
			Claims: []depthClaim{{Node: "Old", Text: "old claim", At: old.Add(time.Minute)}, {Node: "New", Text: "new claim", At: cur.Add(time.Minute)}}},
		{Key: "test/c1/b1", LeafID: "leaf-1", Tree: depthRoot, Parent: depthRoot, Level: 1, Title: "Old", State: depthReported, Report: "r", Created: old, Updated: old},
		{Key: "test/c1/b2", LeafID: "leaf-2", Tree: depthRoot, Parent: "test/c1/b1", Level: 2, Title: "Old nested", State: depthReported, Report: "r", Created: old.Add(time.Second), Updated: old},
		{Key: "test/c1/b3", LeafID: "leaf-3", Tree: depthRoot, Parent: depthRoot, Level: 1, Title: "New", State: depthActive, Created: cur, Updated: cur},
		{Key: "test/c1/b4", LeafID: "leaf-4", Tree: depthRoot, Parent: "test/c1/b3", Level: 2, Title: "New nested", State: depthReported, Report: "r", Created: cur.Add(time.Second), Updated: cur},
	}
	h1.m.persist(recs...)

	h2 := newDepthHarness(t, store)
	h2.m.Restore()
	f := h2.surface.lastFeed(t)
	if len(f.nodes) != 2 || f.nodes[0].Title != "New" || f.nodes[1].Title != "New nested" {
		t.Errorf("restored feed = %+v, want the current fan-out only", f.nodes)
	}
	st, _ := h2.m.statusTool().Execute(depthCtx("test/c1/b3"), nil)
	if strings.Contains(st.Text, "Old") {
		t.Errorf("status shows the earlier fan-out:\n%s", st.Text)
	}
	if res, _ := h2.m.claimsTool().Execute(depthCtx("test/c1/b3"), json.RawMessage(`{}`)); strings.Contains(res.Text, "old claim") || !strings.Contains(res.Text, "new claim") {
		t.Errorf("claims = %q, want the current fan-out's only", res.Text)
	}
	// The split is persisted: a further restart sees the same trees.
	h3 := newDepthHarness(t, store)
	h3.m.Restore()
	if id := h3.node("test/c1/b4").TreeID; id == "" || id != h3.node(depthRoot).TreeID || h3.node("test/c1/b1").TreeID == id {
		t.Errorf("persisted tree ids: root %q, current %q, old %q", h3.node(depthRoot).TreeID, id, h3.node("test/c1/b1").TreeID)
	}
}

// A finished earlier tree of a root expires on its own after the retention
// period; the root's current tree is kept.
func TestDepthRestoreExpiresEarlierTree(t *testing.T) {
	store := newMemSpawnStore()
	h1 := newDepthHarness(t, store)
	old := time.Now().Add(-depthRetention - time.Hour)
	now := time.Now()
	h1.m.persist(
		depthNode{Key: depthRoot, Tree: depthRoot, TreeID: "tree-new", State: depthFanned, Children: []string{"test/c1/b2"}, Created: old, Updated: now},
		depthNode{Key: "test/c1/b1", LeafID: "leaf-1", Tree: depthRoot, TreeID: "tree-old", Parent: depthRoot, Level: 1, Title: "Old", State: depthReported, Created: old, Updated: old},
		depthNode{Key: "test/c1/b2", LeafID: "leaf-2", Tree: depthRoot, TreeID: "tree-new", Parent: depthRoot, Level: 1, Title: "New", State: depthActive, Created: now, Updated: now},
	)
	h2 := newDepthHarness(t, store)
	h2.m.Restore()
	if h2.node("test/c1/b1").Key != "" {
		t.Error("expired earlier tree kept")
	}
	if v, _ := store.AgentMetadataByPrefix("test", depthMetaPrefix+"test/c1/b1"); len(v) != 0 {
		t.Error("expired earlier tree's record not deleted")
	}
	if h2.node("test/c1/b2").Key == "" || h2.node(depthRoot).Key == "" {
		t.Error("current tree lost")
	}
}

// A leaf's question is given as "question" (#2617); the older "prompt" name
// still works, and either is what the user sees as the leaf's first message.
func TestDepthFanoutQuestionField(t *testing.T) {
	h := newDepthHarness(t, nil)
	raw := json.RawMessage(`{"leaves": [{"title": "Q", "question": "Which colour do you want?"}, {"title": "P", "prompt": "Old name?"}]}`)
	if _, err := h.m.fanoutTool().Execute(depthCtx(depthRoot), raw); err != nil {
		t.Fatal(err)
	}
	kids := h.node(depthRoot).Children
	if len(h.surface.prompts) != 2 || h.surface.prompts[0] != kids[0]+"|Which colour do you want?" || h.surface.prompts[1] != kids[1]+"|Old name?" {
		t.Errorf("questions shown = %v", h.surface.prompts)
	}
	if n := h.node(kids[0]); n.Prompt != "Which colour do you want?" {
		t.Errorf("stored question = %q", n.Prompt)
	}
	h.noTurns(t)
	raw = json.RawMessage(`{"leaves": [{"title": "Empty"}]}`)
	if _, err := h.m.fanoutTool().Execute(depthCtx(kids[0]), raw); err == nil || !strings.Contains(err.Error(), "question") {
		t.Errorf("leaf with no question: err = %v", err)
	}
}
