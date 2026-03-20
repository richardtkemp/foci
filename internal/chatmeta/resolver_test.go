package chatmeta

import (
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/log"
	"foci/internal/session"
)

var testLogger = log.NewComponentLogger("chatmeta-test")

func testResolver(t *testing.T) *Resolver {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	return &Resolver{
		Index:        idx,
		AgentID:      "test-agent",
		PlatformName: "test",
		Logger:       func() *log.ComponentLogger { return testLogger },
	}
}

// TestSessionKeyForChat_CreatesAndPersists verifies that a new session key is
// created on first access and subsequent calls return the same persisted value.
func TestSessionKeyForChat_CreatesAndPersists(t *testing.T) {
	r := testResolver(t)
	key1 := r.SessionKeyForChat(42)
	if key1 == "" {
		t.Fatal("expected non-empty session key")
	}
	if !strings.HasPrefix(key1, "test-agent/c42/") {
		t.Errorf("key %q missing expected prefix test-agent/c42/", key1)
	}

	// Second call must return the same persisted key.
	key2 := r.SessionKeyForChat(42)
	if key1 != key2 {
		t.Errorf("expected stable key, got %q then %q", key1, key2)
	}
}

// TestSessionKeyForChat_DifferentChats verifies that different chat IDs produce
// different session keys.
func TestSessionKeyForChat_DifferentChats(t *testing.T) {
	r := testResolver(t)
	k1 := r.SessionKeyForChat(1)
	k2 := r.SessionKeyForChat(2)
	if k1 == k2 {
		t.Errorf("different chat IDs should produce different keys, got %q", k1)
	}
}

// TestSessionKeyForChat_NilIndex verifies that the resolver works without a
// session index (creates keys but cannot persist them).
func TestSessionKeyForChat_NilIndex(t *testing.T) {
	r := &Resolver{
		AgentID:      "agent",
		PlatformName: "test",
		Logger:       func() *log.ComponentLogger { return testLogger },
	}
	key := r.SessionKeyForChat(99)
	if key == "" {
		t.Fatal("expected non-empty key even without index")
	}
}

// TestUpdateSessionKey verifies that updating a session key replaces the
// persisted value so subsequent lookups return the new key.
func TestUpdateSessionKey(t *testing.T) {
	r := testResolver(t)
	oldKey := r.SessionKeyForChat(55)
	r.UpdateSessionKey(55, "test-agent/c55/newversion")
	newKey := r.SessionKeyForChat(55)
	if newKey != "test-agent/c55/newversion" {
		t.Errorf("expected updated key, got %q", newKey)
	}
	if oldKey == newKey {
		t.Error("expected key to change after update")
	}
}

// TestDefaultChatID verifies the default chat lookup and platform filtering.
func TestDefaultChatID(t *testing.T) {
	r := testResolver(t)

	// No default set.
	if id := r.DefaultChatID(); id != 0 {
		t.Errorf("expected 0, got %d", id)
	}

	// Set a default for our platform.
	if err := r.Index.SetDefaultChat("test-agent", "test", 12345); err != nil {
		t.Fatal(err)
	}
	if id := r.DefaultChatID(); id != 12345 {
		t.Errorf("expected 12345, got %d", id)
	}
}

// TestDefaultChatID_WrongPlatform verifies that a default set for a different
// platform is not returned.
func TestDefaultChatID_WrongPlatform(t *testing.T) {
	r := testResolver(t)
	if err := r.Index.SetDefaultChat("test-agent", "other-platform", 999); err != nil {
		t.Fatal(err)
	}
	if id := r.DefaultChatID(); id != 0 {
		t.Errorf("expected 0 for wrong platform, got %d", id)
	}
}

// TestDefaultSessionKey verifies end-to-end default session key resolution.
func TestDefaultSessionKey(t *testing.T) {
	r := testResolver(t)

	// No default -> empty.
	if sk := r.DefaultSessionKey(); sk != "" {
		t.Errorf("expected empty, got %q", sk)
	}

	// Set default chat.
	if err := r.Index.SetDefaultChat("test-agent", "test", 12345); err != nil {
		t.Fatal(err)
	}
	sk := r.DefaultSessionKey()
	if !strings.HasPrefix(sk, "test-agent/c12345/") {
		t.Errorf("expected prefix test-agent/c12345/, got %q", sk)
	}

	// Must be stable.
	sk2 := r.DefaultSessionKey()
	if sk != sk2 {
		t.Errorf("not stable: %q vs %q", sk, sk2)
	}
}

// TestRecordUsername verifies that username recording doesn't panic and persists.
func TestRecordUsername(t *testing.T) {
	r := testResolver(t)
	// Should not panic with empty username.
	r.RecordUsername(42, "")
	// Should persist a username.
	r.RecordUsername(42, "alice")
	// Verify by reading back.
	name, err := r.Index.GetChatMetadata("test-agent", "test", 42, "username")
	if err != nil {
		t.Fatal(err)
	}
	if name != "alice" {
		t.Errorf("expected alice, got %q", name)
	}
}

// TestRecordUsername_NilIndex verifies that username recording with nil index
// does not panic.
func TestRecordUsername_NilIndex(t *testing.T) {
	r := &Resolver{
		AgentID:      "agent",
		PlatformName: "test",
		Logger:       func() *log.ComponentLogger { return testLogger },
	}
	r.RecordUsername(42, "alice") // should not panic
}

// TestNilReceiver verifies that all Resolver methods are safe to call on a
// nil *Resolver, returning zero values without panicking.
func TestNilReceiver(t *testing.T) {
	var r *Resolver
	if id := r.DefaultChatID(); id != 0 {
		t.Errorf("expected 0, got %d", id)
	}
	if sk := r.DefaultSessionKey(); sk != "" {
		t.Errorf("expected empty, got %q", sk)
	}
	// SessionKeyForChat on nil still generates a key (for safety).
	key := r.SessionKeyForChat(42)
	if key == "" {
		t.Error("expected non-empty key from nil resolver")
	}
	// These should not panic.
	r.UpdateSessionKey(42, "x")
	r.RecordUsername(42, "alice")
}
