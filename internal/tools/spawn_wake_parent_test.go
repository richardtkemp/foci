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

	"foci/internal/platform"
	"foci/internal/turnevent"
)

// #3163 (Dick 2026-11-18 13:13: "wake the parent"): a child spawn's result or
// send_to_spawner message to a parent spawn whose run has ended resumes that
// parent with it, and the parent's next result goes to ITS spawner. Nothing
// goes to the parent's session as an injected turn (whose output reaches no
// chat).

const wakeGrandparent = "wake/imain"

// treeAgent runs a spawn tree. The parent spawn's branch (parentKey, the
// first branch created) answers at once with what it was given. A child
// branch waits for release, optionally sends one send_to_spawner message,
// then answers.
type treeAgent struct {
	parentKey string
	release   chan struct{}
	message   func(sk string) string // nil or "" = no send_to_spawner
	spawner   func() *Spawner

	// parentGate, when set, holds the parent's first turn until closed.
	parentGate chan struct{}

	mu           sync.Mutex
	parentTurns  [][]string
	parentTurnCh chan []string
}

func newTreeAgent() *treeAgent {
	return &treeAgent{release: make(chan struct{}), parentTurnCh: make(chan []string, 256)}
}

func (a *treeAgent) HandleMessage(ctx context.Context, sk string, texts []string, _ []platform.Attachment) error {
	if sk == a.parentKey {
		a.mu.Lock()
		first := len(a.parentTurns) == 0
		a.parentTurns = append(a.parentTurns, texts)
		a.mu.Unlock()
		a.parentTurnCh <- texts
		if first && a.parentGate != nil {
			select {
			case <-a.parentGate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		turnevent.Emit(ctx, turnevent.TurnComplete{FinalText: "parent saw: " + strings.Join(texts, " || ")})
		return nil
	}
	select {
	case <-a.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if a.message != nil {
		if m := a.message(sk); m != "" {
			params, _ := json.Marshal(map[string]string{"message": m})
			if _, err := a.spawner().SendToSpawnerTool().Execute(WithSessionKey(ctx, sk), params); err != nil {
				return err
			}
		}
	}
	turnevent.Emit(ctx, turnevent.TurnComplete{FinalText: "child done on " + sk})
	return nil
}

func (a *treeAgent) allParentInput() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var b strings.Builder
	for _, t := range a.parentTurns {
		b.WriteString(strings.Join(t, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// wakeHarness: wakeGrandparent spawns the parent spawn, which spawns kids
// children; every delivery the notifier makes is recorded.
type wakeHarness struct {
	s         *Spawner
	agent     *treeAgent
	delivered chan injected
	parentKey string
}

func newWakeHarness(t *testing.T, kids int) *wakeHarness {
	t.Helper()
	return newWakeHarnessWith(t, kids, newTreeAgent())
}

func newWakeHarnessWith(t *testing.T, kids int, agent *treeAgent) *wakeHarness {
	t.Helper()
	return newWakeHarnessStore(t, kids, agent, nil)
}

// newWakeSpawner builds the harness's Spawner (one process): its tracker
// persists to store when store is non-nil.
func newWakeSpawner(h *wakeHarness, kids int, store SpawnStore) {
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, _ string, origin *AgentOrigin) {
		h.delivered <- injected{sk, msg, replyTo, origin}
	})
	var n atomic.Int64
	branches := func(_ context.Context, parent, _ string) (string, error) {
		return fmt.Sprintf("%s/b%d", parent, 1000+n.Add(1)), nil
	}
	tr := NewSpawnTracker()
	if store != nil {
		tr.SetStore(store)
	}
	h.s = NewSpawner(SpawnDeps{AgentID: "wake", MaxInherit: kids + 4, Notifier: notifier,
		Tracker: tr, CharacterBranch: branches}, func() SpawnAgent { return h.agent })
	h.agent.spawner = func() *Spawner { return h.s }
}

func newWakeHarnessStore(t *testing.T, kids int, agent *treeAgent, store SpawnStore) *wakeHarness {
	t.Helper()
	h := &wakeHarness{agent: agent, delivered: make(chan injected, 1024)}
	newWakeSpawner(h, kids, store)

	// The first branch created is the parent spawn's.
	h.parentKey = wakeGrandparent + "/b1001"
	h.agent.parentKey = h.parentKey
	gctx := WithSessionKey(context.Background(), wakeGrandparent)
	if _, err := h.s.SpawnTool().Execute(gctx, json.RawMessage(`{"name":"parent1","prompt":"review rows","background":true}`)); err != nil {
		t.Fatal(err)
	}
	if got := h.s.deps.Tracker.List(wakeGrandparent)[0].BranchKey; got != h.parentKey {
		t.Fatalf("parent branch = %q, want %q", got, h.parentKey)
	}
	// The parent's first run: it answers at once, so its children are started
	// from its branch as its run (the turn the agent records) goes on.
	pctx := WithSessionKey(context.Background(), h.parentKey)
	for i := range kids {
		params := fmt.Sprintf(`{"name":"kid%d","prompt":"check row %d","background":true}`, i, i)
		if _, err := h.s.SpawnTool().Execute(pctx, json.RawMessage(params)); err != nil {
			t.Fatalf("child %d: %v", i, err)
		}
	}
	return h
}

// next returns the next delivery, failing after a hang guard.
func (h *wakeHarness) next(t *testing.T, what string) injected {
	t.Helper()
	select {
	case d := <-h.delivered:
		return d
	case <-time.After(20 * time.Second):
		t.Fatalf("no delivery: %s", what)
		return injected{}
	}
}

// waitParentEnded waits for the parent's first [SPAWN RESULT] at the
// grandparent: its run has ended.
func (h *wakeHarness) waitParentEnded(t *testing.T) {
	t.Helper()
	d := h.next(t, "the parent's first result")
	if d.session != wakeGrandparent || !strings.Contains(d.msg, "[SPAWN RESULT]") || !strings.Contains(d.msg, "parent saw: ") {
		t.Fatalf("first delivery = %q to %q, want the parent's [SPAWN RESULT] at the grandparent", d.msg, d.session)
	}
	if snap, _ := h.s.deps.Tracker.Lookup(wakeGrandparent, h.parentKey, 0); snap.Running {
		t.Fatal("the parent spawn still runs after its result")
	}
}

// A child's [SPAWN RESULT] that arrives after its parent spawn's run ended
// resumes the parent with it, and the parent's new result reaches the
// grandparent. Nothing is injected into the parent's session.
func TestChildResultWakesEndedParentSpawn(t *testing.T) {
	t.Parallel()
	h := newWakeHarness(t, 1)
	<-h.agent.parentTurnCh
	h.waitParentEnded(t)

	close(h.agent.release)
	d := h.next(t, "the woken parent's result")
	if d.session != wakeGrandparent {
		t.Fatalf("delivery went to %q (%q), want the grandparent %q: the child's result was injected into the ended parent's session",
			d.session, d.msg, wakeGrandparent)
	}
	for _, want := range []string{"[SPAWN RESULT]", "parent saw: ", "[SYSTEM: RESUMED BY YOUR SPAWN]", "child done on " + h.parentKey + "/b"} {
		if !strings.Contains(d.msg, want) {
			t.Errorf("the parent's second result lacks %q:\n%s", want, d.msg)
		}
	}
}

// A child's send_to_spawner message to a parent spawn whose run has ended
// also wakes it; the child's own result follows into the same or a next wake.
func TestChildMessageWakesEndedParentSpawn(t *testing.T) {
	t.Parallel()
	h := newWakeHarness(t, 1)
	h.agent.message = func(string) string { return "row 7 is wrong" }
	<-h.agent.parentTurnCh
	h.waitParentEnded(t)

	close(h.agent.release)
	deadline := time.After(20 * time.Second)
	for !strings.Contains(h.agent.allParentInput(), "child done on ") {
		select {
		case d := <-h.delivered:
			if d.session != wakeGrandparent {
				t.Fatalf("delivery went to %q (%q), want only the grandparent", d.session, d.msg)
			}
		case <-h.agent.parentTurnCh:
		case <-deadline:
			t.Fatalf("the parent never took the child's result; its input:\n%s", h.agent.allParentInput())
		}
	}
	in := h.agent.allParentInput()
	for _, want := range []string{"row 7 is wrong", "MESSAGE FROM YOUR SPAWN", "[SYSTEM: RESUMED BY YOUR SPAWN]"} {
		if !strings.Contains(in, want) {
			t.Errorf("the parent's input lacks %q:\n%s", want, in)
		}
	}
	// The child sent its message before its result: the parent reads them so.
	if m, r := strings.Index(in, "row 7 is wrong"), strings.Index(in, "child done on "); m < 0 || m > r {
		t.Errorf("the child's message (at %d) does not come before its result (at %d):\n%s", m, r, in)
	}
}

// While the parent spawn's run goes on (here its first turn is held, as a
// long protected turn held it on the work Mac), its children's results and
// messages queue for its next step, not in its session's inbox: they run as
// a follow-up turn, and the parent reports ONE result, with all of them, to
// the grandparent.
func TestChildBurstToRunningParentSpawnJoinsItsRun(t *testing.T) {
	t.Parallel()
	const kids = 40
	agent := newTreeAgent()
	agent.parentGate = make(chan struct{})
	agent.message = func(sk string) string { return "note from " + sk }
	h := newWakeHarnessWith(t, kids, agent)
	<-h.agent.parentTurnCh
	close(h.agent.release)
	// Every child has reported once all their results are queued.
	deadline := time.After(60 * time.Second)
	for {
		snap, _ := h.s.deps.Tracker.Lookup(wakeGrandparent, h.parentKey, 0)
		if snap.Pending >= 2*kids {
			break
		}
		select {
		case d := <-h.delivered:
			t.Fatalf("delivery to %q while the parent runs (%q), want every child's report queued for the parent", d.session, d.msg)
		case <-deadline:
			t.Fatalf("after 60s the parent has %d of %d reports queued", snap.Pending, 2*kids)
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(agent.parentGate)
	d := h.next(t, "the parent's one result")
	if d.session != wakeGrandparent {
		t.Fatalf("delivery went to %q (%q), want the grandparent", d.session, d.msg)
	}
	if n, m := strings.Count(d.msg, "child done on "), strings.Count(d.msg, "note from "); n != kids || m != kids {
		t.Errorf("the parent's result carries %d results and %d messages, want %d each:\n%s", n, m, kids, d.msg)
	}
	if strings.Contains(h.agent.allParentInput(), "RESUMED BY YOUR SPAWN") {
		t.Error("a running parent was resumed, want its children's reports in its own run")
	}
}

// A burst of children's results and messages to an ended parent spawn, more
// than the agent inbox holds (inboxChanSize, 64), all reach the parent: none
// is dropped, none goes to the parent's session as an injected turn, and the
// parent's results go to the grandparent.
func TestChildBurstToEndedParentSpawnDropsNothing(t *testing.T) {
	t.Parallel()
	const kids = 40 // 80 deliveries: a result and a message each
	h := newWakeHarness(t, kids)
	h.agent.message = func(sk string) string { return "note from " + sk }
	<-h.agent.parentTurnCh
	h.waitParentEnded(t)

	close(h.agent.release)
	deadline := time.After(60 * time.Second)
	for {
		in := h.agent.allParentInput()
		if strings.Count(in, "child done on ") >= kids && strings.Count(in, "note from ") >= kids {
			break
		}
		select {
		case d := <-h.delivered:
			if d.session != wakeGrandparent {
				t.Fatalf("delivery went to %q (%q), want only the grandparent", d.session, d.msg)
			}
		case <-h.agent.parentTurnCh:
		case <-deadline:
			t.Fatalf("after 60s the parent took %d results and %d messages of %d each",
				strings.Count(in, "child done on "), strings.Count(in, "note from "), kids)
		}
	}
	in := h.agent.allParentInput()
	for i := 2; i <= kids+1; i++ {
		if key := fmt.Sprintf("%s/b%d", h.parentKey, 1000+i); !strings.Contains(in, "child done on "+key) {
			t.Errorf("the result of child %s never reached the parent", key)
		}
	}
}
