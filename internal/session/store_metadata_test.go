package session

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"foci/internal/provider"
)

func TestCreatedAtNewSession(t *testing.T) {
	// Proves that CreatedAt returns "n/a" before any messages are written and a
	// valid RFC3339 timestamp after the first append.
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

	createdAt := s.CreatedAt(key)
	if createdAt != "n/a" {
		t.Errorf("CreatedAt on new session = %q, want 'n/a'", createdAt)
	}

	// Append first message - should create session with creation time
	s.TestAppend(key, msg("user", "hello"))

	createdAt = s.CreatedAt(key)
	if createdAt == "n/a" {
		t.Error("CreatedAt after append should not be n/a")
	}
	// Should be a valid RFC3339 timestamp (20 chars for UTC "Z" or 25 chars for offset "+01:00")
	if _, err := time.Parse(time.RFC3339, createdAt); err != nil {
		t.Errorf("CreatedAt timestamp format = %q, not valid RFC3339: %v", createdAt, err)
	}
}

func TestCreatedAtPreservedThroughReplace(t *testing.T) {
	// Proves that the original creation timestamp survives a Replace (compaction)
	// operation and is unchanged in the resulting file.
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

	// Create session
	s.TestAppend(key, msg("user", "hello"))

	originalCreatedAt := s.CreatedAt(key)
	if originalCreatedAt == "n/a" {
		t.Fatal("expected creation time after append")
	}

	// Replace (simulating compaction)
	replacement := []provider.Message{
		msg("user", "summary"),
	}
	s.TestReplace(key, replacement)

	// Creation time should be preserved
	newCreatedAt := s.CreatedAt(key)
	if newCreatedAt != originalCreatedAt {
		t.Errorf("CreatedAt after Replace = %q, want %q", newCreatedAt, originalCreatedAt)
	}
}

func TestCreatedAtWrittenOnFirstAppend(t *testing.T) {
	// Proves that the first line of a new session file is a session_meta record
	// with a non-empty created_at field, by inspecting the raw JSONL on disk.
	dir := t.TempDir()
	s := NewStore(dir)
	key := "test/imain/1000000000"

	s.TestAppend(key, msg("user", "hello"))

	// Verify session_meta is written by reading raw file
	path, _ := s.SessionPath(key)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines, got %d", len(lines))
	}

	var meta SessionMeta
	if err := json.Unmarshal([]byte(lines[0]), &meta); err != nil {
		t.Fatalf("unmarshal first line: %v", err)
	}
	if meta.Type != "session_meta" {
		t.Errorf("first line type = %q, want session_meta", meta.Type)
	}
	if meta.CreatedAt == "" {
		t.Error("first line missing created_at")
	}
}

func TestCreatedAtPreservedAfterRestart(t *testing.T) {
	// Proves that CreatedAt reads the persisted value from disk on a fresh store
	// instance, meaning the timestamp survives process restart.
	dir := t.TempDir()
	key := "test/imain/1000000000"

	// Create session with first store instance
	s1 := NewStore(dir)
	s1.TestAppend(key, msg("user", "hello"))
	originalCreatedAt := s1.CreatedAt(key)
	if originalCreatedAt == "n/a" {
		t.Fatal("expected creation time after append")
	}

	// Simulate restart by creating new store instance
	s2 := NewStore(dir)
	newCreatedAt := s2.CreatedAt(key)
	if newCreatedAt != originalCreatedAt {
		t.Errorf("CreatedAt after restart = %q, want %q", newCreatedAt, originalCreatedAt)
	}
}

func TestCreatedAtPreservedWithChangedMtime(t *testing.T) {
	// Proves that CreatedAt returns the value embedded in the session_meta record,
	// not the file's modification time — it's stable even when mtime changes.
	dir := t.TempDir()
	key := "test/imain/1000000000"

	s := NewStore(dir)
	s.TestAppend(key, msg("user", "hello"))
	originalCreatedAt := s.CreatedAt(key)
	if originalCreatedAt == "n/a" {
		t.Fatal("expected creation time after append")
	}

	// Modify file mtime (simulating external modification)
	path, _ := s.SessionPath(key)
	newTime := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(path, newTime, newTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// CreatedAt should still return stored value, not file mtime
	newCreatedAt := s.CreatedAt(key)
	if newCreatedAt != originalCreatedAt {
		t.Errorf("CreatedAt after mtime change = %q, want %q", newCreatedAt, originalCreatedAt)
	}
}

func TestLastActivity(t *testing.T) {
	// Proves that LastActivity returns a valid RFC3339 timestamp after a message
	// has been written to the session file.
	s := NewStore(t.TempDir())
	key := "test/c123/1000000000"

	// Write a message to create the file
	s.TestAppend(key, msg("user", "test message"))

	// Get the last activity time
	lastActivity := s.LastActivity(key)

	// Should be a valid RFC3339 formatted timestamp
	if lastActivity == "n/a" {
		t.Error("LastActivity should return a timestamp, not n/a")
	}
	if len(lastActivity) < 19 {
		t.Errorf("LastActivity = %q, doesn't look like RFC3339 format", lastActivity)
	}
}

func TestLastActivity_Missing(t *testing.T) {
	// Proves that LastActivity returns "n/a" when the session file does not exist.
	s := NewStore(t.TempDir())
	key := "test/c999/1000000000"

	// Try to get activity for non-existent session
	lastActivity := s.LastActivity(key)

	// Should return "n/a"
	if lastActivity != "n/a" {
		t.Errorf("LastActivity for missing session = %q, want 'n/a'", lastActivity)
	}
}

func TestLastActivity_InvalidKey(t *testing.T) {
	// Proves that LastActivity returns "n/a" for a malformed key that cannot be
	// resolved to a valid file path.
	s := NewStore(t.TempDir())

	// Try with invalid key (missing parts)
	lastActivity := s.LastActivity("invalid")

	// Should return "n/a" due to SessionPath error
	if lastActivity != "n/a" {
		t.Errorf("LastActivity with invalid key = %q, want 'n/a'", lastActivity)
	}
}
