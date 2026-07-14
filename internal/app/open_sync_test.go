package app

import (
	"testing"

	"foci/internal/app/fap"
	"foci/internal/platform"
)

func lastOpenSync(t *testing.T, c *wsClient) ([]string, bool) {
	t.Helper()
	var out []string
	seen := false
	for _, f := range drain(t, c) {
		if f.t != fap.TypeConversationOpenSync {
			continue
		}
		seen = true
		out = nil
		if ids, ok := f.d["conversationIds"].([]any); ok {
			for _, id := range ids {
				if s, ok := id.(string); ok {
					out = append(out, s)
				}
			}
		}
	}
	return out, seen
}

// TestHandleConversationOpenSet_PersistsAndFansOut proves an open-set persists to
// the shared "open_chats" system-state and mirrors a ConversationOpenSync to the
// OTHER clients — never echoing to the device that sent it.
func TestHandleConversationOpenSet_PersistsAndFansOut(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}

	sender := fakeClient()
	other := fakeClient()
	h.clients[sender] = struct{}{}
	h.clients[other] = struct{}{}

	h.handleConversationOpenSet(sender, fap.ConversationOpenSet{ConversationIDs: []string{"c1", "c2"}})

	if got := h.loadOpenChats(); len(got) != 2 || got[0] != "c1" || got[1] != "c2" {
		t.Errorf("stored open-set = %v, want [c1 c2]", got)
	}
	if ids, seen := lastOpenSync(t, other); !seen || len(ids) != 2 || ids[0] != "c1" || ids[1] != "c2" {
		t.Errorf("other client OpenSync = %v (seen=%v), want [c1 c2]", ids, seen)
	}
	if len(drain(t, sender)) != 0 {
		t.Error("sender must not receive its own open-set echo")
	}
}

// TestHandleConversationOpenSet_EmptyClears proves an empty set is a valid clear:
// it persists [] and still fans out (so other devices close all mirrored tabs).
func TestHandleConversationOpenSet_EmptyClears(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.storeOpenChats([]string{"stale"})

	other := fakeClient()
	h.clients[other] = struct{}{}

	h.handleConversationOpenSet(fakeClient(), fap.ConversationOpenSet{ConversationIDs: nil})

	if got := h.loadOpenChats(); len(got) != 0 {
		t.Errorf("stored open-set = %v, want empty after clear", got)
	}
	if ids, seen := lastOpenSync(t, other); !seen || len(ids) != 0 {
		t.Errorf("other client OpenSync = %v (seen=%v), want an empty clear", ids, seen)
	}
}

// TestOpenSessionsForAgent_FromPersistedOpenSet proves keepalive's candidate
// source reads the PERSISTED open-set (not live sockets): with NO client
// connected it still resolves the open convs to their session keys, filters to
// the requesting agent, and skips conv IDs with no binding.
func TestOpenSessionsForAgent_FromPersistedOpenSet(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	registerBareAgent(h, "ag")
	registerBareAgent(h, "other")

	// Durable bindings with a nil socket — the app-disconnected state.
	bAg := h.ensureBinding(nil, "ag", "cA")
	h.ensureBinding(nil, "other", "cB")

	// Persisted open-set: agent's chat, another agent's chat, and an unbound ghost.
	h.storeOpenChats([]string{"cA", "cB", "cGhost"})

	got := h.OpenSessionsForAgent("ag")
	if len(got) != 1 || got[0] != bAg.sessionKey {
		t.Fatalf("OpenSessionsForAgent(ag) = %v, want [%s]", got, bAg.sessionKey)
	}
}

// TestPushOpenSet_ReplaysStored proves a just-connected client is replayed the
// stored shared open-set, and that nothing is sent when none is stored.
func TestPushOpenSet_ReplaysStored(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}

	empty := fakeClient()
	h.pushOpenSet(empty)
	if _, seen := lastOpenSync(t, empty); seen {
		t.Error("pushOpenSet must send nothing when no open-set is stored")
	}

	h.storeOpenChats([]string{"c1", "c2"})
	c := fakeClient()
	h.pushOpenSet(c)
	if ids, seen := lastOpenSync(t, c); !seen || len(ids) != 2 {
		t.Errorf("pushOpenSet replay = %v (seen=%v), want [c1 c2]", ids, seen)
	}
}
