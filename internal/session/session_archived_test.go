package session

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestSessionArchived covers which sessions count as archived for a send
// (#3024): a session is archived when it has an app conversation and every
// one of its app conversations is archived, directly or through its depth root.
func TestSessionArchived(t *testing.T) {
	idx, err := NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	set := func(platform string, chatID int64, key, value string) {
		t.Helper()
		if err := idx.SetChatMetadata("scout", platform, chatID, key, value); err != nil {
			t.Fatalf("SetChatMetadata: %v", err)
		}
	}
	archive := func(chatID int64) {
		t.Helper()
		if err := idx.SetArchivedChat("scout", "app", chatID, true); err != nil {
			t.Fatalf("SetArchivedChat: %v", err)
		}
	}

	// Chat 1: a live app chat. Chat 2: an archived app chat.
	set("app", 1, "conv_id", "conv-1")
	set("app", 2, "conv_id", "conv-2")
	archive(2)
	// Chat 3: a telegram chat (never archived: archive is app-only).
	set("telegram", 3, "registered", "true")
	// Chat 4: an archived app chat adopted onto the named session scout/inamed.
	set("app", 4, "conv_id", "conv-4")
	set("app", 4, "session_key", "scout/inamed")
	archive(4)
	// Chats 5 and 6: two conversations on scout/itwo, only one archived.
	set("app", 5, "session_key", "scout/itwo")
	set("app", 6, "session_key", "scout/itwo")
	archive(5)
	// Chat 7: a depth leaf of archived root chat 2, without its own flag.
	set("app", 7, "session_key", "scout/c2/b1")
	set("app", 7, "depth_root", "conv-2")
	// Chat 8: a depth leaf of live root chat 1.
	set("app", 8, "session_key", "scout/c1/b2")
	set("app", 8, "depth_root", "conv-1")
	// Chat 9: a depth leaf with its own flag (synced from its root).
	set("app", 9, "session_key", "scout/c1/b3")
	set("app", 9, "depth_root", "conv-1")
	archive(9)

	for _, tc := range []struct {
		key  string
		want bool
	}{
		{"scout/c1", false},
		{"scout/c2", true},
		{"scout/c3", false},
		{"scout/inamed", true},
		{"scout/c4", false}, // chat 4 belongs to scout/inamed now
		{"scout/itwo", false},
		{"scout/c2/b1", true},
		{"scout/c1/b2", false},
		{"scout/c1/b3", true},
		{"scout/c2/b9", false}, // a plain branch of an archived chat has no conversation
		{"scout/c99", false},   // no conversation at all
		{"other/c2", false},    // another agent's chat 2
		{"", false},
	} {
		if got := idx.SessionArchived(tc.key); got != tc.want {
			t.Errorf("SessionArchived(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}

	// Unarchive is reversible.
	if err := idx.SetArchivedChat("scout", "app", 2, false); err != nil {
		t.Fatal(err)
	}
	if idx.SessionArchived("scout/c2") || idx.SessionArchived("scout/c2/b1") {
		t.Error("an unarchived chat (and its depth leaf) still reads as archived")
	}

	err = &ArchivedSessionError{SessionKey: "scout/c2"}
	if !errors.Is(err, ErrSessionArchived) {
		t.Error("ArchivedSessionError does not match ErrSessionArchived")
	}
}
