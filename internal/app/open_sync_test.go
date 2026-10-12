package app

import (
	"strings"
	"testing"

	"foci/internal/fap"
	"foci/internal/platform"
)

// lastOpenSync drains c and reports the LAST ConversationOpenSync it carried
// plus how many were seen. The count is load-bearing: one open-set change is
// ONE mirror per device — every writer pairs its store and fan-out inside
// openSetMu — so a count above 1 means a second send escaped that pairing and
// could follow a newer writer's set (reopen swept tabs / roll back a fresh
// one).
func lastOpenSync(t *testing.T, c *wsClient) ([]string, int) {
	t.Helper()
	var out []string
	n := 0
	for _, f := range drain(t, c) {
		if f.t != fap.TypeConversationOpenSync {
			continue
		}
		n++
		out = nil
		if ids, ok := f.d["conversationIds"].([]any); ok {
			for _, id := range ids {
				if s, ok := id.(string); ok {
					out = append(out, s)
				}
			}
		}
	}
	return out, n
}

// TestHandleConversationOpenSet_PersistsAndFansOut proves an open-set persists to
// the shared "open_chats" system-state and mirrors a ConversationOpenSync to the
// OTHER clients — never echoing to the device that sent it — as exactly ONE
// frame: the store and the fan-out are one paired critical section (openSetMu),
// so a second, later copy of the mirror could follow a newer writer's set and
// roll back what it did.
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
	if ids, n := lastOpenSync(t, other); n != 1 || len(ids) != 2 || ids[0] != "c1" || ids[1] != "c2" {
		t.Errorf("other client OpenSync = %v (frames=%d), want exactly one [c1 c2]", ids, n)
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
	if ids, n := lastOpenSync(t, other); n != 1 || len(ids) != 0 {
		t.Errorf("other client OpenSync = %v (frames=%d), want exactly one empty clear", ids, n)
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
	if _, n := lastOpenSync(t, empty); n != 0 {
		t.Error("pushOpenSet must send nothing when no open-set is stored")
	}

	h.storeOpenChats([]string{"c1", "c2"})
	c := fakeClient()
	h.pushOpenSet(c)
	if ids, n := lastOpenSync(t, c); n != 1 || len(ids) != 2 {
		t.Errorf("pushOpenSet replay = %v (frames=%d), want one [c1 c2]", ids, n)
	}
}

// TestStoreOpenChats_WarnsOnFailedPersist proves a persist that cannot be
// written is surfaced, not silently swallowed: with the index closed behind
// it, storeOpenChats still returns calmly (a lost store degrades — the set in
// memory and on the wire stays — it is not fatal) and says so on the WARN
// channel, where the row's loss is diagnosable before a restart drops the
// user's open tabs.
func TestStoreOpenChats_WarnsOnFailedPersist(t *testing.T) {
	warns := captureWarns(t)
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	h.storeOpenChats([]string{"c1"})

	got := warns()
	if len(got) != 1 || !strings.Contains(got[0], "open-set persist") {
		t.Errorf("warns = %v, want exactly one open-set persist warning", got)
	}
}
