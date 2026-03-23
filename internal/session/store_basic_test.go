package session

import (
	"encoding/json"
	"os"
	"testing"

	"foci/internal/provider"
)

func TestKeyToPath(t *testing.T) {
	// Proves that SessionPath converts session keys to the expected nested
	// directory paths and returns errors for empty or malformed keys.

	s := NewStore("/data/sessions")

	tests := []struct {
		key  string
		want string
	}{
		{"main/imain/1000000000", "/data/sessions/main/imain/1000000000/root.jsonl"},
		{"main/imorning/1000000000", "/data/sessions/main/imorning/1000000000/root.jsonl"},
		{"test/iresearch/1000000000", "/data/sessions/test/iresearch/1000000000/root.jsonl"},
	}

	for _, tt := range tests {
		got, err := s.SessionPath(tt.key)
		if err != nil {
			t.Errorf("keyToPath(%q) unexpected error: %v", tt.key, err)
			continue
		}
		if got != tt.want {
			t.Errorf("keyToPath(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}

	// Empty/malformed keys should return error, not panic
	for _, bad := range []string{"", "agent", "main/c"} {
		_, err := s.SessionPath(bad)
		if err == nil {
			t.Errorf("keyToPath(%q) should return error for malformed key", bad)
		}
	}
}

func TestLoadEmpty(t *testing.T) {
	// Proves that Load returns nil (not an error) when no session file exists yet.
	s := NewStore(t.TempDir())

	msgs, err := s.Load("test/imain/1000000000")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if msgs != nil {
		t.Errorf("Load empty = %v, want nil", msgs)
	}
}

func TestAppendAndLoad(t *testing.T) {
	// Proves the fundamental round-trip: messages appended to a session are returned
	// by Load in the correct order with roles and content intact.
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

	if err := s.TestAppend(key, msg("user", "hello")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.TestAppend(key, msg("assistant", "hi there")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	msgs, err := s.Load(key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2", len(msgs))
	}
	if msgs[0].Role != "user" || provider.TextOf(msgs[0].Content) != "hello" {
		t.Errorf("msgs[0] = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || provider.TextOf(msgs[1].Content) != "hi there" {
		t.Errorf("msgs[1] = %+v", msgs[1])
	}
}

func TestAppendAll(t *testing.T) {
	// Proves that AppendAll writes an entire batch of messages atomically and they
	// are all retrievable via Load in the same order.
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

	batch := []provider.Message{
		msg("user", "one"),
		msg("assistant", "two"),
		msg("user", "three"),
	}
	if err := s.TestAppendAll(key, batch); err != nil {
		t.Fatalf("AppendAll: %v", err)
	}

	msgs, err := s.Load(key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("len = %d, want 3", len(msgs))
	}
}

func TestClear(t *testing.T) {
	// Proves that Clear removes all messages from a session so that subsequent
	// Load returns nil.
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

	s.TestAppend(key, msg("user", "hello"))

	if err := s.TestClear(key); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	msgs, err := s.Load(key)
	if err != nil {
		t.Fatalf("Load after Clear: %v", err)
	}
	if msgs != nil {
		t.Errorf("Load after Clear = %v, want nil", msgs)
	}
}

func TestClearNonexistent(t *testing.T) {
	// Proves that Clear on a non-existent session key is a no-op that returns no error.
	s := NewStore(t.TempDir())
	if err := s.TestClear("ghost/imain/1000000000"); err != nil {
		t.Fatalf("Clear nonexistent: %v", err)
	}
}

func TestReplace(t *testing.T) {
	// Proves that Replace atomically overwrites a session's content so only the
	// replacement messages are visible via Load.
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

	// Write initial messages
	s.TestAppend(key, msg("user", "old1"))
	s.TestAppend(key, msg("assistant", "old2"))
	s.TestAppend(key, msg("user", "old3"))

	// Replace
	replacement := []provider.Message{
		msg("user", "summary"),
		msg("assistant", "acknowledged"),
	}
	if err := s.TestReplace(key, replacement); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	msgs, err := s.Load(key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2", len(msgs))
	}
	if provider.TextOf(msgs[0].Content) != "summary" {
		t.Errorf("msgs[0] text = %q", provider.TextOf(msgs[0].Content))
	}
}

func TestMessageCount(t *testing.T) {
	// Proves that MessageCount returns 0 for an empty session and correctly counts
	// messages as they are appended.
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

	n, _ := s.MessageCount(key)
	if n != 0 {
		t.Errorf("empty count = %d", n)
	}

	s.TestAppend(key, msg("user", "a"))
	s.TestAppend(key, msg("assistant", "b"))

	n, _ = s.MessageCount(key)
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}


func TestAppendAllAtomicOnMarshalError(t *testing.T) {
	// Verify that if one message in a batch fails to marshal, NO messages
	// are written to disk. This prevents partial writes that cause duplicate
	// tool_use IDs when a defer safety-net re-writes the same messages.
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

	// Pre-populate with one message
	if err := s.TestAppend(key, msg("user", "existing")); err != nil {
		t.Fatalf("setup Append: %v", err)
	}

	// Create a batch where the second message fails to marshal.
	// Invalid json.RawMessage in a tool_use Input field causes json.Marshal to error.
	good := msg("assistant", "should not appear")
	bad := provider.Message{
		Role: "assistant",
		Content: []provider.ContentBlock{{
			Type:  "tool_use",
			ID:    "toolu_01",
			Name:  "test",
			Input: json.RawMessage("!!!invalid"),
		}},
	}

	err := s.TestAppendAll(key, []provider.Message{good, bad})
	if err == nil {
		t.Fatal("expected marshal error, got nil")
	}

	// Verify only the original message is on disk — the batch wrote nothing
	msgs, err := s.Load(key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message on disk, got %d", len(msgs))
	}
	if provider.TextOf(msgs[0].Content) != "existing" {
		t.Errorf("unexpected message content: %q", provider.TextOf(msgs[0].Content))
	}
}

func TestFileMode(t *testing.T) {
	// Proves that SetFileMode controls the permissions on session files created
	// via Append (new session), CreateBranch, and Replace.
	s := NewStore(t.TempDir())
	s.SetFileMode(0640)

	key := "test/imain/1000000000"
	branchKey := "test/imain/1000000000/b1000000001"

	// Append creates a new session file
	s.TestAppend(key, msg("user", "hello"))
	checkMode(t, s, key, 0640)

	// Branch file
	s.CreateBranchWithOptions(key, branchKey, BranchOptions{})
	checkMode(t, s, branchKey, 0640)

	// Replace rewrites with the configured mode
	s.TestReplace(key, []provider.Message{msg("user", "replaced")})
	checkMode(t, s, key, 0640)
}

func TestFileModeDefault(t *testing.T) {
	// Proves that NewStore defaults to 0600 without explicit SetFileMode.
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"
	s.TestAppend(key, msg("user", "hello"))
	checkMode(t, s, key, 0600)
}

func checkMode(t *testing.T, s *Store, key string, want os.FileMode) {
	t.Helper()
	path, err := s.SessionPath(key)
	if err != nil {
		t.Fatalf("SessionPath(%s): %v", key, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	got := info.Mode().Perm()
	if got != want {
		t.Errorf("file mode for %s = %04o, want %04o", key, got, want)
	}
}

func TestAppendCreatesDirectories(t *testing.T) {
	// Proves that Append automatically creates all required intermediate directories
	// for a session key that has never been written before.
	dir := t.TempDir()
	s := NewStore(dir)
	// Deep key that requires nested directories
	key := "mybot/idaily/1000000000"
	if err := s.TestAppend(key, msg("user", "wake up")); err != nil {
		t.Fatalf("Append deep key: %v", err)
	}

	msgs, _ := s.Load(key)
	if len(msgs) != 1 {
		t.Errorf("len = %d, want 1", len(msgs))
	}
}
