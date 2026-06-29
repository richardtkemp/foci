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

// TestHandleConversationArchive proves archive purges frames, drops the binding,
// and fires the final-reflection callback with the session key.
func TestHandleConversationArchive(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	s := tempFrameStore(t)
	h.frames = s
	now := time.Now().UnixMilli()
	s.insert(frameWrite{convID: "c1", agentID: "clutch", seq: 1, wire: "w", sentMs: now, visible: true})

	b := &convBinding{convID: "c1", agentID: "clutch", chatID: 42, sessionKey: "clutch/capp/9"}
	h.convs["c1"] = b
	h.bySession[b.sessionKey] = b

	var reflected string
	h.reflectOnArchive = func(key string) { reflected = key }

	h.handleConversationArchive(nil, fap.ConversationArchive{ConversationID: "c1"})

	if h.convs["c1"] != nil || h.bySession[b.sessionKey] != nil {
		t.Error("archive must drop the binding from convs + bySession")
	}
	if len(s.RestorableConvs()) != 0 {
		t.Error("archive must purge the conv's frames")
	}
	if reflected != "clutch/capp/9" {
		t.Errorf("reflectOnArchive fired with %q, want the session key", reflected)
	}

	// Unknown conversation: no panic, callback not fired.
	reflected = ""
	h.handleConversationArchive(nil, fap.ConversationArchive{ConversationID: "ghost"})
	if reflected != "" {
		t.Error("archive of unknown conv must not fire reflection")
	}
}
