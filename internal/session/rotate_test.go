package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/provider"
)

func TestRotateKey(t *testing.T) {
	// TestRotateKey verifies that RotateKey archives the old root.jsonl, returns
	// a new key with an updated VersionTS, and that the old path is gone while
	// the archive exists.
	dir := t.TempDir()
	store := NewStore(dir)

	oldKey := "bot/c100/1000000000"
	store.TestAppend(oldKey, msg("user", "hello"))

	oldPath := mustSessionPath(t, store, oldKey)
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("old file should exist before rotation: %v", err)
	}

	// Capture event
	var event SessionEvent
	store.OnSessionEvent(func(e SessionEvent) { event = e })

	newKey, err := store.RotateKey(oldKey)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}

	// New key should have different VersionTS but same agentID/type/ID
	if newKey == oldKey {
		t.Fatal("expected different key after rotation")
	}
	oldParsed, _ := ParseSessionKey(oldKey)
	newParsed, parseErr := ParseSessionKey(newKey)
	if parseErr != nil {
		t.Fatalf("parse new key: %v", parseErr)
	}
	if newParsed.AgentID != oldParsed.AgentID || newParsed.Type != oldParsed.Type || newParsed.ID != oldParsed.ID {
		t.Errorf("key identity changed: old=%s new=%s", oldKey, newKey)
	}
	if newParsed.VersionTS == oldParsed.VersionTS {
		t.Error("VersionTS should have changed")
	}

	// Old file should be gone (renamed to archive)
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error("old root.jsonl should not exist after rotation")
	}

	// Archive file should exist in the old directory
	archiveDir := filepath.Dir(oldPath)
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatalf("read archive dir: %v", err)
	}
	found := false
	for _, e := range entries {
		if isArchiveFile(e.Name()) {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected archive file in old session directory")
	}

	// No new file should be created (lazy creation)
	newPath, _ := store.SessionPath(newKey)
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Error("RotateKey should not create a new file (lazy creation)")
	}

	// Verify event
	if event.Status != SessionStatusRotated {
		t.Errorf("event.Status = %q, want rotated", event.Status)
	}
	if event.Key != oldKey {
		t.Errorf("event.Key = %q, want %q", event.Key, oldKey)
	}
	if event.NewKey != newKey {
		t.Errorf("event.NewKey = %q, want %q", event.NewKey, newKey)
	}
}

func TestReplaceAndRotate(t *testing.T) {
	// TestReplaceAndRotate verifies that ReplaceAndRotate archives the old file,
	// writes new messages to the rotated key path, and preserves metadata.
	dir := t.TempDir()
	store := NewStore(dir)

	oldKey := "bot/c200/1000000000"

	// Create session with messages
	store.TestAppend(oldKey, msg("user", "original 1"))
	store.TestAppend(oldKey, msg("assistant", "reply 1"))
	store.TestAppend(oldKey, msg("user", "original 2"))
	store.TestAppend(oldKey, msg("assistant", "reply 2"))

	oldPath := mustSessionPath(t, store, oldKey)

	// Capture event
	var event SessionEvent
	store.OnSessionEvent(func(e SessionEvent) { event = e })

	compacted := []provider.Message{
		msg("user", "[compacted]"),
		msg("assistant", "summary"),
		msg("user", "handoff"),
	}

	newKey, err := store.ReplaceAndRotate(oldKey, compacted)
	if err != nil {
		t.Fatalf("ReplaceAndRotate: %v", err)
	}

	if newKey == oldKey {
		t.Fatal("expected different key after rotation")
	}

	// Old file should be archived
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error("old root.jsonl should not exist after rotation")
	}

	// New key should have messages
	msgs, err := store.Load(newKey)
	if err != nil {
		t.Fatalf("load new key: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("new key has %d messages, want 3", len(msgs))
	}
	if provider.TextOf(msgs[0].Content) != "[compacted]" {
		t.Errorf("msgs[0] = %q, want [compacted]", provider.TextOf(msgs[0].Content))
	}

	// Verify key format
	newParsed, _ := ParseSessionKey(newKey)
	oldParsed, _ := ParseSessionKey(oldKey)
	if newParsed.AgentID != oldParsed.AgentID {
		t.Error("AgentID should be preserved")
	}
	if newParsed.Type != oldParsed.Type {
		t.Error("Type should be preserved")
	}
	if newParsed.ID != oldParsed.ID {
		t.Error("ID should be preserved")
	}
	if newParsed.VersionTS == oldParsed.VersionTS {
		t.Error("VersionTS should have changed")
	}

	// Verify event
	if event.Status != SessionStatusRotated {
		t.Errorf("event.Status = %q, want rotated", event.Status)
	}
	if event.NewKey != newKey {
		t.Errorf("event.NewKey = %q, want %q", event.NewKey, newKey)
	}
	if event.ArchivePath == "" {
		t.Error("event.ArchivePath should be set")
	}

	// Session meta should be preserved in new file
	createdAt := store.CreatedAt(newKey)
	if createdAt == "n/a" {
		t.Error("CreatedAt should be preserved through rotation")
	}
}

func TestReplaceAndRotate_PreservesCreatedAt(t *testing.T) {
	// TestReplaceAndRotate_PreservesCreatedAt verifies that the original session's
	// creation time is carried through to the rotated key.
	dir := t.TempDir()
	store := NewStore(dir)
	key := "bot/c300/1000000000"

	store.TestAppend(key, msg("user", "hello"))
	originalCreatedAt := store.CreatedAt(key)
	if originalCreatedAt == "n/a" {
		t.Fatal("expected non-n/a CreatedAt")
	}

	newKey, err := store.ReplaceAndRotate(key, []provider.Message{
		msg("user", "[compacted]"),
		msg("assistant", "summary"),
	})
	if err != nil {
		t.Fatalf("ReplaceAndRotate: %v", err)
	}

	newCreatedAt := store.CreatedAt(newKey)
	if newCreatedAt != originalCreatedAt {
		t.Errorf("CreatedAt changed: %q → %q", originalCreatedAt, newCreatedAt)
	}
}

func TestRotateKey_NoFile(t *testing.T) {
	// TestRotateKey_NoFile verifies RotateKey works when the session file doesn't exist.
	dir := t.TempDir()
	store := NewStore(dir)

	newKey, err := store.RotateKey("bot/c100/1000000000")
	if err != nil {
		t.Fatalf("RotateKey on missing file: %v", err)
	}
	if newKey == "bot/c100/1000000000" {
		t.Fatal("expected new key even with no file")
	}
	if !strings.HasPrefix(newKey, "bot/c100/") {
		t.Errorf("new key should preserve prefix: %q", newKey)
	}
}
