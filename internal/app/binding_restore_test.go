package app

import (
	"context"
	"testing"
	"time"

	"foci/internal/app/fap"
	"foci/internal/platform"
)

// TestFrameStore_RestorableConvs proves the restore set is exactly the convs with
// a VISIBLE frame and a known agent_id — invisible-only convs (typing) and legacy
// rows with no agent are excluded, and the result is distinct.
func TestFrameStore_RestorableConvs(t *testing.T) {
	s := tempFrameStore(t)
	now := time.Now().UnixMilli()
	// c1: visible + agent → restorable (twice, to prove DISTINCT).
	s.insert(frameWrite{convID: "c1", agentID: "clutch", seq: 1, wire: "w", sentMs: now, visible: false})
	s.insert(frameWrite{convID: "c1", agentID: "clutch", seq: 2, wire: "w", sentMs: now, visible: true})
	// c2: visible but NO agent (legacy) → excluded.
	s.insert(frameWrite{convID: "c2", agentID: "", seq: 1, wire: "w", sentMs: now, visible: true})
	// c3: agent but only invisible (typing) → excluded.
	s.insert(frameWrite{convID: "c3", agentID: "clutch", seq: 1, wire: "w", sentMs: now, visible: false})

	got := s.RestorableConvs()
	if len(got) != 1 || got[0].convID != "c1" || got[0].agentID != "clutch" {
		t.Fatalf("RestorableConvs = %+v, want only {c1, clutch}", got)
	}
}

// TestFrameStore_PurgeConv proves archiving a conv removes all its frames so it
// drops out of the restore set.
func TestFrameStore_PurgeConv(t *testing.T) {
	s := tempFrameStore(t)
	now := time.Now().UnixMilli()
	s.insert(frameWrite{convID: "c1", agentID: "clutch", seq: 1, wire: "w", sentMs: now, visible: true})
	s.insert(frameWrite{convID: "c2", agentID: "clutch", seq: 1, wire: "w", sentMs: now, visible: true})

	if n := s.PurgeConv("c1"); n != 1 {
		t.Fatalf("PurgeConv removed %d, want 1", n)
	}
	got := s.RestorableConvs()
	if len(got) != 1 || got[0].convID != "c2" {
		t.Fatalf("after purge RestorableConvs = %+v, want only c2", got)
	}
}

// TestStartAll_RestoresBindings proves bindings are rebuilt from the durable store
// at startup (socketless), so bindingForSession resolves before the app reconnects.
func TestStartAll_RestoresBindings(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	s := tempFrameStore(t)
	h.frames = s
	now := time.Now().UnixMilli()
	s.insert(frameWrite{convID: "c1", agentID: "clutch", seq: 1, wire: "w", sentMs: now, visible: true})
	s.insert(frameWrite{convID: "c2", agentID: "clutch", seq: 1, wire: "w", sentMs: now, visible: true})

	h.StartAll(context.Background())

	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.convs["c1"] == nil || h.convs["c2"] == nil {
		t.Fatalf("StartAll did not restore both bindings: %v", h.convs)
	}
	if b := h.convs["c1"]; b.client != nil {
		t.Error("restored binding must be socketless (nil client) until the app reconnects")
	}
}

// TestHandleConversationArchive proves archive sets the is_archived flag
// (reversibly) and leaves the binding + frames intact — it does NOT purge,
// drop the binding, or flip session status. Round-trips through unarchive.
func TestHandleConversationArchive(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	s := tempFrameStore(t)
	h.frames = s
	now := time.Now().UnixMilli()
	s.insert(frameWrite{convID: "c1", agentID: "clutch", seq: 1, wire: "w", sentMs: now, visible: true})

	b := &convBinding{convID: "c1", agentID: "clutch", chatID: 42, sessionKey: "clutch/c42"}
	h.convs["c1"] = b
	h.bySession[b.sessionKey] = b

	// Archive: flag set, binding + frames retained.
	h.handleConversationArchive(fakeClient(), fap.ConversationArchive{ConversationID: "c1", Archived: true})
	if !idx.ArchivedChatsForAgent("clutch", "app")[42] {
		t.Error("archive must set the is_archived flag for chatID 42")
	}
	if h.convs["c1"] == nil || h.bySession[b.sessionKey] == nil {
		t.Error("archive must NOT drop the binding (flag-based, not destructive)")
	}
	if len(s.RestorableConvs()) != 1 {
		t.Error("archive must NOT purge frames (history retained for unarchive)")
	}

	// Unarchive: flag cleared, binding + frames still intact.
	h.handleConversationArchive(fakeClient(), fap.ConversationArchive{ConversationID: "c1", Archived: false})
	if idx.ArchivedChatsForAgent("clutch", "app")[42] {
		t.Error("unarchive must clear the is_archived flag")
	}
	if h.convs["c1"] == nil {
		t.Error("unarchive must leave the binding live")
	}

	// Unknown conversation: no panic, no flag written.
	h.handleConversationArchive(fakeClient(), fap.ConversationArchive{ConversationID: "ghost", Archived: true})
	if len(idx.ArchivedChatsForAgent("clutch", "app")) != 0 {
		t.Error("archive of unknown conv must not write a flag")
	}
}

// TestAgentRoster_MarksArchivedConversation proves the roster surfaces Archived
// for chats flagged is_archived on the app platform, and only those — mirroring
// the IsDefault roster test. The roster is the app's source of truth for
// archived state across devices and fresh pairings.
func TestAgentRoster_MarksArchivedConversation(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	registerBareAgent(h, "ag")
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "ag", chatID: 42}
	h.convs["c2"] = &convBinding{convID: "c2", agentID: "ag", chatID: 99}
	if err := idx.SetArchivedChat("ag", "app", 99, true); err != nil {
		t.Fatal(err)
	}

	roster := h.agentRoster()
	if len(roster) != 1 {
		t.Fatalf("roster = %d agents, want 1", len(roster))
	}
	var archived, total int
	for _, ci := range roster[0].Conversations {
		total++
		if ci.Archived {
			archived++
			if ci.ID != "c2" {
				t.Errorf("Archived set on %q, want c2 (chatID 99)", ci.ID)
			}
		}
	}
	if total != 2 || archived != 1 {
		t.Fatalf("roster convs total=%d archived=%d, want 2/1", total, archived)
	}
}
