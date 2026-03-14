package main

import (
	"os"
	"path/filepath"
	"testing"

	"foci/internal/config"
	"foci/internal/session"
	"foci/internal/state"
)

// TestCheckFirstRun_LegacyKeyMigration proves that checkFirstRun detects
// colon-separated legacy keys, migrates them to slash format, and returns
// empty (no first-run prompt needed).
func TestCheckFirstRun_LegacyKeyMigration(t *testing.T) {
	dir := t.TempDir()
	stateStore := state.New(filepath.Join(dir, "state.json"))

	// Set legacy colon-separated key (as old code would have written)
	stateStore.Set("agent:fotini:first_run_completed", true)

	acfg := config.AgentConfig{ID: "fotini"}
	result := checkFirstRun(stateStore, acfg)
	if result != "" {
		t.Errorf("checkFirstRun returned %q, want empty (legacy key should be detected)", result)
	}

	// Verify migration: new slash key should exist
	var completed bool
	if !stateStore.Get("agent/fotini/first_run_completed", &completed) || !completed {
		t.Error("slash-separated key should exist after migration")
	}

	// Verify cleanup: legacy key should be deleted
	if stateStore.Get("agent:fotini:first_run_completed", &completed) {
		t.Error("colon-separated key should be deleted after migration")
	}
}

// TestCheckFirstRun_SlashKeyTakesPrecedence proves that when the slash key
// already exists, the legacy key check is never reached.
func TestCheckFirstRun_SlashKeyTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	stateStore := state.New(filepath.Join(dir, "state.json"))

	// Both keys exist (as happens for agents that completed first-run under new code)
	stateStore.Set("agent/test/first_run_completed", true)
	stateStore.Set("agent:test:first_run_completed", true)

	acfg := config.AgentConfig{ID: "test"}
	result := checkFirstRun(stateStore, acfg)
	if result != "" {
		t.Errorf("checkFirstRun returned %q, want empty", result)
	}

	// Legacy key should still exist (wasn't touched since slash key was found first)
	var completed bool
	if !stateStore.Get("agent:test:first_run_completed", &completed) {
		t.Error("legacy key should be untouched when slash key exists")
	}
}

// TestCleanupLegacyStateKeys_MigratesColonKeys proves that colon-separated
// agent keys are migrated to slash format and the old keys deleted.
func TestCleanupLegacyStateKeys_MigratesColonKeys(t *testing.T) {
	dir := t.TempDir()
	stateStore := state.New(filepath.Join(dir, "state.json"))
	sessions := session.NewStore(filepath.Join(dir, "sessions"))

	stateStore.Set("agent:fotini:first_run_completed", true)
	stateStore.Set("agent:scout:first_run_completed", true)
	stateStore.Set("unrelated_key", "keep")

	cleanupLegacyStateKeys(stateStore, sessions)

	// Migrated keys should exist under slash format
	var completed bool
	if !stateStore.Get("agent/fotini/first_run_completed", &completed) || !completed {
		t.Error("fotini key not migrated to slash format")
	}
	if !stateStore.Get("agent/scout/first_run_completed", &completed) || !completed {
		t.Error("scout key not migrated to slash format")
	}

	// Old colon keys should be deleted
	if stateStore.Get("agent:fotini:first_run_completed", &completed) {
		t.Error("fotini colon key should be deleted")
	}
	if stateStore.Get("agent:scout:first_run_completed", &completed) {
		t.Error("scout colon key should be deleted")
	}

	// Unrelated keys should be untouched
	var val string
	if !stateStore.Get("unrelated_key", &val) || val != "keep" {
		t.Error("unrelated key should be untouched")
	}
}

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
