package app

import (
	"testing"

	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/session"
)

// scrollSyncs collects every ScrollSync frame queued for c, keyed by conversation.
func scrollSyncs(t *testing.T, c *wsClient) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, f := range drain(t, c) {
		if f.t != fap.TypeScrollSync {
			continue
		}
		id, _ := f.d["conversationId"].(string)
		out[id] = f.d
	}
	return out
}

func scrollHub(t *testing.T) (*Hub, *session.SessionIndex) {
	t.Helper()
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42, sessionKey: "clutch/c42"}
	return h, idx
}

// TestHandleScroll_PersistsAndFansOutToOtherClients: a put persists under the
// chat's "scroll" metadata and mirrors a ScrollSync to the OTHER clients only —
// the draft mirror's contract (#2144).
func TestHandleScroll_PersistsAndFansOutToOtherClients(t *testing.T) {
	h, idx := scrollHub(t)
	sender, other := fakeClient(), fakeClient()
	h.clients[sender] = struct{}{}
	h.clients[other] = struct{}{}

	h.handleScroll(sender, fap.ScrollPut{ConversationID: "c1", MessageID: "m7"})

	if v, _ := idx.GetChatMetadata("clutch", "app", 42, chatMetaScroll); v != `{"messageId":"m7","following":false}` {
		t.Errorf("stored scroll = %q", v)
	}
	got := scrollSyncs(t, other)["c1"]
	if got == nil || got["messageId"] != "m7" || got["following"] != false {
		t.Errorf("other client ScrollSync = %v, want c1/m7/following=false", got)
	}
	if len(drain(t, sender)) != 0 {
		t.Error("sender must not receive its own scroll echo")
	}
}

// Following needs no message: the id is dropped, not stored as noise, and the
// sync says only "following".
func TestHandleScroll_FollowingDropsMessageID(t *testing.T) {
	h, idx := scrollHub(t)
	other := fakeClient()
	h.clients[other] = struct{}{}

	h.handleScroll(fakeClient(), fap.ScrollPut{ConversationID: "c1", MessageID: "m7", Following: true})

	if v, _ := idx.GetChatMetadata("clutch", "app", 42, chatMetaScroll); v != `{"following":true}` {
		t.Errorf("stored scroll = %q, want following only", v)
	}
	got := scrollSyncs(t, other)["c1"]
	if got == nil || got["following"] != true {
		t.Fatalf("other client ScrollSync = %v, want following=true", got)
	}
	if _, has := got["messageId"]; has {
		t.Errorf("following sync carried a messageId: %v", got)
	}
}

// A not-following put with no message names nowhere: nothing is stored and
// nothing fans out, so it cannot wipe a good stored position.
func TestHandleScroll_RefusesPositionWithoutMessage(t *testing.T) {
	h, idx := scrollHub(t)
	_ = idx.SetChatMetadata("clutch", "app", 42, chatMetaScroll, `{"messageId":"m1","following":false}`)
	other := fakeClient()
	h.clients[other] = struct{}{}

	h.handleScroll(fakeClient(), fap.ScrollPut{ConversationID: "c1"})

	if v, _ := idx.GetChatMetadata("clutch", "app", 42, chatMetaScroll); v != `{"messageId":"m1","following":false}` {
		t.Errorf("stored scroll = %q, want the earlier position kept", v)
	}
	if n := len(scrollSyncs(t, other)); n != 0 {
		t.Errorf("fanned out %d ScrollSync for an empty position, want 0", n)
	}
}

func TestHandleScroll_IgnoresUnknownConversation(t *testing.T) {
	h, idx := scrollHub(t)
	other := fakeClient()
	h.clients[other] = struct{}{}
	h.handleScroll(fakeClient(), fap.ScrollPut{ConversationID: "ghost", MessageID: "m1"})
	if v, _ := idx.GetChatMetadata("clutch", "app", 42, chatMetaScroll); v != "" {
		t.Errorf("scroll for an unknown conv persisted %q", v)
	}
	if n := len(scrollSyncs(t, other)); n != 0 {
		t.Errorf("fanned out %d ScrollSync for an unknown conv, want 0", n)
	}
}

// TestPushScrolls_ReplaysStoredPositions: a just-connected client is replayed
// each conversation's stored position; a never-written or unreadable one is
// skipped rather than sent as a position (and must not panic the replay).
func TestPushScrolls_ReplaysStoredPositions(t *testing.T) {
	h, idx := scrollHub(t)
	h.convs["c2"] = &convBinding{convID: "c2", agentID: "clutch", chatID: 43}
	h.convs["c3"] = &convBinding{convID: "c3", agentID: "clutch", chatID: 44}
	h.convs["c4"] = &convBinding{convID: "c4", agentID: "clutch", chatID: 45}
	_ = idx.SetChatMetadata("clutch", "app", 42, chatMetaScroll, `{"messageId":"m5","following":false}`)
	_ = idx.SetChatMetadata("clutch", "app", 43, chatMetaScroll, `{"following":true}`)
	_ = idx.SetChatMetadata("clutch", "app", 44, chatMetaScroll, `not json`)
	// c4: never written.

	c := fakeClient()
	h.pushScrolls(c)
	got := scrollSyncs(t, c)

	if got["c1"] == nil || got["c1"]["messageId"] != "m5" || got["c1"]["following"] != false {
		t.Errorf("c1 replay = %v, want m5/following=false", got["c1"])
	}
	if got["c2"] == nil || got["c2"]["following"] != true {
		t.Errorf("c2 replay = %v, want following=true", got["c2"])
	}
	if _, ok := got["c3"]; ok {
		t.Error("an unreadable stored position must not be replayed")
	}
	if _, ok := got["c4"]; ok {
		t.Error("a never-written position must not be replayed")
	}
}
