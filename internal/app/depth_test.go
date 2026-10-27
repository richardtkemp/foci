package app

import (
	"sync"
	"testing"

	"foci/internal/agent"
	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/turnevent"
)

// fakeDepthCtl records the hub's depth callbacks.
type fakeDepthCtl struct {
	mu       sync.Mutex
	userMsgs []string
	texts    []string
	busy     []bool
	acks     []string
	dismiss  []string
}

func (f *fakeDepthCtl) LeafUserMessage(k string) {
	f.mu.Lock()
	f.userMsgs = append(f.userMsgs, k)
	f.mu.Unlock()
}
func (f *fakeDepthCtl) LeafText(k, t string) {
	f.mu.Lock()
	f.texts = append(f.texts, k+"|"+t)
	f.mu.Unlock()
}
func (f *fakeDepthCtl) LeafBusy(_ string, b bool) {
	f.mu.Lock()
	f.busy = append(f.busy, b)
	f.mu.Unlock()
}
func (f *fakeDepthCtl) Ack(r, l string) error {
	f.mu.Lock()
	f.acks = append(f.acks, r+"|"+l)
	f.mu.Unlock()
	return nil
}
func (f *fakeDepthCtl) Dismiss(r, l string) error {
	f.mu.Lock()
	f.dismiss = append(f.dismiss, r+"|"+l)
	f.mu.Unlock()
	return nil
}

func withDepthCtl(t *testing.T) *fakeDepthCtl {
	t.Helper()
	f := &fakeDepthCtl{}
	SetDepthController(f)
	t.Cleanup(func() { SetDepthController(nil) })
	return f
}

// A leaf view lands every frame of the leaf's session in the ROOT
// conversation, stamped with the leaf id, and drops the frames that describe
// the conversation as a whole (#2588 §3.1).
func TestDepthLeafView_StampsAndForwardsToRoot(t *testing.T) {
	h := newTestHub()
	ctl := withDepthCtl(t)
	const agentID = "ag"
	root := h.ensureBinding(nil, agentID, "conv-root")
	client := fakeClient()
	root.attach(client)
	branch := root.sessionKey + "/b1"
	if err := h.registerDepthLeaf(root.sessionKey, branch, "leaf-1", "Point A"); err != nil {
		t.Fatalf("register: %v", err)
	}

	conn := &appConn{hub: h, agentID: agentID}
	if err := conn.SendToSession(branch, "hello from the leaf"); err != nil {
		t.Fatal(err)
	}
	lb := h.bindingForSession(branch)
	lb.send(fap.Meta{ConversationID: lb.convID, Model: "m"})
	lb.setCacheExpiry(12345)
	lb.setTurnActivity(fap.ActivityKindTyping)
	lb.setTurnActivity(fap.ActivityKindIdle)

	got := drain(t, client)
	want := []string{fap.TypeMessage, fap.TypeActivity, fap.TypeActivity}
	if len(got) != len(want) {
		t.Fatalf("frames = %v, want %v", types(got), want)
	}
	for i, d := range got {
		if d.t != want[i] {
			t.Fatalf("frame %d = %s, want %s (all %v)", i, d.t, want[i], types(got))
		}
		if d.d["leafId"] != "leaf-1" || d.d["conversationId"] != "conv-root" {
			t.Errorf("frame %d (%s) leafId=%v conv=%v, want leaf-1 on conv-root", i, d.t, d.d["leafId"], d.d["conversationId"])
		}
	}
	if got[0].d["leafTitle"] != "Point A" {
		t.Errorf("message leafTitle = %v", got[0].d["leafTitle"])
	}
	if root.currentSeq() != 3 {
		t.Errorf("root seq = %d, want 3: the leaf's frames are the root stream's", root.currentSeq())
	}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	if len(ctl.texts) != 1 || ctl.texts[0] != branch+"|hello from the leaf" {
		t.Errorf("LeafText calls = %v", ctl.texts)
	}
	if len(ctl.busy) != 2 || !ctl.busy[0] || ctl.busy[1] {
		t.Errorf("LeafBusy calls = %v, want [true false]", ctl.busy)
	}
	// The root's own activity never moved.
	root.mu.Lock()
	kind := root.activitySent.Kind
	root.mu.Unlock()
	if kind != "" {
		t.Errorf("root activity = %q, want untouched", kind)
	}
}

// A streamed leaf turn reports its whole text once, at text.end.
func TestDepthLeafView_StreamedTurnReportsFinalText(t *testing.T) {
	h := newTestHub()
	ctl := withDepthCtl(t)
	root := h.ensureBinding(nil, "ag", "conv-root")
	branch := root.sessionKey + "/b1"
	if err := h.registerDepthLeaf(root.sessionKey, branch, "leaf-1", "A"); err != nil {
		t.Fatal(err)
	}
	conn := &appConn{hub: h, agentID: "ag"}
	sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: branch})
	if sink == nil {
		t.Fatal("no sink for a leaf session")
	}
	ctx := t.Context()
	sink.Emit(ctx, turnevent.TurnStart{})
	sink.Emit(ctx, turnevent.TurnComplete{FinalText: "the answer"})
	cleanup()
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	if len(ctl.texts) != 1 || ctl.texts[0] != branch+"|the answer" {
		t.Errorf("LeafText calls = %v, want one with the final text", ctl.texts)
	}
}

// A message with a leafId runs on the leaf's branch session; its echo is
// stamped; the controller hears of it first. An unknown leaf is refused.
func TestDepthRouteUserTurn_ToLeaf(t *testing.T) {
	h := newTestHub()
	ctl := withDepthCtl(t)
	fa := registerFakeAgent(h, "ag")
	c := fakeClientFor(h)
	root := h.ensureBinding(c, "ag", "conv-1")
	branch := root.sessionKey + "/b9"
	if err := h.registerDepthLeaf(root.sessionKey, branch, "leaf-9", "Nine"); err != nil {
		t.Fatal(err)
	}

	h.routeUserTurn(c, "conv-1", "ag", "leaf-9", "my reply", nil, nil, "env-1", 1, platform.SteerDefault, false)
	if fa.env == nil || fa.env.SessionKey != branch {
		t.Fatalf("enqueued %+v, want session %s", fa.env, branch)
	}
	got := framesOf(drain(t, c), fap.TypeMessage)
	if len(got) != 1 || got[0].d["leafId"] != "leaf-9" || got[0].d["role"] != "user" {
		t.Fatalf("echo frames = %+v", got)
	}
	ctl.mu.Lock()
	if len(ctl.userMsgs) != 1 || ctl.userMsgs[0] != branch {
		t.Errorf("LeafUserMessage = %v", ctl.userMsgs)
	}
	ctl.mu.Unlock()

	fa.env = nil
	h.routeUserTurn(c, "conv-1", "ag", "leaf-gone", "hi", nil, nil, "env-2", 2, platform.SteerDefault, false)
	if fa.env != nil {
		t.Fatalf("a message to an unknown leaf was enqueued: %+v", fa.env)
	}
	if errs := framesOf(drain(t, c), fap.TypeError); len(errs) != 1 || errs[0].d["code"] != "no_leaf" {
		t.Errorf("error frames = %+v, want one no_leaf", errs)
	}
}

// depth.ack / depth.dismiss decode and reach the controller with the root's
// session key.
func TestDepthActions_ReachController(t *testing.T) {
	h := newTestHub()
	ctl := withDepthCtl(t)
	registerFakeAgent(h, "ag")
	c := fakeClientFor(h)
	root := h.ensureBinding(c, "ag", "conv-1")

	h.dispatchInbound(c, []byte(`{"v":1,"t":"depth.ack","id":"e1","seq":1,"d":{"conversationId":"conv-1","leafId":"leaf-1"}}`))
	h.dispatchInbound(c, []byte(`{"v":1,"t":"depth.dismiss","id":"e2","seq":2,"d":{"conversationId":"conv-1","leafId":"leaf-2"}}`))
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	if len(ctl.acks) != 1 || ctl.acks[0] != root.sessionKey+"|leaf-1" {
		t.Errorf("acks = %v", ctl.acks)
	}
	if len(ctl.dismiss) != 1 || ctl.dismiss[0] != root.sessionKey+"|leaf-2" {
		t.Errorf("dismiss = %v", ctl.dismiss)
	}
}

// Unregistering a leaf removes its routing; a branch with a conversation of
// its own (a facet) is never turned into a leaf.
func TestDepthLeaf_UnregisterAndFacetGuard(t *testing.T) {
	h := newTestHub()
	root := h.ensureBinding(nil, "ag", "conv-root")
	other := h.ensureBinding(nil, "ag", "conv-other")
	if err := h.registerDepthLeaf(root.sessionKey, other.sessionKey, "leaf-x", "X"); err == nil {
		t.Fatal("a session with its own conversation was made a leaf")
	}
	branch := root.sessionKey + "/b2"
	if err := h.registerDepthLeaf(root.sessionKey, branch, "leaf-2", "Two"); err != nil {
		t.Fatal(err)
	}
	if h.depthLeaf("conv-root", "leaf-2") == nil {
		t.Fatal("leaf not found by id")
	}
	h.unregisterDepthLeaf(branch)
	if h.bindingForSession(branch) != nil || h.depthLeaf("conv-root", "leaf-2") != nil {
		t.Fatal("leaf still routed after unregister")
	}
	if err := h.registerDepthLeaf("no-such-root", branch, "leaf-3", "Three"); err == nil {
		t.Fatal("registered a leaf of an unbound root")
	}
}

// A leaf's asks render as the batched form when the ROOT's devices can show
// it: the leaf view answers capability checks for the root.
func TestDepthLeaf_FeaturesAreTheRoots(t *testing.T) {
	h := newTestHub()
	root := h.ensureBinding(nil, "ag", "conv-root")
	c := fakeClient()
	c.features = featureSet([]string{featureInteractiveBatch})
	root.attach(c)
	branch := root.sessionKey + "/b1"
	if err := h.registerDepthLeaf(root.sessionKey, branch, "leaf-1", "A"); err != nil {
		t.Fatal(err)
	}
	if !h.bindingForSession(branch).supportsFeature(featureInteractiveBatch) {
		t.Fatal("leaf view does not report the root's batch capability")
	}
}
