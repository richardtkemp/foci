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

// registerLeaf registers branch as a leaf of root and returns its binding.
func registerLeaf(t *testing.T, h *Hub, root *convBinding, branch, leafID, title string) *convBinding {
	t.Helper()
	convID, err := h.registerDepthLeaf(root.sessionKey, branch, leafID, title, "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	h.mu.RLock()
	lb := h.convs[convID]
	h.mu.RUnlock()
	if lb == nil || h.bindingForSession(branch) != lb {
		t.Fatalf("leaf conversation %s is not bound to %s", convID, branch)
	}
	return lb
}

// A leaf is a conversation of its own (#2611): its session's frames land on
// its own stream, unstamped, attached to the devices already connected, and
// the depth manager hears its messages and busy flips. The root's stream and
// activity never move.
func TestDepthLeafConversation_OwnStream(t *testing.T) {
	h := newTestHub()
	ctl := withDepthCtl(t)
	const agentID = "ag"
	client := fakeClientFor(h)
	h.addClient(client)
	root := h.ensureBinding(client, agentID, "conv-root")
	branch := root.sessionKey + "/b1"
	lb := registerLeaf(t, h, root, branch, "leaf-1", "Point A")
	drain(t, client) // the roster push

	conn := &appConn{hub: h, agentID: agentID}
	if err := conn.SendToSession(branch, "hello from the leaf"); err != nil {
		t.Fatal(err)
	}
	lb.send(fap.Meta{ConversationID: lb.convID, Model: "m"})
	lb.setTurnActivity(fap.ActivityKindTyping)
	lb.setTurnActivity(fap.ActivityKindIdle)

	got := drain(t, client)
	want := []string{fap.TypeMessage, fap.TypeMeta, fap.TypeActivity, fap.TypeActivity}
	if len(got) != len(want) {
		t.Fatalf("frames = %v, want %v", types(got), want)
	}
	for i, d := range got {
		if d.t != want[i] {
			t.Fatalf("frame %d = %s, want %s (all %v)", i, d.t, want[i], types(got))
		}
		if d.d["conversationId"] != lb.convID || d.d["leafId"] != nil {
			t.Errorf("frame %d (%s) conv=%v leafId=%v, want the leaf's own conversation, unstamped", i, d.t, d.d["conversationId"], d.d["leafId"])
		}
	}
	if root.currentSeq() != 0 {
		t.Errorf("root seq = %d, want 0: the leaf has its own stream", root.currentSeq())
	}
	ctl.mu.Lock()
	if len(ctl.texts) != 1 || ctl.texts[0] != branch+"|hello from the leaf" {
		t.Errorf("LeafText calls = %v", ctl.texts)
	}
	if len(ctl.busy) != 2 || !ctl.busy[0] || ctl.busy[1] {
		t.Errorf("LeafBusy calls = %v, want [true false]", ctl.busy)
	}
	ctl.mu.Unlock()
	root.mu.Lock()
	kind := root.activitySent.Kind
	root.mu.Unlock()
	if kind != "" {
		t.Errorf("root activity = %q, want untouched", kind)
	}
	// The push preview names the leaf.
	lb.mu.Lock()
	preview := lb.lastPreview
	lb.mu.Unlock()
	if preview != "Point A: hello from the leaf" {
		t.Errorf("leaf preview = %q, want the title prefixed", preview)
	}
}

// The roster marks a leaf's conversation with its root, and a session-blind
// delivery never picks it as the most recent chat.
func TestDepthLeafConversation_RosterAndDelivery(t *testing.T) {
	h := newTestHub()
	registerFakeAgent(h, "ag")
	root := h.ensureBinding(nil, "ag", "conv-root")
	root.send(fap.ServerMessage{ConversationID: "conv-root", MessageID: "m0", Role: "agent", Text: "older"})
	lb := registerLeaf(t, h, root, root.sessionKey+"/b1", "leaf-1", "A")
	lb.send(fap.ServerMessage{ConversationID: lb.convID, MessageID: "m1", Role: "agent", Text: "newer"})

	var seen bool
	for _, a := range h.agentRoster() {
		for _, c := range a.Conversations {
			switch c.ID {
			case lb.convID:
				seen = true
				if c.DepthRootID != "conv-root" {
					t.Errorf("leaf roster row depthRootId = %q, want conv-root", c.DepthRootID)
				}
			case "conv-root":
				if c.DepthRootID != "" {
					t.Errorf("root roster row depthRootId = %q, want empty", c.DepthRootID)
				}
			}
		}
	}
	if !seen {
		t.Fatal("the leaf's conversation is not in the roster")
	}
	if b, rung := h.deliverBinding("ag"); b != root {
		t.Errorf("deliverBinding = %s (%s), want the root, never a leaf", b.convID, rung)
	}
}

// A streamed leaf turn reports its whole text once, at text.end.
func TestDepthLeafView_StreamedTurnReportsFinalText(t *testing.T) {
	h := newTestHub()
	ctl := withDepthCtl(t)
	root := h.ensureBinding(nil, "ag", "conv-root")
	branch := root.sessionKey + "/b1"
	registerLeaf(t, h, root, branch, "leaf-1", "A")
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

// A message sent to a leaf's conversation runs on the leaf's branch session,
// echoes there, and the controller hears of it first.
func TestDepthRouteUserTurn_ToLeaf(t *testing.T) {
	h := newTestHub()
	ctl := withDepthCtl(t)
	fa := registerFakeAgent(h, "ag")
	c := fakeClientFor(h)
	h.addClient(c)
	root := h.ensureBinding(c, "ag", "conv-1")
	branch := root.sessionKey + "/b9"
	lb := registerLeaf(t, h, root, branch, "leaf-9", "Nine")
	drain(t, c)

	h.routeUserTurn(c, lb.convID, "ag", "my reply", nil, nil, "env-1", 1, platform.SteerDefault, false)
	if fa.env == nil || fa.env.SessionKey != branch {
		t.Fatalf("enqueued %+v, want session %s", fa.env, branch)
	}
	got := framesOf(drain(t, c), fap.TypeMessage)
	if len(got) != 1 || got[0].d["conversationId"] != lb.convID || got[0].d["role"] != "user" {
		t.Fatalf("echo frames = %+v", got)
	}
	ctl.mu.Lock()
	if len(ctl.userMsgs) != 1 || ctl.userMsgs[0] != branch {
		t.Errorf("LeafUserMessage = %v", ctl.userMsgs)
	}
	ctl.mu.Unlock()
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

// A re-registration (restart rebuild, retitle) keeps the leaf's conversation;
// unregistering drops the hooks but keeps the conversation; a session with a
// conversation of its own (a facet) is never made a leaf, nor is a leaf a root.
func TestDepthLeaf_ReregisterUnregisterAndGuards(t *testing.T) {
	h := newTestHub()
	ctl := withDepthCtl(t)
	root := h.ensureBinding(nil, "ag", "conv-root")
	other := h.ensureBinding(nil, "ag", "conv-other")
	if _, err := h.registerDepthLeaf(root.sessionKey, other.sessionKey, "leaf-x", "X", ""); err == nil {
		t.Fatal("a session with its own conversation was made a leaf")
	}
	branch := root.sessionKey + "/b2"
	lb := registerLeaf(t, h, root, branch, "leaf-2", "Two")
	again, err := h.registerDepthLeaf(root.sessionKey, branch, "leaf-2", "Two, retitled", "")
	if err != nil || again != lb.convID {
		t.Fatalf("re-register = %q, %v; want the same conversation %s", again, err, lb.convID)
	}
	if _, err := h.registerDepthLeaf(branch, branch+"/g", "leaf-g", "G", ""); err == nil {
		t.Fatal("a leaf's conversation was used as a tree root")
	}
	h.unregisterDepthLeaf(branch)
	if h.bindingForSession(branch) != lb || !lb.isDepthLeaf() {
		t.Fatal("the leaf's conversation went with its view")
	}
	lb.send(fap.ServerMessage{ConversationID: lb.convID, MessageID: "m1", Role: "agent", Text: "after"})
	ctl.mu.Lock()
	n := len(ctl.texts)
	ctl.mu.Unlock()
	if n != 0 {
		t.Errorf("LeafText after unregister = %d call(s), want none", n)
	}
	if _, err := h.registerDepthLeaf("no-such-root", branch+"/x", "leaf-3", "Three", ""); err == nil {
		t.Fatal("registered a leaf of an unbound root")
	}
}

// A leaf minted while no device is connected renders its asks the way the
// root's devices can show them.
func TestDepthLeaf_SeedsFeaturesFromRoot(t *testing.T) {
	h := newTestHub()
	root := h.ensureBinding(nil, "ag", "conv-root")
	c := fakeClient()
	c.features = featureSet([]string{featureInteractiveBatch})
	root.attach(c)
	lb := registerLeaf(t, h, root, root.sessionKey+"/b1", "leaf-1", "A")
	if !lb.supportsFeature(featureInteractiveBatch) {
		t.Fatal("leaf conversation does not carry the root's batch capability")
	}
}

// After a restart the leaf's conversation comes back as a leaf from its
// chat_metadata alone (before the depth manager re-registers its view): bound
// to the branch session, hidden, and never registered as a routable chat.
func TestDepthLeaf_RestartRehydratesFromChatMetadata(t *testing.T) {
	idx := newTestIndex(t)
	h1 := newTestHub()
	h1.deps = platform.ProviderDeps{SessionIndex: idx}
	root := h1.ensureBinding(nil, "ag", "conv-root")
	branch := root.sessionKey + "/b1"
	lb := registerLeaf(t, h1, root, branch, "leaf-1", "A")

	h2 := newTestHub()
	h2.deps = platform.ProviderDeps{SessionIndex: idx}
	b := h2.ensureBinding(nil, "ag", lb.convID)
	if b.sessionKey != branch || !b.isDepthLeaf() || b.info().DepthRootID != "conv-root" {
		t.Fatalf("restored leaf: session %q, depthRoot %q; want %q under conv-root", b.sessionKey, b.info().DepthRootID, branch)
	}
	if v, _ := idx.GetChatMetadata("ag", "app", b.chatID, "registered"); v != "" {
		t.Errorf("leaf chat registered = %q, want no row: a leaf is not a routable chat", v)
	}
}
