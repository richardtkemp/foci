package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"foci/internal/agent"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// envRecorder records the envelopes an agent's Enqueue sees (by session).
type envRecorder struct {
	mu   sync.Mutex
	envs []agent.Envelope
}

func (r *envRecorder) observe(env agent.Envelope) {
	r.mu.Lock()
	r.envs = append(r.envs, env)
	r.mu.Unlock()
}

func (r *envRecorder) last(t *testing.T) agent.Envelope {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.envs) == 0 {
		t.Fatal("no envelope reached Enqueue")
	}
	return r.envs[len(r.envs)-1]
}

// idleAgent is an agent whose inbox is never started: an envelope Enqueue
// accepts stays queued and never runs, so a test can drive its hooks itself.
func idleAgent(t *testing.T, rec *envRecorder) *agent.Agent {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	ag := &agent.Agent{
		Sessions:  session.NewStore(t.TempDir()),
		Tools:     tools.NewRegistry(),
		Bootstrap: workspace.NewBootstrap(t.TempDir(), nil),
		Model:     "test-model",
	}
	ag.SessionIndex = idx
	ag.SetEnqueueObserver(rec.observe)
	return ag
}

// TestSkipHolds_Classification pins which deliveries skip the injection holds
// (#3114, Dick 2026-11-14): within one agent, a message or an async tool
// result ("async_notify", "depth_results") and send_to_session between its
// own sessions; never a delivery to another agent, and never another trigger
// (tmux_watch here; wakes, cron, foci send and webhooks do not come through
// these constructors).
func TestSkipHolds_Classification(t *testing.T) {
	clutchRec, scoutRec := &envRecorder{}, &envRecorder{}
	clutch, scout := idleAgent(t, clutchRec), idleAgent(t, scoutRec)
	resolver := func(id string) *agentInstance {
		switch id {
		case "clutch":
			return &agentInstance{id: "clutch", ag: clutch}
		case "scout":
			return &agentInstance{id: "scout", ag: scout}
		}
		return nil
	}
	cm := chitConnMgr{}
	ctx := context.Background()
	n := newAsyncNotifier(func() *agent.Agent { return clutch }, "clutch", resolver, ctx, cm, nil)

	check := func(name string, rec *envRecorder, wantSkip bool, wantText string) {
		t.Helper()
		env := rec.last(t)
		if env.Inject == nil {
			t.Fatalf("%s: not an injection", name)
		}
		if env.Inject.SkipHolds != wantSkip {
			t.Errorf("%s: SkipHolds = %v, want %v", name, env.Inject.SkipHolds, wantSkip)
		}
		if env.Text != wantText {
			t.Errorf("%s: Text = %q, want %q", name, env.Text, wantText)
		}
	}

	n.InjectToAgent("clutch/c1", "bg result", "", "async_notify")
	check("async result, own session", clutchRec, true, "bg result")

	n.InjectToAgent("clutch/c1", "watch fired", "", "tmux_watch")
	check("tmux_watch, own session", clutchRec, false, "")

	n.InjectToAgent("scout/c42", "for scout", "", "async_notify")
	check("async_notify to another agent", scoutRec, false, "")

	n.InjectToAgentThen("clutch/c1", "[DEPTH RESULTS] x", "depth_results", func(bool) {})
	check("depth results, own session", clutchRec, true, "[DEPTH RESULTS] x")

	n.InjectAgentMessage("clutch/c2", "to sibling", "clutch/c1", tools.NewAgentOrigin("clutch/c1", "to sibling"))
	check("reply_to=caller target, own agent (captured: never steers)", clutchRec, true, "")

	n.InjectAgentMessage("scout/c42", "to scout", "clutch/c1", tools.NewAgentOrigin("clutch/c1", "to scout"))
	check("reply_to=caller target, other agent", scoutRec, false, "")

	sn := newSessionNotifyFn(resolver, ctx, cm, "session_notify", nil)
	origin := tools.NewAgentOrigin("clutch/c1", "hi")
	sn("clutch/c3", "to own session", &origin)
	check("session_notify, own agent", clutchRec, true, "to own session")
	sn("scout/c42", "to scout session", &origin)
	check("session_notify, other agent", scoutRec, false, "")

	// The text a steer folds in is origin.SteerText, when the sender gave
	// one (the message without its [[NO_RESPONSE]] instruction, #3114).
	steered := tools.NewAgentOrigin("clutch/c1", "hi")
	steered.SteerText = "fold variant"
	sn("clutch/c3", "standalone variant", &steered)
	check("session_notify with a fold variant", clutchRec, true, "fold variant")
	n.InjectAgentMessage("clutch/c1", "standalone variant", "", steered)
	check("spawn message with a fold variant", clutchRec, true, "fold variant")
	n.InjectAgentMessage("scout/c42", "standalone variant", "", steered)
	check("fold variant to another agent (never steers)", scoutRec, false, "")
}

// TestSkipHolds_SteeredMarksDelivered: a steered [SPAWN RESULT] still counts
// as delivered for the restart store (Dick, Q41), and the message's chit
// fires with it — the work the injection's Run would have done around a turn
// of its own.
func TestSkipHolds_SteeredMarksDelivered(t *testing.T) {
	rec := &envRecorder{}
	clutch := idleAgent(t, rec)
	resolver := func(id string) *agentInstance {
		if id == "clutch" {
			return &agentInstance{id: "clutch", ag: clutch}
		}
		return nil
	}
	n := newAsyncNotifier(func() *agent.Agent { return clutch }, "clutch", resolver, context.Background(), chitConnMgr{}, nil)

	var delivered []bool
	n.InjectToAgentThen("clutch/c1", "[SPAWN RESULT] done", "async_notify", func(ok bool) { delivered = append(delivered, ok) })
	env := rec.last(t)
	if env.Inject.Steered == nil {
		t.Fatal("no Steered hook on a [SPAWN RESULT] within the agent")
	}
	env.Inject.Steered()
	if len(delivered) != 1 || !delivered[0] {
		t.Fatalf("done calls = %v after a steer, want [true]", delivered)
	}

	chits := newChitRecorder()
	deliverToSessionChatThen(clutch, agent.WithSkipHolds(context.Background()), "async_notify", chitConnMgr{}, "clutch", "clutch/c1", "msg",
		newAgentChit(chits.deliver, tools.NewAgentOrigin("clutch/c9", "from spawn")), "", nil)
	rec.last(t).Inject.Steered()
	if got := chits.snapshot(); len(got) != 1 || got[0].chat != "clutch/c1" {
		t.Fatalf("chits after a steer = %+v, want one on clutch/c1", got)
	}
}
