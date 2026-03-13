package agent

import (
	"path/filepath"
	"testing"

	"foci/internal/state"
	"foci/internal/tools"
)

// TestRotateSession verifies that RotateSession migrates the meta map,
// StateStore keys, turn locks, and fires the rotation callback.
func TestRotateSession(t *testing.T) {
	// Proves that RotateSession atomically moves all session state (effort, no_compact, state store keys, turn lock) from the old key to the new one and fires the registered rotation callback.
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

// TestRotateSession_MigratesAsyncPending verifies that RotateSession calls
// AsyncNotifier.MigrateSession so that in-flight async goroutines holding the
// old key resolve to the new key for both delivery and pending tracking.
func TestRotateSession_MigratesAsyncPending(t *testing.T) {
	// Proves that pending async tool results and MarkDone calls using the old session key are transparently redirected to the new key after RotateSession.
	var delivered string
	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		delivered = sk
	})

	ag := &Agent{AsyncNotifier: notifier}

	oldKey := "bot/c100/1000000000"
	newKey := "bot/c100/2000000000"

	// Simulate an async tool dispatched before rotation
	notifier.MarkPending(oldKey)

	ag.RotateSession(oldKey, newKey)

	// Pending should now be on the new key
	if notifier.HasPending(oldKey) {
		t.Error("old key should not have pending after rotation")
	}
	if !notifier.HasPending(newKey) {
		t.Error("new key should have pending after rotation")
	}

	// InjectToAgent with old key should deliver to new key
	notifier.InjectToAgent(oldKey, "async result", "", "")
	if delivered != newKey {
		t.Errorf("InjectToAgent delivered to %q, want %q", delivered, newKey)
	}

	// MarkDone with old key should decrement new key's count
	notifier.MarkDone(oldKey)
	if notifier.HasPending(newKey) {
		t.Error("new key should have no pending after MarkDone")
	}
}

// TestRotateSession_NoOp verifies that RotateSession is a no-op when
// oldKey equals newKey or newKey is empty.
func TestRotateSession_NoOp(t *testing.T) {
	// Proves that RotateSession does not fire the rotation callback when the old and new keys are identical or the new key is empty.
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
