package main

import (
	"os"
	"path/filepath"
	"testing"

	"foci/internal/session"
	"foci/internal/state"
)


// TestCleanupLegacyStateKeys_RemovesStaleNoCompact proves that no_compact
// entries for sessions whose files no longer exist are removed.
func TestCleanupLegacyStateKeys_RemovesStaleNoCompact(t *testing.T) {
	dir := t.TempDir()
	stateStore := state.New(filepath.Join(dir, "state.json"))
	sessDir := filepath.Join(dir, "sessions")
	sessions := session.NewStore(sessDir)

	// Create a no_compact entry for a branch session that doesn't exist on disk
	stateStore.Set("no_compact/fotini/c123/1710000000/b1710000001", "true")

	// Create a no_compact entry for a session that DOES exist on disk
	existingKey := "fotini/c456/1710000000"
	existingPath := filepath.Join(sessDir, existingKey, "root.jsonl")
	os.MkdirAll(filepath.Dir(existingPath), 0o755)
	os.WriteFile(existingPath, []byte("{}"), 0o644)
	stateStore.Set("no_compact/"+existingKey, "true")

	cleanupLegacyStateKeys(stateStore, sessions)

	// Stale entry should be removed
	var val string
	if stateStore.Get("no_compact/fotini/c123/1710000000/b1710000001", &val) {
		t.Error("stale no_compact entry should be removed")
	}

	// Existing session's no_compact should be kept
	if !stateStore.Get("no_compact/"+existingKey, &val) {
		t.Error("no_compact for existing session should be kept")
	}
}

// TestCleanupLegacyStateKeys_NoopWhenClean proves cleanup is a no-op
// when there are no legacy keys.
func TestCleanupLegacyStateKeys_NoopWhenClean(t *testing.T) {
	dir := t.TempDir()
	stateStore := state.New(filepath.Join(dir, "state.json"))
	sessions := session.NewStore(filepath.Join(dir, "sessions"))

	stateStore.Set("agent/fotini/first_run_completed", true)
	stateStore.Set("some_other_key", "value")

	cleanupLegacyStateKeys(stateStore, sessions)

	// All keys should remain
	keys := stateStore.AllKeys()
	if len(keys) != 2 {
		t.Errorf("expected 2 keys, got %d: %v", len(keys), keys)
	}
}
