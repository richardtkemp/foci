package agent

import (
	"path/filepath"
	"testing"

	"foci/internal/state"
)

// TestRotateSession verifies that RotateSession migrates the meta map,
// StateStore keys, turn locks, and fires the rotation callback.
func TestRotateSession(t *testing.T) {
	stateStore := state.New(filepath.Join(t.TempDir(), "state.json"))
	ag := &Agent{StateStore: stateStore}

	oldKey := "bot/c100/1000000000"
	newKey := "bot/c100/2000000000"

	// Set up session metadata
	ag.SetSessionEffort(oldKey, "high")
	ag.SetSessionNoCompact(oldKey, true)

	// Get a turn lock for the old key
	oldLock := ag.turnLock(oldKey)
	_ = oldLock // just ensure it's created

	// Track callback
	var callbackOld, callbackNew string
	ag.SessionKeyRotatedFunc.Add(func(old, new string) {
		callbackOld = old
		callbackNew = new
	})

	// Rotate
	ag.RotateSession(oldKey, newKey)

	// Verify meta migrated
	if ag.SessionEffort(newKey) != "high" {
		t.Errorf("effort not migrated: got %q", ag.SessionEffort(newKey))
	}
	if !ag.SessionNoCompact(newKey) {
		t.Error("no_compact not migrated")
	}

	// Old key should have empty/default values
	if ag.SessionEffort(oldKey) != ag.Effort {
		t.Errorf("old key should return agent default effort, got %q", ag.SessionEffort(oldKey))
	}

	// Verify StateStore keys migrated
	var val string
	if stateStore.Get("effort:"+newKey, &val) && val != "high" {
		t.Errorf("effort StateStore key not migrated: %q", val)
	}
	if stateStore.Get("effort:"+oldKey, &val) && val != "" {
		t.Error("old effort StateStore key should be deleted")
	}

	// Verify turn lock migrated
	newLock := ag.turnLock(newKey)
	if newLock != oldLock {
		t.Error("turn lock should be the same object after migration")
	}

	// Verify callback fired
	if callbackOld != oldKey || callbackNew != newKey {
		t.Errorf("callback: old=%q new=%q, want %q/%q", callbackOld, callbackNew, oldKey, newKey)
	}
}

// TestRotateSession_NoOp verifies that RotateSession is a no-op when
// oldKey equals newKey or newKey is empty.
func TestRotateSession_NoOp(t *testing.T) {
	ag := &Agent{}

	var called bool
	ag.SessionKeyRotatedFunc.Add(func(old, new string) {
		called = true
	})

	// Same key
	ag.RotateSession("bot/c100/1", "bot/c100/1")
	if called {
		t.Error("should not fire callback for same key")
	}

	// Empty new key
	ag.RotateSession("bot/c100/1", "")
	if called {
		t.Error("should not fire callback for empty new key")
	}
}
