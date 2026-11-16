package main

import (
	"path/filepath"
	"testing"

	"foci/internal/session"
)

// TestOpenChatArchiveHold: the archive sweep's hold keeps exactly the
// sessions of open app chats uncompressed (#3134), and holds every session
// when the open set cannot be read, rather than reading it as "nothing open".
func TestOpenChatArchiveHold(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	if err := idx.SetChatMetadata("alpha", "app", 7, "conv_id", "conv-open"); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetChatMetadata("alpha", "app", 7, "session_key", "alpha/c1/b7"); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetSystemState(session.SystemStateOpenChats, `["conv-open"]`); err != nil {
		t.Fatal(err)
	}
	hold := openChatArchiveHold(idx)
	if !hold(session.SessionIndexEntry{SessionKey: "alpha/c1/b7"}) {
		t.Error("open chat's session not held")
	}
	if hold(session.SessionIndexEntry{SessionKey: "alpha/c1/b8"}) {
		t.Error("closed session held")
	}

	if err := idx.SetSystemState(session.SystemStateOpenChats, `garbage`); err != nil {
		t.Fatal(err)
	}
	if !openChatArchiveHold(idx)(session.SessionIndexEntry{SessionKey: "alpha/c1/b8"}) {
		t.Error("unreadable open set must hold every session")
	}
}
