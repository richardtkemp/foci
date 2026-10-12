package app

import (
	"testing"

	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/session"
)

// convPinSyncs collects every ConvPinSync frame queued for c, keyed by
// conversation (last one wins, matching what the client would end up applying).
func convPinSyncs(t *testing.T, c *wsClient) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, f := range drain(t, c) {
		if f.t != fap.TypeConvPinSync {
			continue
		}
		id, _ := f.d["conversationId"].(string)
		pinned, _ := f.d["pinned"].(bool)
		out[id] = pinned
	}
	return out
}

func convPinHub(t *testing.T) (*Hub, *session.SessionIndex) {
	t.Helper()
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42, sessionKey: "clutch/c42"}
	return h, idx
}

// TestHandleConvPin_PinPersistsAndFansOut proves a pin persists under the
// chat's "conv_pinned" metadata as "1" and mirrors a ConvPinSync to the OTHER
// clients only — never echoing to the device that pinned it (which already
// flipped its own row optimistically). Dropping the SetChatMetadata reddens
// the stored-value assertion; dropping the broadcastExcept reddens the other-
// client one (#2313).
func TestHandleConvPin_PinPersistsAndFansOut(t *testing.T) {
	h, idx := convPinHub(t)
	sender, other := fakeClient(), fakeClient()
	h.clients[sender] = struct{}{}
	h.clients[other] = struct{}{}

	h.handleConvPin(sender, fap.ConvPinPut{ConversationID: "c1", Pinned: true})

	if v, _ := idx.GetChatMetadata("clutch", "app", 42, chatMetaConvPinned); v != "1" {
		t.Errorf("stored conv_pinned = %q, want \"1\"", v)
	}
	if got, ok := convPinSyncs(t, other)["c1"]; !ok || !got {
		t.Errorf("other client ConvPinSync = %v, want c1/pinned=true", got)
	}
	if n := len(convPinSyncs(t, sender)); n != 0 {
		t.Error("sender must not receive its own conv-pin echo")
	}
}

// TestHandleConvPin_UnpinPersistsAndFansOut proves an unpin is a real event:
// it stores "0" (not a delete) and still fans out, so a device that was
// offline for it learns the conversation is no longer pinned.
func TestHandleConvPin_UnpinPersistsAndFansOut(t *testing.T) {
	h, idx := convPinHub(t)
	other := fakeClient()
	h.clients[other] = struct{}{}

	h.handleConvPin(fakeClient(), fap.ConvPinPut{ConversationID: "c1", Pinned: false})

	if v, _ := idx.GetChatMetadata("clutch", "app", 42, chatMetaConvPinned); v != "0" {
		t.Errorf("stored conv_pinned = %q, want \"0\"", v)
	}
	if got, ok := convPinSyncs(t, other)["c1"]; !ok || got {
		t.Errorf("other client ConvPinSync = %v, want c1/pinned=false", got)
	}
}

// TestDispatchConvPinPut_PersistsAndFansOut drives a real `convpin.put`
// envelope through dispatchInbound, proving the codec registration and the
// dispatch case are wired — not just the handler. At the "frames exist,
// dispatch case missing" stage this hits the default unknown-type arm and
// reddens here.
func TestDispatchConvPinPut_PersistsAndFansOut(t *testing.T) {
	h, idx := convPinHub(t)
	sender, other := fakeClient(), fakeClient()
	h.clients[sender] = struct{}{}
	h.clients[other] = struct{}{}

	h.dispatchInbound(sender, []byte(`{"t":"convpin.put","id":"i1","d":{"conversationId":"c1","pinned":true}}`))

	if v, _ := idx.GetChatMetadata("clutch", "app", 42, chatMetaConvPinned); v != "1" {
		t.Errorf("stored conv_pinned = %q, want \"1\"", v)
	}
	if got, ok := convPinSyncs(t, other)["c1"]; !ok || !got {
		t.Errorf("other client ConvPinSync = %v, want c1/pinned=true", got)
	}
	if n := len(convPinSyncs(t, sender)); n != 0 {
		t.Error("sender must not receive its own conv-pin echo")
	}
}

// TestHandleConvPin_IgnoresUnknownConversation proves a put for a conversation
// with no live binding is a no-op, exactly like the scroll handler.
func TestHandleConvPin_IgnoresUnknownConversation(t *testing.T) {
	h, idx := convPinHub(t)
	other := fakeClient()
	h.clients[other] = struct{}{}

	h.handleConvPin(fakeClient(), fap.ConvPinPut{ConversationID: "ghost", Pinned: true})

	if v, _ := idx.GetChatMetadata("clutch", "app", 42, chatMetaConvPinned); v != "" {
		t.Errorf("conv pin for unknown conv persisted %q", v)
	}
	if n := len(convPinSyncs(t, other)); n != 0 {
		t.Errorf("fanned out %d ConvPinSync for an unknown conv, want 0", n)
	}
}

// TestPushConvPins_ReplaysStoredPins proves a just-connected client is
// replayed each conversation's stored pin, in every distinguishable state:
//   - c1 stores "1" → replayed as pinned.
//   - c2 stores "0" — pinned then unpinned → replayed as NOT pinned, so an
//     unpin reaches a device that was offline for it.
//   - c3 has NO stored key — never pinned on any device since this mirror
//     shipped → NOT replayed. Conversation pins were per-device until #2313,
//     so a device may hold local-only pins the server never heard of; telling
//     it "not pinned" would wipe them on the first post-upgrade hello.
//   - c4 stores garbage no writer of ours can produce → skipped, not sent as
//     a state (and must not panic the replay).
func TestPushConvPins_ReplaysStoredPins(t *testing.T) {
	h, idx := convPinHub(t)
	h.convs["c2"] = &convBinding{convID: "c2", agentID: "clutch", chatID: 43}
	h.convs["c3"] = &convBinding{convID: "c3", agentID: "clutch", chatID: 44}
	h.convs["c4"] = &convBinding{convID: "c4", agentID: "clutch", chatID: 45}
	_ = idx.SetChatMetadata("clutch", "app", 42, chatMetaConvPinned, "1")
	_ = idx.SetChatMetadata("clutch", "app", 43, chatMetaConvPinned, "0")
	_ = idx.SetChatMetadata("clutch", "app", 45, chatMetaConvPinned, "yes")
	// c3: never written.

	c := fakeClient()
	h.pushConvPins(c)
	got := convPinSyncs(t, c)

	if pinned, ok := got["c1"]; !ok || !pinned {
		t.Errorf("c1 replay = %v/%v, want pinned=true", pinned, ok)
	}
	if pinned, ok := got["c2"]; !ok || pinned {
		t.Errorf("c2 replay = %v/%v, want pinned=false — an unpin must reach an offline device", pinned, ok)
	}
	if _, ok := got["c3"]; ok {
		t.Error("a never-written conv pin must not be replayed; that clear would wipe a device's pre-existing local-only pins on upgrade")
	}
	if _, ok := got["c4"]; ok {
		t.Error("an unrecognised stored value must not be replayed as a pin state")
	}
}

// TestHello_SeedsConvPinsToReconnectingDevice is the wiring test for the
// replay: it drives a real `hello` envelope through dispatchInbound and asserts
// a ConvPinSync comes back. Calling h.pushConvPins(c) proves the helper works;
// only this proves it is WIRED INTO THE HANDSHAKE. Deleting the
// h.pushConvPins(client) line from the hello branch reddens exactly this test.
//
// Scenario: the pin happened on the phone while this device was offline, so it
// never saw the live fan-out. Its only chance is the replay.
func TestHello_SeedsConvPinsToReconnectingDevice(t *testing.T) {
	h, idx := convPinHub(t)
	_ = idx.SetChatMetadata("clutch", "app", 42, chatMetaConvPinned, "1")

	mac := fakeClient()
	h.clients[mac] = struct{}{}

	h.dispatchInbound(mac, []byte(`{"t":"hello","id":"i1","d":{"client":{"deviceId":"mac","app":"foci","os":"macos","version":"1"}}}`))

	if pinned, ok := convPinSyncs(t, mac)["c1"]; !ok || !pinned {
		t.Errorf("hello replay ConvPinSync for c1 = %v/%v, want pinned=true — the replay is not wired into the handshake", pinned, ok)
	}
}

// TestConversationPinned_ThreeStates pins the accessor's contract for its
// consumers (#1243's sweep now, #1159's later): (true, true) for pinned,
// (false, true) for explicitly unpinned, and (false, false) for never-written
// (or unanswerable — no session index), which a caller must not conflate with
// "unpinned". Since the promotion to production the third state is split: a
// row that cannot be READ returns err instead, which a caller must fail safe
// on (the sweep keeps the chat).
func TestConversationPinned_ThreeStates(t *testing.T) {
	h, idx := convPinHub(t)
	_ = idx.SetChatMetadata("clutch", "app", 42, chatMetaConvPinned, "1")
	h.convs["c2"] = &convBinding{convID: "c2", agentID: "clutch", chatID: 43}
	_ = idx.SetChatMetadata("clutch", "app", 43, chatMetaConvPinned, "0")
	h.convs["c3"] = &convBinding{convID: "c3", agentID: "clutch", chatID: 44}
	// c3: never written.

	if pinned, known, err := h.conversationPinned(h.convs["c1"]); err != nil || !pinned || !known {
		t.Errorf("conversationPinned(c1) = %v/%v/%v, want true/true", pinned, known, err)
	}
	if pinned, known, err := h.conversationPinned(h.convs["c2"]); err != nil || pinned || !known {
		t.Errorf("conversationPinned(c2) = %v/%v/%v, want false/true", pinned, known, err)
	}
	if pinned, known, err := h.conversationPinned(h.convs["c3"]); err != nil || pinned || known {
		t.Errorf("conversationPinned(c3) = %v/%v/%v, want false/false", pinned, known, err)
	}

	bare := newTestHub() // no SessionIndex at all
	if pinned, known, err := bare.conversationPinned(&convBinding{convID: "c1", agentID: "clutch", chatID: 42}); err != nil || pinned || known {
		t.Errorf("conversationPinned with no session index = %v/%v/%v, want false/false", pinned, known, err)
	}
}

// TestInboundConvID_ConvPinPutIsFireAndForget proves ConvPinPut is classified
// exactly like its ScrollPut/DraftPut siblings: no conversation id from
// inboundConvID, so the reliability gate never dedups or acks it. Adding
// ConvPinPut to that switch (e.g. to make the client's outbox retry it) would
// redden the first assertion while the controls stay green.
func TestInboundConvID_ConvPinPutIsFireAndForget(t *testing.T) {
	if got := inboundConvID(fap.ConvPinPut{ConversationID: "c1", Pinned: true}); got != "" {
		t.Errorf("inboundConvID(ConvPinPut) = %q, want \"\" (fire-and-forget, like ScrollPut)", got)
	}
	if got := inboundConvID(fap.ScrollPut{ConversationID: "c1"}); got != "" {
		t.Errorf("control: inboundConvID(ScrollPut) = %q, want \"\"", got)
	}
	if got := inboundConvID(fap.ClientMessage{ConversationID: "c1"}); got != "c1" {
		t.Errorf("control: inboundConvID(ClientMessage) = %q, want c1", got)
	}
}
