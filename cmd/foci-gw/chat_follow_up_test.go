package main

import (
	"path/filepath"
	"testing"

	"foci/internal/provider"
	"foci/internal/session"
)

// TestChatFollowUpParent is #3159's scope: the reflection passes (interval,
// pre-compaction, session-end) of a chat may send the follow-up to that chat;
// consolidation and keepalive branches, the chat itself, and a reflection of a
// spawn may not (Dick 2026-11-20, Q140).
func TestChatFollowUpParent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := session.NewStore(filepath.Join(dir, "sessions"))
	idx, err := session.NewSessionIndex(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	store.OnSessionEvent(func(e session.SessionEvent) {
		if e.Status == session.SessionStatusActive {
			idx.Upsert(session.SessionIndexEntry{
				SessionKey:       e.Key,
				FilePath:         e.FilePath,
				CreatedAt:        e.CreatedAt,
				ParentSessionKey: e.ParentKey,
				SessionType:      e.Type,
				Status:           session.SessionStatusActive,
			})
		}
	})

	const chat = "clutch/c1"
	if err := store.TestAppend(chat, provider.Message{Role: "user", Content: provider.TextContent("hello")}); err != nil {
		t.Fatalf("append: %v", err)
	}
	idx.Upsert(session.SessionIndexEntry{SessionKey: chat, FilePath: "x", SessionType: session.SessionTypeChat, Status: session.SessionStatusActive})
	spawn, err := store.CreateBranchWithOptions(chat, session.BranchOptions{BranchType: "spawn"})
	if err != nil {
		t.Fatalf("spawn branch: %v", err)
	}
	branch := func(parent, bt string) string {
		t.Helper()
		k, err := store.CreateBranchWithOptions(parent, session.BranchOptions{BranchType: bt})
		if err != nil {
			t.Fatalf("%s branch: %v", bt, err)
		}
		return k
	}

	fn := chatFollowUpParent(store, idx)
	for _, tc := range []struct {
		name, caller, want string
	}{
		{"interval reflection", branch(chat, "reflection"), chat},
		{"pre-compaction reflection", branch(chat, "compaction-memory"), chat},
		{"session-end reflection", branch(chat, "session-end-memory"), chat},
		{"consolidation", branch(chat, "consolidation"), ""},
		{"keepalive", branch(chat, "keepalive"), ""},
		{"reflection of a spawn", branch(spawn, "reflection"), ""},
		{"the chat itself", chat, ""},
	} {
		got, ok := fn(tc.caller)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s (%s): got (%q, %v), want (%q, %v)", tc.name, tc.caller, got, ok, tc.want, tc.want != "")
		}
	}
}
