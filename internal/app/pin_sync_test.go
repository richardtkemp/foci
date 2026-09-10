package app

import (
	"sync"
	"testing"

	"foci/internal/app/fap"
	"foci/internal/platform"
)

// pinSyncsFor collects every PinSync frame queued to c, keyed by conversationId
// (last one wins, matching what the client would end up applying).
func pinSyncsFor(t *testing.T, c *wsClient) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, f := range drain(t, c) {
		if f.t != fap.TypePinSync {
			continue
		}
		convID, _ := f.d["conversationId"].(string)
		raw, ok := f.d["messageIds"].([]any)
		if !ok {
			t.Fatalf("pin.sync for %q has no messageIds array (d=%v) — a null here "+
				"would fail the Kotlin decoder, whose field is a non-nullable List", convID, f.d)
		}
		ids := make([]string, 0, len(raw))
		for _, v := range raw {
			s, _ := v.(string)
			ids = append(ids, s)
		}
		out[convID] = ids
	}
	return out
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestHandlePin_PersistsAndFansOutToOtherClients proves a pin persists into the
// chat's "pins" metadata set and mirrors a PinSync carrying the WHOLE set to the
// OTHER clients — never echoing to the device that pinned it (which already
// flipped its own row optimistically).
func TestHandlePin_PersistsAndFansOutToOtherClients(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42, sessionKey: "clutch/c42"}

	sender := fakeClient()
	other := fakeClient()
	h.clients[sender] = struct{}{}
	h.clients[other] = struct{}{}

	h.handlePin(sender, fap.PinPut{ConversationID: "c1", MessageID: "m7", Pinned: true})

	if v, _ := idx.GetChatMetadata("clutch", "app", 42, chatMetaPins); v != `["m7"]` {
		t.Errorf("pins = %q, want [\"m7\"]", v)
	}
	if got := pinSyncsFor(t, other)["c1"]; !equalIDs(got, []string{"m7"}) {
		t.Errorf("other client PinSync = %v, want [m7]", got)
	}
	if len(drain(t, sender)) != 0 {
		t.Error("sender must not receive its own pin echo")
	}
}

// TestHandlePin_SecondPinAccumulates proves the frame carries the conversation's
// full set, not just the message that changed — the property the cold-start
// replay depends on (one frame type serves both paths).
func TestHandlePin_SecondPinAccumulates(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42}
	other := fakeClient()
	h.clients[other] = struct{}{}

	h.handlePin(fakeClient(), fap.PinPut{ConversationID: "c1", MessageID: "m2", Pinned: true})
	h.handlePin(fakeClient(), fap.PinPut{ConversationID: "c1", MessageID: "m1", Pinned: true})

	if got := pinSyncsFor(t, other)["c1"]; !equalIDs(got, []string{"m1", "m2"}) {
		t.Errorf("PinSync after two pins = %v, want the whole set [m1 m2]", got)
	}
}

// TestHandlePin_UnpinClearsAndStillFansOut proves an unpin down to nothing is a
// real event that must reach the other devices — an empty set is applied, not
// skipped. Without this, an unpin on the phone would leave the Mac pinned.
func TestHandlePin_UnpinClearsAndStillFansOut(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42}
	_ = idx.SetChatMetadata("clutch", "app", 42, chatMetaPins, `["m7"]`)

	other := fakeClient()
	h.clients[other] = struct{}{}

	h.handlePin(fakeClient(), fap.PinPut{ConversationID: "c1", MessageID: "m7", Pinned: false})

	if v, _ := idx.GetChatMetadata("clutch", "app", 42, chatMetaPins); v != `[]` {
		t.Errorf("pins = %q, want an empty set after the unpin", v)
	}
	got, ok := pinSyncsFor(t, other)["c1"]
	if !ok {
		t.Fatal("other client must receive a PinSync for an unpin-to-empty")
	}
	if len(got) != 0 {
		t.Errorf("PinSync = %v, want an empty set", got)
	}
}

// TestHandlePin_IgnoresUnknownConversation proves a pin for a conversation with
// no live binding is a no-op.
func TestHandlePin_IgnoresUnknownConversation(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	other := fakeClient()
	h.clients[other] = struct{}{}

	h.handlePin(fakeClient(), fap.PinPut{ConversationID: "ghost", MessageID: "m1", Pinned: true})

	if v, _ := idx.GetChatMetadata("clutch", "app", 42, chatMetaPins); v != "" {
		t.Error("pin for unknown conv must not persist")
	}
	if len(drain(t, other)) != 0 {
		t.Error("pin for unknown conv must not fan out")
	}
}

// TestHandlePin_EmptyMessageIDIsIgnored guards the degenerate frame.
func TestHandlePin_EmptyMessageIDIsIgnored(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42}
	_ = idx.SetChatMetadata("clutch", "app", 42, chatMetaPins, `["m7"]`)

	h.handlePin(fakeClient(), fap.PinPut{ConversationID: "c1", MessageID: "", Pinned: true})

	if v, _ := idx.GetChatMetadata("clutch", "app", 42, chatMetaPins); v != `["m7"]` {
		t.Errorf("pins = %q, want the stored set untouched", v)
	}
}

// TestHandlePin_ConcurrentTogglesKeepBoth proves the read-modify-write is
// serialised. Two devices pinning DIFFERENT messages in the same chat at the
// same instant must end with both pinned; an unguarded RMW loses one because
// both compute their new set from the same stale base. Run under -race.
func TestHandlePin_ConcurrentTogglesKeepBoth(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42}

	var wg sync.WaitGroup
	for _, id := range []string{"m1", "m2", "m3", "m4"} {
		wg.Add(1)
		go func(mid string) {
			defer wg.Done()
			h.handlePin(fakeClient(), fap.PinPut{ConversationID: "c1", MessageID: mid, Pinned: true})
		}(id)
	}
	wg.Wait()

	v, _ := idx.GetChatMetadata("clutch", "app", 42, chatMetaPins)
	if v != `["m1","m2","m3","m4"]` {
		t.Errorf("pins = %q, want all four survivors — a lost update means the RMW raced", v)
	}
}

// TestPushPins_ReplaysStoredSet proves a just-connected client is replayed each
// conversation's stored pinned set. This is the cold-start half of #1882: the
// reported failure was a Mac that was not connected when the pin happened, so
// live fan-out alone would not have fixed it.
//
// The three conversations are the three distinguishable states, and each is a
// separate rule:
//   - c1 has pins → replayed (the reported bug).
//   - c2 stores "[]" — pinned then fully unpinned → replayed as a clear, so an
//     unpin reaches a device that was offline for it.
//   - c3 has NO stored key — never pinned on any device since this feature
//     shipped → NOT replayed. Message pins predate this sync (#893), so a device
//     may hold local-only pins the server never heard of; telling it "nothing is
//     pinned" would wipe them on the first post-upgrade hello.
func TestPushPins_ReplaysStoredSet(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42}
	h.convs["c2"] = &convBinding{convID: "c2", agentID: "clutch", chatID: 43}
	h.convs["c3"] = &convBinding{convID: "c3", agentID: "clutch", chatID: 44}
	_ = idx.SetChatMetadata("clutch", "app", 42, chatMetaPins, `["m1","m9"]`)
	_ = idx.SetChatMetadata("clutch", "app", 43, chatMetaPins, encodePinSet(nil))

	c := fakeClient()
	h.pushPins(c)

	got := pinSyncsFor(t, c)
	if !equalIDs(got["c1"], []string{"m1", "m9"}) {
		t.Errorf("pushPins must replay c1's stored set, got %v", got["c1"])
	}
	ids, ok := got["c2"]
	if !ok {
		t.Error("pushPins must replay a stored-empty set as a clear (c2) — else an unpin never reaches an offline device")
	} else if len(ids) != 0 {
		t.Errorf("c2 replay = %v, want empty", ids)
	}
	if ids, ok := got["c3"]; ok {
		t.Errorf("pushPins replayed %v for c3, which has no stored pin set at all; "+
			"that clear would wipe a device's pre-existing local-only pins on upgrade", ids)
	}
}

// TestHello_SeedsPinsToReconnectingDevice is the DONE-WHEN for #1882, and the
// one test here that could not pass vacuously by calling the new helper
// directly: it drives a real `hello` envelope through dispatchInbound and
// asserts a PinSync comes back. Calling h.pushPins(c) proves the helper works;
// only this proves it is WIRED INTO THE HANDSHAKE, which is the step the ticket
// flagged as most likely to be forgotten. Deleting the h.pushPins(client) line
// from the hello branch reddens exactly this test and nothing else.
//
// Scenario: the pin happened on the phone while this device was offline, so it
// never saw the live fan-out. Its only chance is the replay.
func TestHello_SeedsPinsToReconnectingDevice(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42}
	_ = idx.SetChatMetadata("clutch", "app", 42, chatMetaPins, `["m7"]`)

	mac := fakeClient()
	h.clients[mac] = struct{}{}

	h.dispatchInbound(mac, []byte(`{"t":"hello","id":"i1","d":{"client":{"deviceId":"mac","app":"foci","os":"macos","version":"1"}}}`))

	if got := pinSyncsFor(t, mac)["c1"]; !equalIDs(got, []string{"m7"}) {
		t.Errorf("hello replay PinSync for c1 = %v, want [m7] — the cold-start seed is not wired into the handshake", got)
	}
}
