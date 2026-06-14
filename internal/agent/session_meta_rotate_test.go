package agent

import (
	"context"
	"path/filepath"
	"testing"

	"foci/internal/session"
	"foci/internal/tools"
)

func TestRotateSession(t *testing.T) {
	// Proves that RotateSession atomically moves all session state (effort, no_compact, session metadata, turn lock) from the old key to the new one and fires the registered rotation callback.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ag := &Agent{SessionIndex: idx}

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
	if ag.SessionEffort(oldKey) != "" {
		t.Errorf("old key should return empty default effort, got %q", ag.SessionEffort(oldKey))
	}

	// Verify session metadata keys migrated
	val, err := idx.GetSessionMetadata(newKey, "effort")
	if err != nil {
		t.Fatalf("get effort for new key: %v", err)
	}
	if val != "high" {
		t.Errorf("effort session metadata not migrated: %q", val)
	}
	oldVal, err := idx.GetSessionMetadata(oldKey, "effort")
	if err != nil {
		t.Fatalf("get effort for old key: %v", err)
	}
	if oldVal != "" {
		t.Error("old effort session metadata key should be deleted")
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

func TestRotateSession_DoesNotMigrateBackend(t *testing.T) {
	// Proves RotateSession leaves a live delegated backend under the OLD key —
	// it must never drag a running CC onto the rotated-to key. The async soft
	// reset depends on this: the old backend stays put for background reflection
	// while the fresh key starts with no backend (lazy spawn on next message).
	idx := newTestSessionIndex(t)
	mgr, _ := newTestManager(t, idx)
	t.Cleanup(func() { mgr.Close() })

	oldKey := "bot/c100/1000000000"
	newKey := "bot/c100/2000000000"

	if _, err := mgr.Get(context.Background(), oldKey); err != nil {
		t.Fatalf("Get: %v", err)
	}

	ag := &Agent{SessionIndex: idx, DelegatedManager: mgr}
	ag.RotateSession(oldKey, newKey)

	if _, ok := mgr.getManaged(oldKey); !ok {
		t.Error("backend should still be mapped under the old key after RotateSession")
	}
	if _, ok := mgr.getManaged(newKey); ok {
		t.Error("backend must NOT be migrated to the new key by RotateSession")
	}
}

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
