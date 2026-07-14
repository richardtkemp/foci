package app

import (
	"testing"

	"foci/internal/app/fap"
	"foci/internal/platform"
)

func lastDraftSync(t *testing.T, c *wsClient) map[string]string {
	t.Helper()
	var out map[string]string
	for _, f := range drain(t, c) {
		if f.t != fap.TypeDraftSync {
			continue
		}
		out = map[string]string{}
		out["conversationId"], _ = f.d["conversationId"].(string)
		out["text"], _ = f.d["text"].(string)
	}
	return out
}

// TestHandleDraft_PersistsAndFansOutToOtherClients proves a draft persists under
// the chat's "draft" metadata and mirrors a DraftSync to the OTHER clients —
// never echoing to the device that put it.
func TestHandleDraft_PersistsAndFansOutToOtherClients(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42, sessionKey: "clutch/c42"}

	sender := fakeClient()
	other := fakeClient()
	h.clients[sender] = struct{}{}
	h.clients[other] = struct{}{}

	h.handleDraft(sender, fap.DraftPut{ConversationID: "c1", Text: "half a thought"})

	if v, _ := idx.GetChatMetadata("clutch", "app", 42, "draft"); v != "half a thought" {
		t.Errorf("draft = %q, want %q", v, "half a thought")
	}
	ds := lastDraftSync(t, other)
	if ds["conversationId"] != "c1" || ds["text"] != "half a thought" {
		t.Errorf("other client DraftSync = %v, want c1/'half a thought'", ds)
	}
	if len(drain(t, sender)) != 0 {
		t.Error("sender must not receive its own draft echo")
	}
}

// TestHandleDraft_EmptyTextClears proves an empty draft is a valid clear: it
// persists "" and still fans out a DraftSync (so the other devices empty their
// composers), unlike handleRead which drops an empty messageId.
func TestHandleDraft_EmptyTextClears(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42}
	_ = idx.SetChatMetadata("clutch", "app", 42, "draft", "stale")

	other := fakeClient()
	h.clients[other] = struct{}{}

	h.handleDraft(fakeClient(), fap.DraftPut{ConversationID: "c1", Text: ""})

	if v, _ := idx.GetChatMetadata("clutch", "app", 42, "draft"); v != "" {
		t.Errorf("draft = %q, want empty after clear", v)
	}
	if ds := lastDraftSync(t, other); ds == nil || ds["text"] != "" {
		t.Errorf("other client DraftSync = %v, want a clear (text='')", ds)
	}
}

// TestPushDrafts_ReplaysStoredDraft proves a just-connected client is replayed
// each conversation's stored draft, including an empty one — a cleared draft must
// reach a device that was offline during the clear (else its box stays wedged).
func TestPushDrafts_ReplaysStoredDraft(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42}
	h.convs["c2"] = &convBinding{convID: "c2", agentID: "clutch", chatID: 43}
	_ = idx.SetChatMetadata("clutch", "app", 42, "draft", "resume me")
	_ = idx.SetChatMetadata("clutch", "app", 43, "draft", "")

	c := fakeClient()
	h.pushDrafts(c)

	got := map[string]string{}
	for _, f := range drain(t, c) {
		if f.t == fap.TypeDraftSync {
			id, _ := f.d["conversationId"].(string)
			text, _ := f.d["text"].(string)
			got[id] = text
		}
	}
	if got["c1"] != "resume me" {
		t.Errorf("pushDrafts must replay c1's stored draft, got %q", got["c1"])
	}
	if _, ok := got["c2"]; !ok {
		t.Error("pushDrafts must replay an empty draft as a clear (c2)")
	}
}

// TestHandleDraft_IgnoresUnknownConversation proves a draft for a conversation
// with no live binding is a no-op (nothing persisted).
func TestHandleDraft_IgnoresUnknownConversation(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.handleDraft(fakeClient(), fap.DraftPut{ConversationID: "ghost", Text: "x"})
	if v, _ := idx.GetChatMetadata("clutch", "app", 42, "draft"); v != "" {
		t.Error("draft for unknown conv must not persist")
	}
}
