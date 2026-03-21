package main

import (
	"os"
	"path/filepath"
	"testing"

	"foci/internal/session"
)

// TestCleanupStaleSessionMetadata_RemovesStaleNoCompact proves that no_compact
// entries for sessions whose files no longer exist are removed.
func TestCleanupStaleSessionMetadata_RemovesStaleNoCompact(t *testing.T) {
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	sessDir := filepath.Join(dir, "sessions")
	sessions := session.NewStore(sessDir)

	// Create a no_compact entry for a branch session that doesn't exist on disk
	idx.SetSessionMetadata("fotini/c123/1710000000/b1710000001", "no_compact", "true")

	// Create a no_compact entry for a session that DOES exist on disk
	existingKey := "fotini/c456/1710000000"
	existingPath := filepath.Join(sessDir, existingKey, "root.jsonl")
	os.MkdirAll(filepath.Dir(existingPath), 0o755)
	os.WriteFile(existingPath, []byte("{}"), 0o644)
	idx.SetSessionMetadata(existingKey, "no_compact", "true")

	cleanupStaleSessionMetadata(idx, sessions)

	// Stale entry should be removed
	val, _ := idx.GetSessionMetadata("fotini/c123/1710000000/b1710000001", "no_compact")
	if val != "" {
		t.Error("stale no_compact entry should be removed")
	}

	// Existing session's no_compact should be kept
	val, _ = idx.GetSessionMetadata(existingKey, "no_compact")
	if val != "true" {
		t.Error("no_compact for existing session should be kept")
	}
}

// TestCleanupStaleSessionMetadata_PreservesOtherMetadata proves that cleanup
// only affects no_compact metadata and leaves other metadata untouched.
func TestCleanupStaleSessionMetadata_PreservesOtherMetadata(t *testing.T) {
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	sessions := session.NewStore(filepath.Join(dir, "sessions"))

	// Set non-no_compact metadata that should not be affected
	idx.SetAgentMetadata("fotini", "first_run_completed", "true")
	_ = idx.SetDefaultChat("fotini", "telegram", 12345)

	cleanupStaleSessionMetadata(idx, sessions)

	// All metadata should remain
	val, err := idx.GetAgentMetadata("fotini", "first_run_completed")
	if err != nil || val != "true" {
		t.Errorf("expected first_run_completed=true, got %q (err=%v)", val, err)
	}

	chatID := idx.DefaultChatForAgent("fotini", "telegram")
	if chatID != 12345 {
		t.Errorf("expected default chat=12345, got %d", chatID)
	}
}
