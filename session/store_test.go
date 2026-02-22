package session

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

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

func TestCreatedAtNewSession(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "agent:test:main"

	createdAt := s.CreatedAt(key)
	if createdAt != "n/a" {
		t.Errorf("CreatedAt on new session = %q, want 'n/a'", createdAt)
	}

	// Append first message - should create session with creation time
	s.Append(key, msg("user", "hello"))

	createdAt = s.CreatedAt(key)
	if createdAt == "n/a" {
		t.Error("CreatedAt after append should not be n/a")
	}
	// Should be a valid timestamp
	if len(createdAt) != 20 { // "2006-01-02T15:04:05Z" length
		t.Errorf("CreatedAt timestamp format = %q, want RFC3339 format", createdAt)
	}
}

func TestCreatedAtPreservedThroughReplace(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "agent:test:main"

	// Create session
	s.Append(key, msg("user", "hello"))

	originalCreatedAt := s.CreatedAt(key)
	if originalCreatedAt == "n/a" {
		t.Fatal("expected creation time after append")
	}

	// Replace (simulating compaction)
	replacement := []anthropic.Message{
		msg("user", "summary"),
	}
	s.Replace(key, replacement)

	// Creation time should be preserved
	newCreatedAt := s.CreatedAt(key)
	if newCreatedAt != originalCreatedAt {
		t.Errorf("CreatedAt after Replace = %q, want %q", newCreatedAt, originalCreatedAt)
	}
}

func TestCreatedAtWrittenOnFirstAppend(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	key := "agent:test:main"

	s.Append(key, msg("user", "hello"))

	// Verify session_meta is written by reading raw file
	data, err := os.ReadFile(s.keyToPath(key))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines, got %d", len(lines))
	}

	var meta SessionMeta
	if err := json.Unmarshal([]byte(lines[0]), &meta); err != nil {
		t.Fatalf("unmarshal first line: %v", err)
	}
	if meta.Type != "session_meta" {
		t.Errorf("first line type = %q, want session_meta", meta.Type)
	}
	if meta.CreatedAt == "" {
		t.Error("first line missing created_at")
	}
}

func TestCreatedAtPreservedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	key := "agent:test:main"

	// Create session with first store instance
	s1 := NewStore(dir)
	s1.Append(key, msg("user", "hello"))
	originalCreatedAt := s1.CreatedAt(key)
	if originalCreatedAt == "n/a" {
		t.Fatal("expected creation time after append")
	}

	// Simulate restart by creating new store instance
	s2 := NewStore(dir)
	newCreatedAt := s2.CreatedAt(key)
	if newCreatedAt != originalCreatedAt {
		t.Errorf("CreatedAt after restart = %q, want %q", newCreatedAt, originalCreatedAt)
	}
}

func TestCreatedAtPreservedWithChangedMtime(t *testing.T) {
	dir := t.TempDir()
	key := "agent:test:main"

	s := NewStore(dir)
	s.Append(key, msg("user", "hello"))
	originalCreatedAt := s.CreatedAt(key)
	if originalCreatedAt == "n/a" {
		t.Fatal("expected creation time after append")
	}

	// Modify file mtime (simulating external modification)
	path := s.keyToPath(key)
	newTime := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(path, newTime, newTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// CreatedAt should still return stored value, not file mtime
	newCreatedAt := s.CreatedAt(key)
	if newCreatedAt != originalCreatedAt {
		t.Errorf("CreatedAt after mtime change = %q, want %q", newCreatedAt, originalCreatedAt)
	}
}

// --- RepairOrphans tests ---

func toolUseMsg(ids ...string) anthropic.Message {
	var blocks []anthropic.ContentBlock
	for _, id := range ids {
		blocks = append(blocks, anthropic.ContentBlock{
			Type:  "tool_use",
			ID:    id,
			Name:  "exec",
			Input: []byte(`{"command":"ls"}`),
		})
	}
	return anthropic.Message{Role: "assistant", Content: blocks}
}

func TestRepairOrphansDetectsTrailingToolUse(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "agent:test:main"

	s.Append(key, msg("user", "hello"))
	s.Append(key, toolUseMsg("toolu_123"))

	n, err := s.RepairOrphans()
	if err != nil {
		t.Fatalf("RepairOrphans: %v", err)
	}
	if n != 1 {
		t.Fatalf("repaired = %d, want 1", n)
	}

	msgs, err := s.Load(key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("len = %d, want 3", len(msgs))
	}

	repair := msgs[2]
	if repair.Role != "user" {
		t.Errorf("repair role = %q, want user", repair.Role)
	}
	if len(repair.Content) != 1 {
		t.Fatalf("repair content blocks = %d, want 1", len(repair.Content))
	}
	block := repair.Content[0]
	if block.Type != "tool_result" {
		t.Errorf("block type = %q, want tool_result", block.Type)
	}
	if block.ToolUseID != "toolu_123" {
		t.Errorf("tool_use_id = %q, want toolu_123", block.ToolUseID)
	}
	if !block.IsError {
		t.Error("expected is_error = true")
	}
	if block.Content != "Tool call interrupted by service restart" {
		t.Errorf("content = %q", block.Content)
	}
}

func TestRepairOrphansNoOpWhenClean(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "agent:test:main"

	s.Append(key, msg("user", "hello"))
	s.Append(key, msg("assistant", "hi"))
	s.Append(key, msg("user", "bye"))

	n, err := s.RepairOrphans()
	if err != nil {
		t.Fatalf("RepairOrphans: %v", err)
	}
	if n != 0 {
		t.Errorf("repaired = %d, want 0", n)
	}

	msgs, _ := s.Load(key)
	if len(msgs) != 3 {
		t.Errorf("len = %d, want 3 (unchanged)", len(msgs))
	}
}

func TestRepairOrphansMultipleSessions(t *testing.T) {
	s := NewStore(t.TempDir())

	// Broken session
	broken := "agent:test:main"
	s.Append(broken, msg("user", "hello"))
	s.Append(broken, toolUseMsg("toolu_aaa"))

	// Clean session
	clean := "agent:test:cron:daily"
	s.Append(clean, msg("user", "wake"))
	s.Append(clean, msg("assistant", "done"))

	n, err := s.RepairOrphans()
	if err != nil {
		t.Fatalf("RepairOrphans: %v", err)
	}
	if n != 1 {
		t.Errorf("repaired = %d, want 1", n)
	}

	// Broken should be repaired
	msgs, _ := s.Load(broken)
	if len(msgs) != 3 {
		t.Errorf("broken len = %d, want 3", len(msgs))
	}

	// Clean should be unchanged
	msgs, _ = s.Load(clean)
	if len(msgs) != 2 {
		t.Errorf("clean len = %d, want 2", len(msgs))
	}
}

func TestRepairOrphansMultipleToolUse(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "agent:test:main"

	s.Append(key, msg("user", "do things"))
	s.Append(key, toolUseMsg("toolu_one", "toolu_two"))

	n, err := s.RepairOrphans()
	if err != nil {
		t.Fatalf("RepairOrphans: %v", err)
	}
	if n != 1 {
		t.Fatalf("repaired = %d, want 1", n)
	}

	msgs, _ := s.Load(key)
	if len(msgs) != 3 {
		t.Fatalf("len = %d, want 3", len(msgs))
	}

	repair := msgs[2]
	if len(repair.Content) != 2 {
		t.Fatalf("repair blocks = %d, want 2", len(repair.Content))
	}
	if repair.Content[0].ToolUseID != "toolu_one" {
		t.Errorf("block[0] tool_use_id = %q", repair.Content[0].ToolUseID)
	}
	if repair.Content[1].ToolUseID != "toolu_two" {
		t.Errorf("block[1] tool_use_id = %q", repair.Content[1].ToolUseID)
	}
}

func TestRepairOrphansEmptyDir(t *testing.T) {
	s := NewStore(t.TempDir())

	n, err := s.RepairOrphans()
	if err != nil {
		t.Fatalf("RepairOrphans: %v", err)
	}
	if n != 0 {
		t.Errorf("repaired = %d, want 0", n)
	}
}
