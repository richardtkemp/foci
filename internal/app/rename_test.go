package app

import (
	"path/filepath"
	"testing"
	"time"

	"foci/internal/fap"
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
	b := withIdent(&convBinding{convID: "c1", chatID: 42}, "clutch", "")

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
	b := withIdent(&convBinding{convID: "c1", chatID: 42}, "clutch", "")
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

// TestHandleConversationRename_Claim pins #2709: a plain rename to a taken name
// is refused with alias_taken; the same rename with Claim takes the name and
// renames the holder to "<name> <its last activity>".
func TestHandleConversationRename_Claim(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	act := time.Date(2026, 10, 31, 14, 2, 0, 0, time.Local)
	old := withIdent(&convBinding{convID: "c1", chatID: 41, lastActMs: act.UnixMilli()}, "clutch", "")
	b := withIdent(&convBinding{convID: "c2", chatID: 42}, "clutch", "")
	h.convs["c1"], h.convs["c2"] = old, b
	if err := idx.SetChatAliasUnique("clutch", "app", 41, "Holiday"); err != nil {
		t.Fatal(err)
	}
	// A chat with no binding and no stored frames also holds a claimed name.
	if err := idx.SetChatAliasUnique("clutch", "app", 43, "Work"); err != nil {
		t.Fatal(err)
	}

	c := fakeClient()
	h.handleConversationRename(c, fap.ConversationRename{ConversationID: "c2", Title: "holiday"})
	var code string
	for _, f := range drain(t, c) {
		if f.t == fap.TypeError {
			code, _ = f.d["code"].(string)
		}
	}
	if code != "alias_taken" {
		t.Fatalf("plain rename to a taken name: error code = %q, want alias_taken", code)
	}
	if got := h.aliasFor(b); got != "" {
		t.Fatalf("refused rename wrote alias %q", got)
	}

	h.handleConversationRename(fakeClient(), fap.ConversationRename{ConversationID: "c2", Title: " holiday ", Claim: true})
	if got := h.aliasFor(b); got != "holiday" {
		t.Fatalf("claimer alias = %q, want \"holiday\"", got)
	}
	if got, want := h.aliasFor(old), "Holiday 2026-10-31 14:02"; got != want {
		t.Fatalf("displaced alias = %q, want %q", got, want)
	}

	h.handleConversationRename(fakeClient(), fap.ConversationRename{ConversationID: "c2", Title: "Work", Claim: true})
	if got, want := h.aliasForChat("clutch", 43), "Work (old)"; got != want {
		t.Fatalf("displaced alias with no activity = %q, want %q", got, want)
	}
}
