package app

import (
	"encoding/json"
	"testing"

	"foci/internal/fap"
	"foci/internal/platform"
)

// Tests for #2670: a pin can carry a label, and one view lists every pin
// across chats (the pins.list / pins.all pair).

// pinLabelsFor collects the labels map of the last PinSync queued to c for
// each conversation. A PinSync with no labels field maps to an empty map.
func pinLabelsFor(t *testing.T, frames []decoded) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	for _, f := range frames {
		if f.t != fap.TypePinSync {
			continue
		}
		convID, _ := f.d["conversationId"].(string)
		labels := map[string]string{}
		if raw, ok := f.d["labels"].(map[string]any); ok {
			for k, v := range raw {
				s, _ := v.(string)
				labels[k] = s
			}
		}
		out[convID] = labels
	}
	return out
}

// seedMessageFrame stores one visible message frame in the frame store, the
// way convBinding.send does (wire + roster preview + send time).
func seedMessageFrame(t *testing.T, s *frameStore, convID string, seq int64, messageID, text string, sentMs int64) {
	t.Helper()
	w, err := fap.Encode(fap.ServerMessage{ConversationID: convID, MessageID: messageID, Role: "agent", Text: text}, seq, 0, "", "")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s.insert(frameWrite{convID: convID, seq: seq, wire: w, sentMs: sentMs, visible: true, preview: truncatePreview(text)})
}

// pinsAllFrom decodes the single pins.all frame queued to c.
func pinsAllFrom(t *testing.T, c *wsClient) fap.PinsAll {
	t.Helper()
	var got []fap.PinsAll
	for _, f := range drain(t, c) {
		if f.t != fap.TypePinsAll {
			continue
		}
		b, _ := json.Marshal(f.d)
		var pa fap.PinsAll
		if err := json.Unmarshal(b, &pa); err != nil {
			t.Fatalf("pins.all payload: %v", err)
		}
		got = append(got, pa)
	}
	if len(got) != 1 {
		t.Fatalf("got %d pins.all frames, want exactly 1", len(got))
	}
	return got[0]
}

// H6: device A pins M with label L. Connected device B receives the pin with
// the label; device D, connecting afterwards, receives it on its hello replay.
func TestPinLabel_ReachesOtherDeviceAndReconnectingDevice(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42}

	a, b := fakeClient(), fakeClient()
	h.clients[a] = struct{}{}
	h.clients[b] = struct{}{}

	h.handlePin(a, fap.PinPut{ConversationID: "c1", MessageID: "m7", Pinned: true, Label: "  flight times  "})

	bFrames := drain(t, b)
	if got := pinLabelsFor(t, bFrames)["c1"]["m7"]; got != "flight times" {
		t.Errorf("connected device B label = %q, want %q (stored trimmed)", got, "flight times")
	}

	d := fakeClient()
	h.clients[d] = struct{}{}
	h.dispatchInbound(d, []byte(`{"t":"hello","id":"i1","d":{"client":{"deviceId":"d","app":"foci","os":"macos","version":"1"}}}`))
	dFrames := drain(t, d)
	if got := pinLabelsFor(t, dFrames)["c1"]["m7"]; got != "flight times" {
		t.Errorf("reconnecting device D label = %q, want %q", got, "flight times")
	}
}

// H7: pin with L1, unpin, pin with L2 — every device ends with L2; and the
// unpin reaches an offline device as an empty set (no stale label survives).
func TestPinLabel_RepinTakesNewLabelAndUnpinClears(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42}
	other := fakeClient()
	h.clients[other] = struct{}{}

	h.handlePin(fakeClient(), fap.PinPut{ConversationID: "c1", MessageID: "m1", Pinned: true, Label: "L1"})
	h.handlePin(fakeClient(), fap.PinPut{ConversationID: "c1", MessageID: "m1", Pinned: false})

	offline := fakeClient()
	h.pushPins(offline)
	off := drain(t, offline)
	if ids := pinSyncsFromFrames(t, off)["c1"]; len(ids) != 0 {
		t.Errorf("offline device after unpin got set %v, want empty", ids)
	}
	if l := pinLabelsFor(t, off)["c1"]; len(l) != 0 {
		t.Errorf("offline device after unpin got labels %v, want none", l)
	}

	h.handlePin(fakeClient(), fap.PinPut{ConversationID: "c1", MessageID: "m1", Pinned: true, Label: "L2"})
	if got := pinLabelsFor(t, drain(t, other))["c1"]["m1"]; got != "L2" {
		t.Errorf("live device label after re-pin = %q, want L2", got)
	}
	late := fakeClient()
	h.pushPins(late)
	if got := pinLabelsFor(t, drain(t, late))["c1"]["m1"]; got != "L2" {
		t.Errorf("reconnecting device label after re-pin = %q, want L2", got)
	}
}

// Requirement 3: a pin's label is set once. A PinPut for a message that is
// already pinned (a re-delivery, or a stale second device) does not change it.
func TestPinLabel_PinOfPinnedMessageKeepsLabel(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42}
	other := fakeClient()
	h.clients[other] = struct{}{}

	h.handlePin(fakeClient(), fap.PinPut{ConversationID: "c1", MessageID: "m1", Pinned: true, Label: "first"})
	h.handlePin(fakeClient(), fap.PinPut{ConversationID: "c1", MessageID: "m1", Pinned: true, Label: "second"})
	h.handlePin(fakeClient(), fap.PinPut{ConversationID: "c1", MessageID: "m1", Pinned: true})

	if got := pinLabelsFor(t, drain(t, other))["c1"]["m1"]; got != "first" {
		t.Errorf("label = %q, want it unchanged as %q", got, "first")
	}
}

// H8 + H13 (server half): a pin set stored before labels existed — a plain id
// array under "pins", no pin_meta — still replays as pinned, with no labels
// field at all; and an old app's PinPut (no label field) pins unlabelled.
func TestPinLabel_LegacyRowsAndOldAppFrames(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42}
	h.convs["c2"] = &convBinding{convID: "c2", agentID: "clutch", chatID: 43}
	_ = idx.SetChatMetadata("clutch", "app", 42, chatMetaPins, `["m1","m2"]`)

	c := fakeClient()
	h.pushPins(c)
	frames := drain(t, c)
	if ids := pinSyncsFromFrames(t, frames)["c1"]; !equalIDs(ids, []string{"m1", "m2"}) {
		t.Errorf("legacy set replay = %v, want [m1 m2]", ids)
	}
	for _, f := range frames {
		if _, has := f.d["labels"]; has && f.t == fap.TypePinSync {
			t.Errorf("legacy unlabelled set must omit labels, got %v", f.d["labels"])
		}
	}

	other := fakeClient()
	h.clients[other] = struct{}{}
	h.dispatchInbound(fakeClient(), []byte(`{"t":"pin.put","id":"p1","d":{"conversationId":"c2","messageId":"m9","pinned":true}}`))
	of := drain(t, other)
	if ids := pinSyncsFromFrames(t, of)["c2"]; !equalIDs(ids, []string{"m9"}) {
		t.Errorf("old-app pin.put fan-out = %v, want [m9]", ids)
	}
	if l := pinLabelsFor(t, of)["c2"]; len(l) != 0 {
		t.Errorf("old-app pin must be unlabelled, got %v", l)
	}
}

// H9: two agents, three chats, pins in each. The asking device has never
// received a single frame of c3 (it holds no binding attachment, no client
// state for it). pins.all, requested over the real dispatch path, still lists
// every pin, grouped by chat, with label, snippet and the message time.
func TestPinsList_ListsEveryPinAcrossAgentsAndChats(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.frames = tempFrameStore(t)
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42}
	h.convs["c2"] = &convBinding{convID: "c2", agentID: "clutch", chatID: 43}
	h.convs["c3"] = &convBinding{convID: "c3", agentID: "scout", chatID: 44}
	_ = idx.SetChatMetadata("clutch", "app", 42, "alias", "Flights")
	_ = idx.SetChatMetadata("scout", "app", 44, "alias", "Research")

	seedMessageFrame(t, h.frames, "c1", 1, "m1", "BA123 departs 09:40", 1000)
	seedMessageFrame(t, h.frames, "c1", 2, "m2", "return flight 18:05", 2000)
	seedMessageFrame(t, h.frames, "c2", 1, "m3", "the fix was the lock", 1500)
	seedMessageFrame(t, h.frames, "c3", 1, "m4", "paper on stemming", 3000)

	pinner := fakeClient()
	h.handlePin(pinner, fap.PinPut{ConversationID: "c1", MessageID: "m1", Pinned: true, Label: "outbound"})
	h.handlePin(pinner, fap.PinPut{ConversationID: "c1", MessageID: "m2", Pinned: true})
	h.handlePin(pinner, fap.PinPut{ConversationID: "c2", MessageID: "m3", Pinned: true, Label: "the fix that worked"})
	h.handlePin(pinner, fap.PinPut{ConversationID: "c3", MessageID: "m4", Pinned: true, Label: "read later"})

	asker := fakeClient()
	h.dispatchInbound(asker, []byte(`{"t":"pins.list","id":"q1"}`))
	got := pinsAllFrom(t, asker)

	if len(got.Conversations) != 3 {
		t.Fatalf("groups = %d, want 3 (one per chat): %+v", len(got.Conversations), got)
	}
	// Ordered by each group's newest pinned message: c3 (3000), c1 (2000), c2 (1500).
	order := []string{got.Conversations[0].ConversationID, got.Conversations[1].ConversationID, got.Conversations[2].ConversationID}
	if !equalIDs(order, []string{"c3", "c1", "c2"}) {
		t.Errorf("group order = %v, want [c3 c1 c2]", order)
	}
	byConv := map[string]fap.PinGroup{}
	for _, g := range got.Conversations {
		byConv[g.ConversationID] = g
	}
	c3 := byConv["c3"]
	if c3.AgentID != "scout" || c3.Title != "Research" {
		t.Errorf("c3 group = agent %q title %q, want scout / Research", c3.AgentID, c3.Title)
	}
	if len(c3.Pins) != 1 || c3.Pins[0] != (fap.PinnedMessage{MessageID: "m4", Label: "read later", Snippet: "paper on stemming", SentMs: 3000}) {
		t.Errorf("c3 pins = %+v", c3.Pins)
	}
	c1 := byConv["c1"]
	if c1.Title != "Flights" || len(c1.Pins) != 2 {
		t.Fatalf("c1 group = %+v", c1)
	}
	if c1.Pins[0] != (fap.PinnedMessage{MessageID: "m2", Snippet: "return flight 18:05", SentMs: 2000}) {
		t.Errorf("c1 newest pin = %+v, want m2 unlabelled", c1.Pins[0])
	}
	if c1.Pins[1] != (fap.PinnedMessage{MessageID: "m1", Label: "outbound", Snippet: "BA123 departs 09:40", SentMs: 1000}) {
		t.Errorf("c1 older pin = %+v", c1.Pins[1])
	}
	if p := byConv["c2"].Pins; len(p) != 1 || p[0].Label != "the fix that worked" || p[0].Snippet != "the fix was the lock" || p[0].SentMs != 1500 {
		t.Errorf("c2 pins = %+v", p)
	}
}

// H10 + the TTL case: a pin whose message the frame store no longer holds is
// listed from the snapshot taken at pin time; a pin found nowhere (a legacy
// pin of a trimmed message) is skipped, never sent as a blank row. With
// nothing listable at all the answer is an empty list, not null.
func TestPinsList_SnapshotFallbackAndMissingMessages(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.frames = tempFrameStore(t)
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42}
	h.convs["c2"] = &convBinding{convID: "c2", agentID: "clutch", chatID: 43}

	seedMessageFrame(t, h.frames, "c1", 1, "m1", "kept by snapshot", 1000)
	h.handlePin(fakeClient(), fap.PinPut{ConversationID: "c1", MessageID: "m1", Pinned: true, Label: "snap"})
	// The frame store's TTL trims the message.
	if _, err := h.frames.db.Exec(`DELETE FROM app_frames`); err != nil {
		t.Fatal(err)
	}
	// A legacy pin: in the set, no meta, no frames anywhere.
	_ = idx.SetChatMetadata("clutch", "app", 42, chatMetaPins, `["ghost","m1"]`)
	_ = idx.SetChatMetadata("clutch", "app", 43, chatMetaPins, `["ghost2"]`)

	c := fakeClient()
	h.handlePinsList(c)
	got := pinsAllFrom(t, c)
	if len(got.Conversations) != 1 {
		t.Fatalf("groups = %+v, want only c1 (c2's only pin is missing)", got.Conversations)
	}
	p := got.Conversations[0].Pins
	if len(p) != 1 || p[0] != (fap.PinnedMessage{MessageID: "m1", Label: "snap", Snippet: "kept by snapshot", SentMs: 1000}) {
		t.Errorf("c1 pins = %+v, want only m1 from its snapshot (ghost skipped)", p)
	}

	empty := newTestHub()
	empty.deps = platform.ProviderDeps{SessionIndex: newTestIndex(t)}
	ec := fakeClient()
	empty.handlePinsList(ec)
	for _, f := range drain(t, ec) {
		if f.t == fap.TypePinsAll {
			if arr, ok := f.d["conversations"].([]any); !ok || len(arr) != 0 {
				t.Errorf("empty answer conversations = %v, want []", f.d["conversations"])
			}
		}
	}
}

// pinSyncsFromFrames is pinSyncsFor over already-drained frames.
func pinSyncsFromFrames(t *testing.T, frames []decoded) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, f := range frames {
		if f.t != fap.TypePinSync {
			continue
		}
		convID, _ := f.d["conversationId"].(string)
		raw, _ := f.d["messageIds"].([]any)
		ids := make([]string, 0, len(raw))
		for _, v := range raw {
			s, _ := v.(string)
			ids = append(ids, s)
		}
		out[convID] = ids
	}
	return out
}
