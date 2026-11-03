package app

// #1286: agent-to-agent messages (send_to_session) surface in the receiving
// chat as ExternalPrompt chits carrying the sender's ids. These tests pin the
// app-side half: the frame shape, and the bound-conversations-only rule the
// chit shares with foci send's DeliverExternalPrompt.

import (
	"testing"

	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/tools"
)

// TestDeliverAgentMessage_BoundSessionGetsFrameWithFromFields proves a
// send_to_session chit reaches the conversation bound to the receiving
// session as one external.prompt frame whose from fields name the sending
// agent and session — and nothing else changes on the wire (same frame type
// foci send uses, so push/unread classification is untouched).
func TestDeliverAgentMessage_BoundSessionGetsFrameWithFromFields(t *testing.T) {
	h := newTestHub()
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })

	b := h.ensureBinding(nil, "scout", "conv-1")
	client := fakeClient()
	b.attach(client)

	DeliverAgentMessage(b.key(), tools.AgentOrigin{SessionKey: "clutch/c111", AgentID: "clutch", Text: "what changed overnight?"})

	got := drain(t, client)
	if len(got) != 1 || got[0].t != fap.TypeExternalPrompt {
		t.Fatalf("frames = %v, want exactly one external.prompt", types(got))
	}
	d := got[0].d
	if d["text"] != "what changed overnight?" {
		t.Errorf("text = %v, want the caller's raw message", d["text"])
	}
	if d["fromAgent"] != "clutch" {
		t.Errorf("fromAgent = %v, want clutch (the sending agent's id)", d["fromAgent"])
	}
	if d["fromSession"] != "clutch/c111" {
		t.Errorf("fromSession = %v, want clutch/c111 (the sending session's key)", d["fromSession"])
	}
}

// TestDeliverAgentMessage_ChildNameRidesAsFromChild pins #2836: a spawn's
// message to its spawner carries the spawn's name as fromChild (the app's
// "from <spawn name>" label), and an ordinary agent message carries none.
func TestDeliverAgentMessage_ChildNameRidesAsFromChild(t *testing.T) {
	h := newTestHub()
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })

	b := h.ensureBinding(nil, "clutch", "conv-1")
	client := fakeClient()
	b.attach(client)

	DeliverAgentMessage(b.key(), tools.AgentOrigin{SessionKey: "clutch/c1/b2", AgentID: "clutch", Text: "halfway", ChildName: "todo-2826"})
	DeliverAgentMessage(b.key(), tools.AgentOrigin{SessionKey: "scout/c4", AgentID: "scout", Text: "plain"})

	got := drain(t, client)
	if len(got) != 2 {
		t.Fatalf("frames = %v, want two external.prompt", types(got))
	}
	if got[0].d["fromChild"] != "todo-2826" {
		t.Errorf("fromChild = %v, want todo-2826", got[0].d["fromChild"])
	}
	if _, ok := got[1].d["fromChild"]; ok {
		t.Errorf("plain agent message has fromChild %v, want the field absent", got[1].d["fromChild"])
	}
}

// TestDeliverAgentMessage_ChildSenderIsNamed pins the general case of #2836:
// an ordinary agent message (send_to_session, no name on the origin) from a
// running spawn of the receiving session is labelled with the spawn's name.
func TestDeliverAgentMessage_ChildSenderIsNamed(t *testing.T) {
	h := newTestHub()
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })

	b := h.ensureBinding(nil, "dlvchild", "conv-1")
	client := fakeClient()
	b.attach(client)

	tracker := tools.NewSpawnTracker()
	tools.RegisterSpawner("dlvchild", tools.NewSpawner(tools.SpawnDeps{AgentID: "dlvchild", Tracker: tracker}, nil))
	t.Cleanup(func() { tools.UnregisterSpawner("dlvchild") })
	tracker.Add(b.key(), tools.RunningSpawn{ID: "spawn-z", BranchKey: b.key() + "/b7", Name: "audit-run"}, func() {})

	DeliverAgentMessage(b.key(), tools.NewAgentOrigin(b.key()+"/b7", "done half"))

	got := drain(t, client)
	if len(got) != 1 || got[0].d["fromChild"] != "audit-run" {
		t.Fatalf("frames = %v, want one external.prompt with fromChild audit-run", got)
	}
}

// TestDeliverAgentMessage_UnboundSessionGetsNoFrameAnywhere pins the
// bound-only rule: a session with no bound conversation of its own (a branch
// the app never opened) gets no chit in ANY conversation — even though the
// agent's default chat is pinned and live, so a fallback there would really
// deliver. The fixture guard proves the forbidden destination resolves;
// without it a default-chat fallback could silently deliver nowhere and the
// test would still pass.
func TestDeliverAgentMessage_UnboundSessionGetsNoFrameAnywhere(t *testing.T) {
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: newTestIndex(t)}
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })

	// The agent HAS a live bound conversation pinned as its default chat; the
	// branch session named in the delivery does not.
	b := h.ensureBinding(nil, "scout", "conv-default")
	if err := h.deps.SessionIndex.SetDefaultChat("scout", "app", b.chatID); err != nil {
		t.Fatalf("SetDefaultChat: %v", err)
	}
	if got := h.defaultChatBinding("scout"); got != b {
		t.Fatalf("fixture: scout's default chat resolves to %v, want the pinned binding — the no-fallback rule is unproven otherwise", got)
	}
	defClient := fakeClient()
	b.attach(defClient)

	DeliverAgentMessage("scout/c42/b1709596800", tools.AgentOrigin{SessionKey: "clutch/c111", AgentID: "clutch", Text: "what changed overnight?"})

	if got := drain(t, defClient); len(got) != 0 {
		t.Fatalf("default-chat conversation saw %v, want nothing (chits are bound-only, no fallback)", types(got))
	}
	// A bound delivery also buffers the frame for replay on sockets that are
	// not attached right now, so the pin must hold on every conversation's
	// replay buffer, not just the live client.
	h.mu.RLock()
	for convID, cb := range h.convs {
		cb.mu.Lock()
		n := len(cb.buffer)
		cb.mu.Unlock()
		if n != 0 {
			t.Errorf("conversation %s buffered %d frame(s), want none", convID, n)
		}
	}
	h.mu.RUnlock()
}

// TestDeliverAgentMessage_NoHubIsNoop proves delivery is a clean no-op when
// the app provider is not running at all.
func TestDeliverAgentMessage_NoHubIsNoop(t *testing.T) {
	setActiveHub(nil)
	DeliverAgentMessage("scout/c42", tools.AgentOrigin{SessionKey: "clutch/c111", AgentID: "clutch", Text: "must not panic"})
}

// TestDeliverExternalPrompt_FrameCarriesNoFromKeys is the #1286 req-1
// characterisation: a foci send prompt sets no sender, so its wire bytes are
// unchanged — the from keys stay absent from the encoded frame.
func TestDeliverExternalPrompt_FrameCarriesNoFromKeys(t *testing.T) {
	h := newTestHub()
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })

	b := h.ensureBinding(nil, "ag", "conv-1")
	client := fakeClient()
	b.attach(client)

	DeliverExternalPrompt(b.key(), "sent from the CLI")

	got := drain(t, client)
	if len(got) != 1 || got[0].t != fap.TypeExternalPrompt {
		t.Fatalf("frames = %v, want exactly one external.prompt", types(got))
	}
	if _, ok := got[0].d["fromAgent"]; ok {
		t.Error("fromAgent present on a foci send frame, want absent (wire bytes unchanged)")
	}
	if _, ok := got[0].d["fromSession"]; ok {
		t.Error("fromSession present on a foci send frame, want absent (wire bytes unchanged)")
	}
}
