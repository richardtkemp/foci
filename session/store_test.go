package session

import (
	"testing"

	"clod/anthropic"
)

func msg(role, text string) anthropic.Message {
	return anthropic.Message{
		Role:    role,
		Content: anthropic.TextContent(text),
	}
}

func TestKeyToPath(t *testing.T) {
	s := NewStore("/data/sessions")

	tests := []struct {
		key  string
		want string
	}{
		{"agent:main:main", "/data/sessions/agent/main/main.jsonl"},
		{"agent:main:cron:morning", "/data/sessions/agent/main/cron/morning.jsonl"},
		{"agent:test:subagent:research", "/data/sessions/agent/test/subagent/research.jsonl"},
	}

	for _, tt := range tests {
		got := s.keyToPath(tt.key)
		if got != tt.want {
			t.Errorf("keyToPath(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

func TestLoadEmpty(t *testing.T) {
	s := NewStore(t.TempDir())

	msgs, err := s.Load("agent:test:main")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if msgs != nil {
		t.Errorf("Load empty = %v, want nil", msgs)
	}
}

func TestAppendAndLoad(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "agent:test:main"

	if err := s.Append(key, msg("user", "hello")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Append(key, msg("assistant", "hi there")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	msgs, err := s.Load(key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2", len(msgs))
	}
	if msgs[0].Role != "user" || anthropic.TextOf(msgs[0].Content) != "hello" {
		t.Errorf("msgs[0] = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || anthropic.TextOf(msgs[1].Content) != "hi there" {
		t.Errorf("msgs[1] = %+v", msgs[1])
	}
}

func TestAppendAll(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "agent:test:main"

	batch := []anthropic.Message{
		msg("user", "one"),
		msg("assistant", "two"),
		msg("user", "three"),
	}
	if err := s.AppendAll(key, batch); err != nil {
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
	key := "agent:test:main"

	s.Append(key, msg("user", "hello"))

	if err := s.Clear(key); err != nil {
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
	if err := s.Clear("agent:ghost:main"); err != nil {
		t.Fatalf("Clear nonexistent: %v", err)
	}
}

func TestReplace(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "agent:test:main"

	// Write initial messages
	s.Append(key, msg("user", "old1"))
	s.Append(key, msg("assistant", "old2"))
	s.Append(key, msg("user", "old3"))

	// Replace
	replacement := []anthropic.Message{
		msg("user", "summary"),
		msg("assistant", "acknowledged"),
	}
	if err := s.Replace(key, replacement); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	msgs, err := s.Load(key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2", len(msgs))
	}
	if anthropic.TextOf(msgs[0].Content) != "summary" {
		t.Errorf("msgs[0] text = %q", anthropic.TextOf(msgs[0].Content))
	}
}

func TestMessageCount(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "agent:test:main"

	n, _ := s.MessageCount(key)
	if n != 0 {
		t.Errorf("empty count = %d", n)
	}

	s.Append(key, msg("user", "a"))
	s.Append(key, msg("assistant", "b"))

	n, _ = s.MessageCount(key)
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}

func TestLoadFullRegularSession(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "agent:test:main"

	s.Append(key, msg("user", "hello"))
	s.Append(key, msg("assistant", "world"))

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
	key := "agent:mybot:cron:daily"
	if err := s.Append(key, msg("user", "wake up")); err != nil {
		t.Fatalf("Append deep key: %v", err)
	}

	msgs, _ := s.Load(key)
	if len(msgs) != 1 {
		t.Errorf("len = %d, want 1", len(msgs))
	}
}
