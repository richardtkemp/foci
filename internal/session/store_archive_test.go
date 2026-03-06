package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/provider"
)

func TestListChatSessionsSkipsArchives(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	// Create a real chat session
	key := "test/c555/1000000000"
	s.TestAppend(key, msg("user", "hello"))

	// Simulate an archive file by creating it directly (using timestamp pattern)
	timestamp := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	archiveDir := filepath.Join(dir, "test", "c555", "1000000000")
	os.MkdirAll(archiveDir, 0755)
	archivePath := filepath.Join(archiveDir, fmt.Sprintf("root.%s.jsonl", timestamp))
	os.WriteFile(archivePath, []byte(`{"role":"user","content":[{"type":"text","text":"old"}]}`+"\n"), 0644)

	sessions, err := s.ListChatSessions("test")
	if err != nil {
		t.Fatalf("ListChatSessions: %v", err)
	}

	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1 (archive should be skipped)", len(sessions))
	}
	if sessions[0].ChatID != 555 {
		t.Errorf("ChatID = %d, want 555", sessions[0].ChatID)
	}
}

func TestRepairOrphansSkipsArchives(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	// Create a session with an orphaned tool_use
	key := "test/c444/1000000000"
	s.TestAppend(key, msg("user", "hello"))
	s.TestAppend(key, provider.Message{
		Role: "assistant",
		Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "tool_1", Name: "exec", Input: json.RawMessage(`{}`)},
		},
	})

	// Create an archive file with the same orphaned pattern (using timestamp pattern)
	archiveDir := filepath.Join(dir, "test", "c444", "1000000000")
	os.MkdirAll(archiveDir, 0755)
	timestamp := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	archiveData := `{"role":"user","content":[{"type":"text","text":"old"}]}` + "\n" +
		`{"role":"assistant","content":[{"type":"tool_use","id":"tool_2","name":"shell","input":{}}]}` + "\n"
	os.WriteFile(filepath.Join(archiveDir, fmt.Sprintf("root.%s.jsonl", timestamp)), []byte(archiveData), 0644)

	repaired, err := s.RepairOrphans()
	if err != nil {
		t.Fatalf("RepairOrphans: %v", err)
	}

	// Should only repair the current file, not the archive
	if repaired != 1 {
		t.Errorf("repaired = %d, want 1 (archive should be skipped)", repaired)
	}
}

func TestReplaceNonexistentFile(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/c333/1000000000"

	// Replace on a key with no existing file should work (no rotation needed)
	compacted := []provider.Message{
		msg("user", "fresh"),
	}
	if err := s.TestReplace(key, compacted); err != nil {
		t.Fatalf("Replace nonexistent: %v", err)
	}

	msgs, _ := s.Load(key)
	if len(msgs) != 1 {
		t.Fatalf("len = %d, want 1", len(msgs))
	}
}

func TestIsArchiveFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"5970082313.jsonl", false},
		{"5970082313.1.jsonl", true},                                    // old numbered pattern
		{"5970082313.2.jsonl", true},                                    // old numbered pattern
		{"5970082313.10.jsonl", true},                                   // old numbered pattern
		{"5970082313.2026-03-04T02-30-00Z.jsonl", true},                 // new timestamp pattern
		{"5970082313.2026-03-04T02-30-00Z.2.jsonl", true},               // new timestamp pattern with counter
		{"5970082313.2026-03-04T02-30-00Z.10.jsonl", true},              // new timestamp pattern with counter
		{"wake-111.2026-12-25T14-35-22Z.jsonl", true},                   // new timestamp pattern
		{"wake-111.2026-12-25T14-35-22Z.3.jsonl", true},                 // new timestamp pattern with counter
		{"main.jsonl", false},
		{"wake-111.jsonl", false},
		{"wake-111.1.jsonl", true},                                      // old numbered pattern
		{"invalid.2026-03-04.jsonl", false},                             // invalid timestamp (missing time)
		{"invalid.2026-03-04T02-30-00.jsonl", false},                    // invalid timestamp (missing Z)
		{"invalid.abc.jsonl", false},                                    // invalid suffix
		{"invalid.2026-03-04T02-30-00Z.abc.jsonl", false},               // timestamp with invalid counter
	}
	for _, tt := range tests {
		if got := isArchiveFile(tt.name); got != tt.want {
			t.Errorf("isArchiveFile(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestSessionWriter verifies that SessionWriter prevents cross-session writes for all operations.
func TestSessionWriter(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	sessionA := "test/imain/1000000000"
	sessionB := "test/ibranch/1000000001"

	// Create a writer for session A
	writerA := store.For(sessionA)

	// Test Append: should successfully append to session A
	msgA := msg("user", "message for session A")
	if err := writerA.Append(sessionA, msgA); err != nil {
		t.Fatalf("SessionWriter.Append to own session failed: %v", err)
	}

	// Test Append: should reject append to session B (cross-session write)
	msgB := msg("user", "message for session B")
	err := writerA.Append(sessionB, msgB)
	if err == nil {
		t.Fatal("SessionWriter.Append to different session should have failed")
	}
	if !strings.Contains(err.Error(), "cross-session write blocked") {
		t.Errorf("error should mention cross-session write, got: %v", err)
	}

	// Test AppendAll: should successfully append multiple messages to session A
	msgs := []provider.Message{
		msg("user", "msg1"),
		msg("user", "msg2"),
	}
	if err := writerA.AppendAll(sessionA, msgs); err != nil {
		t.Fatalf("SessionWriter.AppendAll to own session failed: %v", err)
	}

	// Test AppendAll: should reject cross-session writes
	if err := writerA.AppendAll(sessionB, msgs); err == nil {
		t.Fatal("SessionWriter.AppendAll to different session should have failed")
	}

	// Test Replace: should successfully replace session A
	replaceMessages := []provider.Message{msg("user", "replaced content")}
	if err := writerA.Replace(sessionA, replaceMessages); err != nil {
		t.Fatalf("SessionWriter.Replace to own session failed: %v", err)
	}

	// Test Replace: should reject cross-session writes
	if err := writerA.Replace(sessionB, replaceMessages); err == nil {
		t.Fatal("SessionWriter.Replace to different session should have failed")
	}

	// Test Clear: should successfully clear session A
	if err := writerA.Clear(sessionA); err != nil {
		t.Fatalf("SessionWriter.Clear on own session failed: %v", err)
	}

	// Test Clear: should reject cross-session writes
	if err := writerA.Clear(sessionB); err == nil {
		t.Fatal("SessionWriter.Clear on different session should have failed")
	}

	// Verify session A is empty after clear
	loadedMsgs, err := store.Load(sessionA)
	if err != nil {
		t.Fatalf("Load session A: %v", err)
	}
	if len(loadedMsgs) != 0 {
		t.Fatalf("session A should be empty after clear, got %d messages", len(loadedMsgs))
	}

	// Verify session B was never written to
	loadedMsgs, err = store.Load(sessionB)
	if err != nil {
		t.Fatalf("Load session B: %v", err)
	}
	if len(loadedMsgs) != 0 {
		t.Fatalf("session B should have 0 messages, got %d", len(loadedMsgs))
	}
}
