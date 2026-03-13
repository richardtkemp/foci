package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/provider"
)

func TestReplaceBranchPreservesMeta(t *testing.T) {
	// Proves that Replace on a branch file preserves branch_meta (parent key and
	// NoResetHook flag) in the new file, resets BranchPoint to 0, and that
	// LoadFull after compaction returns only the compacted messages.
	s := NewStore(t.TempDir())
	parentKey := "test/c123/1000000000"
	branchKey := "test/iwake-999/1000000000"

	// Build parent with 4 messages
	s.TestAppend(parentKey, msg("user", "parent1"))
	s.TestAppend(parentKey, msg("assistant", "parent2"))
	s.TestAppend(parentKey, msg("user", "parent3"))
	s.TestAppend(parentKey, msg("assistant", "parent4"))

	// Create branch at point 4
	s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{NoResetHook: true})

	// Add branch messages
	s.TestAppend(branchKey, msg("user", "branch q"))
	s.TestAppend(branchKey, msg("assistant", "branch a"))

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
	if err := s.TestReplace(branchKey, compacted); err != nil {
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
	// Proves that Replace archives the old session file before writing new content:
	// the current file holds only the compacted messages while an archive file
	// retains the original messages.
	dir := t.TempDir()
	s := NewStore(dir)
	key := "test/c999/1000000000"

	// Write initial messages
	s.TestAppend(key, msg("user", "old1"))
	s.TestAppend(key, msg("assistant", "old2"))
	s.TestAppend(key, msg("user", "old3"))

	// Replace (simulating compaction)
	compacted := []provider.Message{
		msg("user", "summary"),
		msg("assistant", "acknowledged"),
	}
	if err := s.TestReplace(key, compacted); err != nil {
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
	// Proves that each successive Replace creates a new archive file with a unique
	// timestamp suffix, accumulating N archives for N replacements while the current
	// file always holds only the latest compacted content.
	dir := t.TempDir()
	s := NewStore(dir)
	key := "test/c888/1000000000"

	for round := 1; round <= 3; round++ {
		s.TestAppend(key, msg("user", fmt.Sprintf("round %d", round)))
		s.TestAppend(key, msg("assistant", fmt.Sprintf("reply %d", round)))

		compacted := []provider.Message{
			msg("user", fmt.Sprintf("summary %d", round)),
		}
		if err := s.TestReplace(key, compacted); err != nil {
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
	// Proves that when a branch session is compacted via Replace, the archive file
	// preserves branch_meta as its first line with the original BranchPoint, and
	// the new file's branch_meta has BranchPoint reset to 0.
	dir := t.TempDir()
	s := NewStore(dir)
	parentKey := "test/c777/1000000000"
	branchKey := "test/iwake-111/1000000000"

	s.TestAppend(parentKey, msg("user", "parent"))
	s.TestAppend(parentKey, msg("assistant", "reply"))
	s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{})
	s.TestAppend(branchKey, msg("user", "branch q"))
	s.TestAppend(branchKey, msg("assistant", "branch a"))

	compacted := []provider.Message{
		msg("user", "[compacted]"),
		msg("assistant", "summary"),
	}
	if err := s.TestReplace(branchKey, compacted); err != nil {
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
