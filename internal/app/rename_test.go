package app

import (
	"path/filepath"
	"testing"

	"foci/internal/app/fap"
	"foci/internal/platform"
	"foci/internal/session"
)

func newTestIndex(t *testing.T) *session.SessionIndex {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

// TestAliasFor_RoundTrip proves aliasFor reads the persisted chat_metadata alias.
func TestAliasFor_RoundTrip(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	b := &convBinding{convID: "c1", agentID: "clutch", chatID: 42}

	if got := h.aliasFor(b); got != "" {
		t.Fatalf("aliasFor (unset) = %q, want \"\"", got)
	}
	if err := idx.SetChatMetadata("clutch", "app", 42, "alias", "Holiday plans"); err != nil {
		t.Fatal(err)
	}
	if got := h.aliasFor(b); got != "Holiday plans" {
		t.Fatalf("aliasFor = %q, want \"Holiday plans\"", got)
	}
}

// TestHandleConversationRename_Persists proves the rename handler trims and persists
// the alias (keyed by the stable chatID) so it survives — and that the roster then
// surfaces it via aliasFor.
func TestHandleConversationRename_Persists(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	b := &convBinding{convID: "c1", agentID: "clutch", chatID: 42}
	h.convs["c1"] = b

	h.handleConversationRename(fakeClient(), fap.ConversationRename{ConversationID: "c1", Title: "  Holiday  "})

	if got, _ := idx.GetChatMetadata("clutch", "app", 42, "alias"); got != "Holiday" {
		t.Fatalf("persisted alias = %q, want \"Holiday\" (trimmed)", got)
	}
	if got := h.aliasFor(b); got != "Holiday" {
		t.Fatalf("aliasFor after rename = %q, want \"Holiday\"", got)
	}

	// Unknown conversation: no panic, no write.
	h.handleConversationRename(fakeClient(), fap.ConversationRename{ConversationID: "ghost", Title: "x"})
}
