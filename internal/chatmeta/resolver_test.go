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

// TestSessionKeyForChat_DerivesAndRegisters verifies that the derived key is
// deterministic and stable, and that the first access persists the
// platform-ownership 'registered' row for outbound routing.
func TestSessionKeyForChat_DerivesAndRegisters(t *testing.T) {
	r := testResolver(t)
	key1 := r.SessionKeyForChat(42)
	if key1 != "test-agent/c42" {
		t.Errorf("expected test-agent/c42, got %q", key1)
	}

	// The registered row must exist so PlatformForChat can route.
	v, err := r.Index.GetChatMetadata("test-agent", "test", 42, "registered")
	if err != nil {
		t.Fatal(err)
	}
	if v != "true" {
		t.Errorf("expected registered=true row, got %q", v)
	}

	// Second call must return the same key.
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
// session index (derives keys but cannot register the chat).
func TestSessionKeyForChat_NilIndex(t *testing.T) {
	r := &Resolver{
		AgentID:      "agent",
		PlatformName: "test",
		Logger:       func() *log.ComponentLogger { return testLogger },
	}
	key := r.SessionKeyForChat(99)
	if key != "agent/c99" {
		t.Errorf("expected agent/c99 even without index, got %q", key)
	}
}

// TestRegisterChat_Idempotent verifies that RegisterChat only writes the
// ownership row once per process (cached) and never errors on repeat calls.
func TestRegisterChat_Idempotent(t *testing.T) {
	r := testResolver(t)
	r.RegisterChat(7)
	r.RegisterChat(7) // cached, no-op
	v, err := r.Index.GetChatMetadata("test-agent", "test", 7, "registered")
	if err != nil {
		t.Fatal(err)
	}
	if v != "true" {
		t.Errorf("expected registered=true row after RegisterChat, got %q", v)
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
	if sk != "test-agent/c12345" {
		t.Errorf("expected test-agent/c12345, got %q", sk)
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
	// SessionKeyForChat on nil still derives a key (for safety).
	key := r.SessionKeyForChat(42)
	if !strings.HasSuffix(key, "/c42") {
		t.Errorf("expected derived key from nil resolver, got %q", key)
	}
	// These should not panic.
	r.RegisterChat(42)
	r.RecordUsername(42, "alice")
}
