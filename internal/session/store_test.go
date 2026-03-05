package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/provider"
)

func msg(role, text string) provider.Message {
	return provider.Message{
		Role:    role,
		Content: provider.TextContent(text),
	}
}

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
	key := "test/imain/1000000000"

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
	if err := s.Clear("ghost/imain/1000000000"); err != nil {
		t.Fatalf("Clear nonexistent: %v", err)
	}
}

func TestReplace(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

	// Write initial messages
	s.Append(key, msg("user", "old1"))
	s.Append(key, msg("assistant", "old2"))
	s.Append(key, msg("user", "old3"))

	// Replace
	replacement := []provider.Message{
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

	s.Append(key, msg("user", "a"))
	s.Append(key, msg("assistant", "b"))

	n, _ = s.MessageCount(key)
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}

func TestLoadFullRegularSession(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

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
	key := "mybot/idaily/1000000000"
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
	key := "test/imain/1000000000"

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
	key := "test/imain/1000000000"

	// Create session
	s.Append(key, msg("user", "hello"))

	originalCreatedAt := s.CreatedAt(key)
	if originalCreatedAt == "n/a" {
		t.Fatal("expected creation time after append")
	}

	// Replace (simulating compaction)
	replacement := []provider.Message{
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
	key := "test/imain/1000000000"

	s.Append(key, msg("user", "hello"))

	// Verify session_meta is written by reading raw file
	path, _ := s.SessionPath(key)
	data, err := os.ReadFile(path)
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
	key := "test/imain/1000000000"

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
	key := "test/imain/1000000000"

	s := NewStore(dir)
	s.Append(key, msg("user", "hello"))
	originalCreatedAt := s.CreatedAt(key)
	if originalCreatedAt == "n/a" {
		t.Fatal("expected creation time after append")
	}

	// Modify file mtime (simulating external modification)
	path, _ := s.SessionPath(key)
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

func toolUseMsg(ids ...string) provider.Message {
	var blocks []provider.ContentBlock
	for _, id := range ids {
		blocks = append(blocks, provider.ContentBlock{
			Type:  "tool_use",
			ID:    id,
			Name:  "shell",
			Input: []byte(`{"command":"ls"}`),
		})
	}
	return provider.Message{Role: "assistant", Content: blocks}
}

func TestRepairOrphansDetectsTrailingToolUse(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

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
	key := "test/imain/1000000000"

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
	broken := "test/imain/1000000000"
	s.Append(broken, msg("user", "hello"))
	s.Append(broken, toolUseMsg("toolu_aaa"))

	// Clean session
	clean := "test/idaily/1000000000"
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
	key := "test/imain/1000000000"

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

// --- InjectRestartMarkers tests ---

func TestInjectRestartMarkersRecentFile(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

	// Create a session (file will have recent mtime)
	s.Append(key, msg("user", "hello"))
	s.Append(key, msg("assistant", "hi"))

	n, err := s.InjectRestartMarkers(1 * time.Hour)
	if err != nil {
		t.Fatalf("InjectRestartMarkers: %v", err)
	}
	if n != 1 {
		t.Fatalf("marked = %d, want 1", n)
	}

	msgs, err := s.Load(key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("len = %d, want 3", len(msgs))
	}

	marker := msgs[2]
	if marker.Role != "user" {
		t.Errorf("marker role = %q, want user", marker.Role)
	}
	text := provider.TextOf(marker.Content)
	if !strings.Contains(text, "SYSTEM RESTART") {
		t.Errorf("marker text = %q, want restart marker", text)
	}
}

func TestInjectRestartMarkersOldFile(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

	s.Append(key, msg("user", "hello"))

	// Set mtime to 2 hours ago
	path, _ := s.SessionPath(key)
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	n, err := s.InjectRestartMarkers(1 * time.Hour)
	if err != nil {
		t.Fatalf("InjectRestartMarkers: %v", err)
	}
	if n != 0 {
		t.Errorf("marked = %d, want 0 (file too old)", n)
	}

	msgs, _ := s.Load(key)
	if len(msgs) != 1 {
		t.Errorf("len = %d, want 1 (unchanged)", len(msgs))
	}
}

func TestInjectRestartMarkersEmptyDir(t *testing.T) {
	s := NewStore(t.TempDir())

	n, err := s.InjectRestartMarkers(1 * time.Hour)
	if err != nil {
		t.Fatalf("InjectRestartMarkers: %v", err)
	}
	if n != 0 {
		t.Errorf("marked = %d, want 0", n)
	}
}

func TestInjectRestartMarkersMultipleSessions(t *testing.T) {
	s := NewStore(t.TempDir())

	// Recent session
	recent := "test/imain/1000000000"
	s.Append(recent, msg("user", "hello"))

	// Old session
	old := "test/idaily/1000000000"
	s.Append(old, msg("user", "wake"))
	oldPath, _ := s.SessionPath(old)
	oldTime := time.Now().Add(-2 * time.Hour)
	os.Chtimes(oldPath, oldTime, oldTime)

	n, err := s.InjectRestartMarkers(1 * time.Hour)
	if err != nil {
		t.Fatalf("InjectRestartMarkers: %v", err)
	}
	if n != 1 {
		t.Errorf("marked = %d, want 1 (only recent)", n)
	}

	// Recent should have marker
	msgs, _ := s.Load(recent)
	if len(msgs) != 2 {
		t.Errorf("recent len = %d, want 2", len(msgs))
	}

	// Old should be unchanged
	msgs, _ = s.Load(old)
	if len(msgs) != 1 {
		t.Errorf("old len = %d, want 1", len(msgs))
	}
}

func TestReplaceBranchPreservesMeta(t *testing.T) {
	s := NewStore(t.TempDir())
	parentKey := "test/c123/1000000000"
	branchKey := "test/iwake-999/1000000000"

	// Build parent with 4 messages
	s.Append(parentKey, msg("user", "parent1"))
	s.Append(parentKey, msg("assistant", "parent2"))
	s.Append(parentKey, msg("user", "parent3"))
	s.Append(parentKey, msg("assistant", "parent4"))

	// Create branch at point 4
	s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{NoResetHook: true})

	// Add branch messages
	s.Append(branchKey, msg("user", "branch q"))
	s.Append(branchKey, msg("assistant", "branch a"))

	// Verify branch_meta before Replace
	meta, err := s.GetBranchMeta(branchKey)
	if err != nil {
		t.Fatalf("GetBranchMeta before: %v", err)
	}
	if meta == nil {
		t.Fatal("expected branch_meta before Replace")
	}
	if meta.BranchPoint != 4 {
		t.Errorf("BranchPoint before = %d, want 4", meta.BranchPoint)
	}

	// Replace (simulating compaction)
	compacted := []provider.Message{
		msg("user", "[Session compacted]"),
		msg("assistant", "summary of parent + branch"),
	}
	if err := s.Replace(branchKey, compacted); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	// branch_meta should be preserved with BranchPoint=0
	meta, err = s.GetBranchMeta(branchKey)
	if err != nil {
		t.Fatalf("GetBranchMeta after: %v", err)
	}
	if meta == nil {
		t.Fatal("branch_meta lost after Replace")
	}
	if meta.ParentKey != parentKey {
		t.Errorf("ParentKey = %q, want %q", meta.ParentKey, parentKey)
	}
	if meta.BranchPoint != 0 {
		t.Errorf("BranchPoint after = %d, want 0", meta.BranchPoint)
	}
	if !meta.NoResetHook {
		t.Error("NoResetHook should be preserved")
	}

	// LoadFull after compaction: parent[:0] + own = just compacted messages
	msgs, err := s.LoadFull(branchKey)
	if err != nil {
		t.Fatalf("LoadFull after Replace: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("LoadFull len = %d, want 2", len(msgs))
	}
	if provider.TextOf(msgs[0].Content) != "[Session compacted]" {
		t.Errorf("msgs[0] = %q", provider.TextOf(msgs[0].Content))
	}

	// Parent should be unaffected
	parentMsgs, _ := s.Load(parentKey)
	if len(parentMsgs) != 4 {
		t.Errorf("parent has %d messages, want 4", len(parentMsgs))
	}
}

func TestReplaceRotatesFile(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	key := "test/c999/1000000000"

	// Write initial messages
	s.Append(key, msg("user", "old1"))
	s.Append(key, msg("assistant", "old2"))
	s.Append(key, msg("user", "old3"))

	// Replace (simulating compaction)
	compacted := []provider.Message{
		msg("user", "summary"),
		msg("assistant", "acknowledged"),
	}
	if err := s.Replace(key, compacted); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	// Current file should have compacted messages
	msgs, _ := s.Load(key)
	if len(msgs) != 2 {
		t.Fatalf("current len = %d, want 2", len(msgs))
	}
	if provider.TextOf(msgs[0].Content) != "summary" {
		t.Errorf("current msgs[0] = %q", provider.TextOf(msgs[0].Content))
	}

	// Archive file should exist with old messages - check for timestamp pattern
	chatDir := filepath.Join(dir, "test", "c999", "1000000000")
	entries, err := os.ReadDir(chatDir)
	if err != nil {
		t.Fatalf("read chat dir: %v", err)
	}
	var archivePath string
	for _, e := range entries {
		if isArchiveFile(e.Name()) && strings.HasPrefix(e.Name(), "root.") {
			archivePath = filepath.Join(chatDir, e.Name())
			break
		}
	}
	if archivePath == "" {
		t.Fatal("archive file not found")
	}

	// Read archive to verify old messages are preserved
	archiveData, _ := os.ReadFile(archivePath)
	lines := strings.Split(strings.TrimSpace(string(archiveData)), "\n")
	// session_meta + 3 messages = 4 lines
	if len(lines) != 4 {
		t.Errorf("archive lines = %d, want 4 (meta + 3 messages)", len(lines))
	}
}

func TestReplaceMultipleRotations(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	key := "test/c888/1000000000"

	for round := 1; round <= 3; round++ {
		s.Append(key, msg("user", fmt.Sprintf("round %d", round)))
		s.Append(key, msg("assistant", fmt.Sprintf("reply %d", round)))

		compacted := []provider.Message{
			msg("user", fmt.Sprintf("summary %d", round)),
		}
		if err := s.Replace(key, compacted); err != nil {
			t.Fatalf("Replace round %d: %v", round, err)
		}
	}

	// Should have 3 archive files with timestamp suffixes
	chatDir := filepath.Join(dir, "test", "c888", "1000000000")
	entries, err := os.ReadDir(chatDir)
	if err != nil {
		t.Fatalf("read chat dir: %v", err)
	}
	var archiveCount int
	for _, e := range entries {
		if isArchiveFile(e.Name()) && strings.HasPrefix(e.Name(), "root.") {
			archiveCount++
		}
	}
	if archiveCount != 3 {
		t.Errorf("expected 3 archives, found %d", archiveCount)
	}

	// Current file should have latest compacted messages
	msgs, _ := s.Load(key)
	if len(msgs) != 1 {
		t.Fatalf("current len = %d, want 1", len(msgs))
	}
	if provider.TextOf(msgs[0].Content) != "summary 3" {
		t.Errorf("current = %q, want %q", provider.TextOf(msgs[0].Content), "summary 3")
	}
}

func TestReplaceBranchRotation(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	parentKey := "test/c777/1000000000"
	branchKey := "test/iwake-111/1000000000"

	s.Append(parentKey, msg("user", "parent"))
	s.Append(parentKey, msg("assistant", "reply"))
	s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{})
	s.Append(branchKey, msg("user", "branch q"))
	s.Append(branchKey, msg("assistant", "branch a"))

	compacted := []provider.Message{
		msg("user", "[compacted]"),
		msg("assistant", "summary"),
	}
	if err := s.Replace(branchKey, compacted); err != nil {
		t.Fatalf("Replace branch: %v", err)
	}

	// Archive should exist - check for timestamp pattern
	cronDir := filepath.Join(dir, "test", "iwake-111", "1000000000")
	entries, err := os.ReadDir(cronDir)
	if err != nil {
		t.Fatalf("read cron dir: %v", err)
	}
	var archivePath string
	for _, e := range entries {
		if isArchiveFile(e.Name()) && strings.HasPrefix(e.Name(), "root.") {
			archivePath = filepath.Join(cronDir, e.Name())
			break
		}
	}
	if archivePath == "" {
		t.Fatal("branch archive not found")
	}

	// Archive should have branch_meta as first line
	archiveData, _ := os.ReadFile(archivePath)
	firstLine := strings.SplitN(string(archiveData), "\n", 2)[0]
	var meta BranchMeta
	if err := json.Unmarshal([]byte(firstLine), &meta); err != nil {
		t.Fatalf("archive branch_meta unmarshal: %v", err)
	}
	if meta.Type != "branch_meta" {
		t.Errorf("archive first line type = %q, want branch_meta", meta.Type)
	}
	if meta.BranchPoint != 2 {
		t.Errorf("archive branch_point = %d, want 2 (original)", meta.BranchPoint)
	}

	// New file should have branch_meta with branch_point=0
	newMeta, _ := s.GetBranchMeta(branchKey)
	if newMeta == nil {
		t.Fatal("new file missing branch_meta")
	}
	if newMeta.BranchPoint != 0 {
		t.Errorf("new branch_point = %d, want 0", newMeta.BranchPoint)
	}
}

func TestListChatSessionsSkipsArchives(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	// Create a real chat session
	key := "test/c555/1000000000"
	s.Append(key, msg("user", "hello"))

	// Simulate an archive file by creating it directly (using timestamp pattern)
	timestamp := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	archiveDir := filepath.Join(dir, "test", "c555", "1000000000")
	os.MkdirAll(archiveDir, 0755)
	archivePath := filepath.Join(archiveDir, fmt.Sprintf("root.%s.jsonl", timestamp))
	os.WriteFile(archivePath, []byte(`{"role":"user","content":[{"type":"text","text":"old"}]}`+"\n"), 0644)

	sessions, err := s.ListChatSessions("test")
	if err != nil {
		t.Fatalf("ListChatSessions: %v", err)
	}

	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1 (archive should be skipped)", len(sessions))
	}
	if sessions[0].ChatID != 555 {
		t.Errorf("ChatID = %d, want 555", sessions[0].ChatID)
	}
}

func TestRepairOrphansSkipsArchives(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	// Create a session with an orphaned tool_use
	key := "test/c444/1000000000"
	s.Append(key, msg("user", "hello"))
	s.Append(key, provider.Message{
		Role: "assistant",
		Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "tool_1", Name: "exec", Input: json.RawMessage(`{}`)},
		},
	})

	// Create an archive file with the same orphaned pattern (using timestamp pattern)
	archiveDir := filepath.Join(dir, "test", "c444", "1000000000")
	os.MkdirAll(archiveDir, 0755)
	timestamp := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	archiveData := `{"role":"user","content":[{"type":"text","text":"old"}]}` + "\n" +
		`{"role":"assistant","content":[{"type":"tool_use","id":"tool_2","name":"shell","input":{}}]}` + "\n"
	os.WriteFile(filepath.Join(archiveDir, fmt.Sprintf("root.%s.jsonl", timestamp)), []byte(archiveData), 0644)

	repaired, err := s.RepairOrphans()
	if err != nil {
		t.Fatalf("RepairOrphans: %v", err)
	}

	// Should only repair the current file, not the archive
	if repaired != 1 {
		t.Errorf("repaired = %d, want 1 (archive should be skipped)", repaired)
	}
}

func TestReplaceNonexistentFile(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/c333/1000000000"

	// Replace on a key with no existing file should work (no rotation needed)
	compacted := []provider.Message{
		msg("user", "fresh"),
	}
	if err := s.Replace(key, compacted); err != nil {
		t.Fatalf("Replace nonexistent: %v", err)
	}

	msgs, _ := s.Load(key)
	if len(msgs) != 1 {
		t.Fatalf("len = %d, want 1", len(msgs))
	}
}

func TestIsArchiveFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"5970082313.jsonl", false},
		{"5970082313.1.jsonl", true},                                    // old numbered pattern
		{"5970082313.2.jsonl", true},                                    // old numbered pattern
		{"5970082313.10.jsonl", true},                                   // old numbered pattern
		{"5970082313.2026-03-04T02-30-00Z.jsonl", true},                 // new timestamp pattern
		{"5970082313.2026-03-04T02-30-00Z.2.jsonl", true},               // new timestamp pattern with counter
		{"5970082313.2026-03-04T02-30-00Z.10.jsonl", true},              // new timestamp pattern with counter
		{"wake-111.2026-12-25T14-35-22Z.jsonl", true},                   // new timestamp pattern
		{"wake-111.2026-12-25T14-35-22Z.3.jsonl", true},                 // new timestamp pattern with counter
		{"main.jsonl", false},
		{"wake-111.jsonl", false},
		{"wake-111.1.jsonl", true},                                      // old numbered pattern
		{"invalid.2026-03-04.jsonl", false},                             // invalid timestamp (missing time)
		{"invalid.2026-03-04T02-30-00.jsonl", false},                    // invalid timestamp (missing Z)
		{"invalid.abc.jsonl", false},                                    // invalid suffix
		{"invalid.2026-03-04T02-30-00Z.abc.jsonl", false},               // timestamp with invalid counter
	}
	for _, tt := range tests {
		if got := isArchiveFile(tt.name); got != tt.want {
			t.Errorf("isArchiveFile(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestLastActivity tests the LastActivity function
func TestLastActivity(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/c123/1000000000"

	// Write a message to create the file
	s.Append(key, msg("user", "test message"))

	// Get the last activity time
	lastActivity := s.LastActivity(key)

	// Should be a valid RFC3339 formatted timestamp
	if lastActivity == "n/a" {
		t.Error("LastActivity should return a timestamp, not n/a")
	}
	if len(lastActivity) < 19 {
		t.Errorf("LastActivity = %q, doesn't look like RFC3339 format", lastActivity)
	}
}

// TestLastActivity_Missing tests LastActivity with a non-existent session
func TestLastActivity_Missing(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/c999/1000000000"

	// Try to get activity for non-existent session
	lastActivity := s.LastActivity(key)

	// Should return "n/a"
	if lastActivity != "n/a" {
		t.Errorf("LastActivity for missing session = %q, want 'n/a'", lastActivity)
	}
}

// TestLastActivity_InvalidKey tests LastActivity with an invalid key
func TestLastActivity_InvalidKey(t *testing.T) {
	s := NewStore(t.TempDir())

	// Try with invalid key (missing parts)
	lastActivity := s.LastActivity("invalid")

	// Should return "n/a" due to SessionPath error
	if lastActivity != "n/a" {
		t.Errorf("LastActivity with invalid key = %q, want 'n/a'", lastActivity)
	}
}
