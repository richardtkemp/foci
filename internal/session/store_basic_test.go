package session

import (
	"testing"

	"foci/internal/provider"
)

func TestKeyToPath(t *testing.T) {
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
	s := NewStore(t.TempDir())
	if err := s.TestClear("ghost/imain/1000000000"); err != nil {
		t.Fatalf("Clear nonexistent: %v", err)
	}
}

func TestReplace(t *testing.T) {
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

func TestLoadFullRegularSession(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

	s.TestAppend(key, msg("user", "hello"))
	s.TestAppend(key, msg("assistant", "world"))

	msgs, err := s.LoadFull(key)
	if err != nil {
		t.Fatalf("LoadFull: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2", len(msgs))
	}
}

func TestAppendCreatesDirectories(t *testing.T) {
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
