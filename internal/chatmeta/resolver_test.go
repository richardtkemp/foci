package chatmeta

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/log"
	"foci/internal/platform"
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

// TestStableChatID_Pinned pins the exact int64 chat IDs the FNV-64a
// derivation produces for fixed identities — the same literals as the app's
// TestChatIDForConv_PinnedValues, so the shared function (the body was lifted
// from the app's chatIDForConv) can never drift from what app conversations
// already hash to, and Slack identities get the same stability guarantee.
// conv-delta and the slack/thread identity have bit 62 set in their hashes:
// the derivation masks with 0x7fff…, and those pins fail if the mask is ever
// narrowed (e.g. to 0x3fff…), which would silently rename every conversation
// whose hash has that bit set.
func TestStableChatID_Pinned(t *testing.T) {
	for _, tc := range []struct {
		identity string
		want     int64
	}{
		{"conv-alpha", 720718529578504038},
		{"conv-beta", 3836012143067692060},
		{"conv-delta", 6838516087377924408},
		{"singleton", 4486002201837802786},
		{"slack/thread/C200/1700000000.000200", 5736613814651099991},
	} {
		if got := StableChatID(tc.identity); got != tc.want {
			t.Errorf("StableChatID(%q) = %d, want %d", tc.identity, got, tc.want)
		}
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

// TestResolverRegisterChat_OrdinaryChatWritesNoFlag lives in
// resolver_ordinary_test.go (a base-compilable characterisation file).

// TestResolverNeverDefaultWritesFlag proves a resolver with NeverDefault set
// (the Slack transport's shape, #2459) stamps the never_default row on
// registration, alongside the ordinary registered row, with no error.
func TestResolverNeverDefaultWritesFlag(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	r := &Resolver{
		Index:        idx,
		AgentID:      "helen",
		PlatformName: "slack",
		Logger:       func() *log.ComponentLogger { return testLogger },
		NeverDefault: true,
	}
	if err := r.RegisterChat(31); err != nil {
		t.Fatalf("RegisterChat: %v", err)
	}
	for key, want := range map[string]string{
		"registered":    "true",
		"never_default": "true",
	} {
		if v, err := idx.GetChatMetadata("helen", "slack", 31, key); err != nil || v != want {
			t.Errorf("row %s = %q (err %v), want %q", key, v, err, want)
		}
	}
}

// failingIndex wraps a real index and fails the SetChatMetadata keys listed
// in fail, so a test can drive RegisterChat's two write-error paths.
type failingIndex struct {
	platform.SessionIndex
	fail map[string]bool
}

func (f *failingIndex) SetChatMetadata(agentID, platformName string, chatID int64, key, value string) error {
	if f.fail[key] {
		return fmt.Errorf("injected failure for key %q", key)
	}
	return f.SessionIndex.SetChatMetadata(agentID, platformName, chatID, key, value)
}

// TestRegisterChat_NeverDefaultWriteFailureWritesNeitherRow proves the
// fail-closed ordering (#2459 req 6): when the never_default row cannot be
// written, NO row lands — a chat that could not be marked never-default must
// not become a registered, default-eligible chat. The error is returned (the
// Slack transport refuses the triggering message on it), logged as an ERROR
// naming the chat, and not cached, so the next contact retries.
func TestRegisterChat_NeverDefaultWriteFailureWritesNeitherRow(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	fake := &failingIndex{SessionIndex: idx, fail: map[string]bool{session.ChatMetaNeverDefault: true}}
	r := &Resolver{
		Index:        fake,
		AgentID:      "helen",
		PlatformName: "slack",
		Logger:       func() *log.ComponentLogger { return testLogger },
		NeverDefault: true,
	}

	var logged []string
	remove := log.AddTap(log.ERROR, func(level log.Level, component, msg string) {
		if strings.Contains(msg, "never-default") {
			logged = append(logged, msg)
		}
	})
	t.Cleanup(remove)

	if err := r.RegisterChat(31); err == nil {
		t.Fatal("RegisterChat swallowed the never-default write failure; want it propagated so the caller refuses the message")
	}
	for _, key := range []string{"registered", session.ChatMetaNeverDefault} {
		if v, err := idx.GetChatMetadata("helen", "slack", 31, key); err == nil && v != "" {
			t.Errorf("row %s = %q despite the failed never-default write; want no row (fail closed)", key, v)
		}
	}
	found := false
	for _, msg := range logged {
		if strings.Contains(msg, "chat 31") {
			found = true
		}
	}
	if !found {
		t.Errorf("no ERROR naming chat 31's failed never-default write; tapped: %q", logged)
	}

	// The failed registration is retried on the next contact, not cached.
	fake.fail = nil
	if err := r.RegisterChat(31); err != nil {
		t.Fatalf("retry after the write recovered: %v", err)
	}
	for key, want := range map[string]string{
		"registered":                 "true",
		session.ChatMetaNeverDefault: "true",
	} {
		if v, err := idx.GetChatMetadata("helen", "slack", 31, key); err != nil || v != want {
			t.Errorf("after retry, row %s = %q (err %v), want %q", key, v, err, want)
		}
	}
}

// TestRegisterChat_RegisteredWriteFailureKeepsFlagAndRetries proves the
// registered-row failure path: the never_default mark (written first, and
// only ever excluding) stays, no registered row lands, the error is returned
// for the caller to refuse the message on, and the next contact retries.
func TestRegisterChat_RegisteredWriteFailureKeepsFlagAndRetries(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	fake := &failingIndex{SessionIndex: idx, fail: map[string]bool{"registered": true}}
	r := &Resolver{
		Index:        fake,
		AgentID:      "helen",
		PlatformName: "slack",
		Logger:       func() *log.ComponentLogger { return testLogger },
		NeverDefault: true,
	}
	if err := r.RegisterChat(31); err == nil {
		t.Fatal("RegisterChat swallowed the registered write failure; want it propagated so the caller refuses the message")
	}
	if v, err := idx.GetChatMetadata("helen", "slack", 31, "registered"); err == nil && v != "" {
		t.Errorf("registered row = %q despite the failed write; want no row", v)
	}
	if v, err := idx.GetChatMetadata("helen", "slack", 31, session.ChatMetaNeverDefault); err != nil || v != "true" {
		t.Errorf("never_default row = %q (err %v); want it kept — it only ever excludes", v, err)
	}

	fake.fail = nil
	if err := r.RegisterChat(31); err != nil {
		t.Fatalf("retry after the write recovered: %v", err)
	}
	if v, err := idx.GetChatMetadata("helen", "slack", 31, "registered"); err != nil || v != "true" {
		t.Errorf("after retry, registered row = %q (err %v), want true", v, err)
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

// TestSessionKeyForChat_NeverDefaultFailsClosed proves a NeverDefault
// resolver never hands out a session key for a chat whose registration
// failed (#2459 A1 review F3, req 6): a chat that could not be marked
// never-default must not become an addressable — and so default-eligible —
// session. Either write failing returns "" (RegisterChat logs the ERROR);
// once the writes land, the next contact returns the key. An ordinary
// resolver keeps its best-effort contract and returns the key regardless.
func TestSessionKeyForChat_NeverDefaultFailsClosed(t *testing.T) {
	for _, failKey := range []string{session.ChatMetaNeverDefault, "registered"} {
		t.Run(failKey, func(t *testing.T) {
			idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			fake := &failingIndex{SessionIndex: idx, fail: map[string]bool{failKey: true}}
			r := &Resolver{
				Index:        fake,
				AgentID:      "helen",
				PlatformName: "slack",
				Logger:       func() *log.ComponentLogger { return testLogger },
				NeverDefault: true,
			}
			if got := r.SessionKeyForChat(42); got != "" {
				t.Errorf("SessionKeyForChat with a failed %s write = %q, want \"\" (fail closed)", failKey, got)
			}
			fake.fail = nil
			if got, want := r.SessionKeyForChat(42), session.NewChatSessionKey("helen", 42); got != want {
				t.Errorf("SessionKeyForChat after the write recovered = %q, want %q", got, want)
			}
		})
	}

	t.Run("ordinary resolver stays best-effort", func(t *testing.T) {
		idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		r := &Resolver{
			Index:        &failingIndex{SessionIndex: idx, fail: map[string]bool{"registered": true}},
			AgentID:      "main",
			PlatformName: "telegram",
			Logger:       func() *log.ComponentLogger { return testLogger },
		}
		if got, want := r.SessionKeyForChat(7), session.NewChatSessionKey("main", 7); got != want {
			t.Errorf("ordinary SessionKeyForChat with a failed write = %q, want %q", got, want)
		}
	})
}
