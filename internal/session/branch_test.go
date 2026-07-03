package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/provider"
)

func newTestSessionIndex(t *testing.T) *SessionIndex {
	t.Helper()
	idx, err := NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx
}

func TestCreateBranchAndLoadFull(t *testing.T) {
	// Proves that LoadFull on a branch returns the parent messages at the branch point
	// followed by the branch's own messages — in the correct order and count.
	s := NewStore(t.TempDir())
	parentKey := "main/imain"
	branchKey := "main/imain/b1000000001"

	s.TestAppend(parentKey, msg("user", "hello"))
	s.TestAppend(parentKey, msg("assistant", "hi"))
	s.TestAppend(parentKey, msg("user", "how are you"))
	s.TestAppend(parentKey, msg("assistant", "good"))

	if err := s.createBranchFile(parentKey, branchKey, false, ""); err != nil {
		t.Fatalf("createBranchFile: %v", err)
	}

	s.TestAppend(branchKey, msg("user", "branch question"))
	s.TestAppend(branchKey, msg("assistant", "branch answer"))

	msgs, err := s.LoadFull(branchKey)
	if err != nil {
		t.Fatalf("LoadFull: %v", err)
	}
	if len(msgs) != 6 {
		t.Fatalf("len = %d, want 6 (4 parent + 2 branch)", len(msgs))
	}
	if provider.TextOf(msgs[0].Content) != "hello" {
		t.Errorf("msgs[0] = %q", provider.TextOf(msgs[0].Content))
	}
	if provider.TextOf(msgs[3].Content) != "good" {
		t.Errorf("msgs[3] = %q", provider.TextOf(msgs[3].Content))
	}
	if provider.TextOf(msgs[4].Content) != "branch question" {
		t.Errorf("msgs[4] = %q", provider.TextOf(msgs[4].Content))
	}
	if provider.TextOf(msgs[5].Content) != "branch answer" {
		t.Errorf("msgs[5] = %q", provider.TextOf(msgs[5].Content))
	}
}

func TestLoadFullRecoversParentPrefixFromArchive(t *testing.T) {
	// Proves that when a branch's parent root.jsonl has been rotated away to a
	// timestamped archive (e.g. by compaction), LoadFull recovers the parent
	// prefix from the archive instead of silently returning an empty prefix.
	// BranchPoint indexes the pre-rotation list, so the archive is the correct
	// source. (P2-5.)
	s := NewStore(t.TempDir())
	parentKey := "main/imain"
	branchKey := "main/imain/b1000000001"

	s.TestAppend(parentKey, msg("user", "hello"))
	s.TestAppend(parentKey, msg("assistant", "hi"))
	s.TestAppend(parentKey, msg("user", "how are you"))
	s.TestAppend(parentKey, msg("assistant", "good"))

	if err := s.createBranchFile(parentKey, branchKey, false, ""); err != nil {
		t.Fatalf("createBranchFile: %v", err)
	}
	s.TestAppend(branchKey, msg("user", "branch q"))

	// Simulate parent rotation: move the live root.jsonl to a timestamped
	// archive sibling, leaving no live parent file.
	parentPath, err := s.SessionPath(parentKey)
	if err != nil {
		t.Fatalf("SessionPath: %v", err)
	}
	archivePath := nextArchivePath(parentPath)
	if err := os.Rename(parentPath, archivePath); err != nil {
		t.Fatalf("rename parent to archive: %v", err)
	}

	msgs, err := s.LoadFull(branchKey)
	if err != nil {
		t.Fatalf("LoadFull: %v", err)
	}
	// 4 recovered parent + 1 branch. Before the fix this was 1 (prefix lost).
	if len(msgs) != 5 {
		t.Fatalf("len = %d, want 5 (4 recovered parent + 1 branch)", len(msgs))
	}
	if provider.TextOf(msgs[0].Content) != "hello" {
		t.Errorf("msgs[0] = %q, want recovered parent 'hello'", provider.TextOf(msgs[0].Content))
	}
	if provider.TextOf(msgs[4].Content) != "branch q" {
		t.Errorf("msgs[4] = %q, want 'branch q'", provider.TextOf(msgs[4].Content))
	}
}

func TestLoadFullRecoversParentPrefixAfterCompaction(t *testing.T) {
	// Proves the same archive recovery through the REAL compaction path: after
	// the parent is compacted via Replace (same key, file archived in place),
	// the live parent holds fewer messages than the branch point, so LoadFull
	// must recover the branch prefix from the parent's archive. With stable
	// keys, compaction no longer re-keys the parent — the branch's parent_key
	// stays valid and recovery is purely archive-based. (P2-5.)
	s := NewStore(t.TempDir())
	parentKey := "main/c123"

	s.TestAppend(parentKey, msg("user", "hello"))
	s.TestAppend(parentKey, msg("assistant", "hi"))
	s.TestAppend(parentKey, msg("user", "how are you"))
	s.TestAppend(parentKey, msg("assistant", "good"))

	branchKey := "main/c123/b1000000001"
	if err := s.createBranchFile(parentKey, branchKey, false, ""); err != nil {
		t.Fatalf("createBranchFile: %v", err)
	}
	s.TestAppend(branchKey, msg("user", "branch q"))

	// Compact the parent down to a single summary message.
	if err := s.TestReplace(parentKey, []provider.Message{msg("user", "[summary]")}); err != nil {
		t.Fatalf("Replace parent: %v", err)
	}

	msgs, err := s.LoadFull(branchKey)
	if err != nil {
		t.Fatalf("LoadFull: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("len = %d, want 5 (4 recovered parent + 1 branch)", len(msgs))
	}
	if provider.TextOf(msgs[0].Content) != "hello" {
		t.Errorf("msgs[0] = %q, want pre-compaction 'hello'", provider.TextOf(msgs[0].Content))
	}
}

func TestLoadFullRecoversParentPrefixAfterReset(t *testing.T) {
	// Proves archive recovery through the /reset path: after Store.Reset
	// archives the parent's file in place (leaving no live file under the same
	// key), LoadFull on an existing branch recovers the parent prefix from the
	// reset archive instead of returning only the branch's own messages.
	s := NewStore(t.TempDir())
	parentKey := "main/c123"

	s.TestAppend(parentKey, msg("user", "one"))
	s.TestAppend(parentKey, msg("assistant", "two"))

	branchKey := "main/c123/b1000000001"
	if err := s.createBranchFile(parentKey, branchKey, false, ""); err != nil {
		t.Fatalf("createBranchFile: %v", err)
	}
	s.TestAppend(branchKey, msg("user", "branch q"))

	if err := s.Reset(parentKey); err != nil {
		t.Fatalf("Reset parent: %v", err)
	}

	msgs, err := s.LoadFull(branchKey)
	if err != nil {
		t.Fatalf("LoadFull: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("len = %d, want 3 (2 recovered parent + 1 branch)", len(msgs))
	}
	if provider.TextOf(msgs[0].Content) != "one" {
		t.Errorf("msgs[0] = %q, want recovered parent 'one'", provider.TextOf(msgs[0].Content))
	}
	if provider.TextOf(msgs[2].Content) != "branch q" {
		t.Errorf("msgs[2] = %q, want 'branch q'", provider.TextOf(msgs[2].Content))
	}
}

func TestBranchOfBranchMintsSibling(t *testing.T) {
	// Proves that branching from a branch mints a SIBLING key under the same
	// root (the file layout is flat), while branch_meta.parent_key records the
	// true parent — so the logical lineage is preserved in metadata, not the key.
	s := NewStore(t.TempDir())
	rootKey := "main/c123"

	s.TestAppend(rootKey, msg("user", "hello"))

	firstBranch := "main/c123/b1000000001"
	if err := s.createBranchFile(rootKey, firstBranch, false, ""); err != nil {
		t.Fatalf("create first branch: %v", err)
	}
	s.TestAppend(firstBranch, msg("user", "branch work"))

	secondBranch, err := s.CreateBranchWithOptions(firstBranch, BranchOptions{})
	if err != nil {
		t.Fatalf("branch from branch: %v", err)
	}

	sk, err := ParseSessionKey(secondBranch)
	if err != nil {
		t.Fatalf("parse second branch key %q: %v", secondBranch, err)
	}
	if sk.Root().String() != rootKey {
		t.Errorf("second branch root = %q, want %q (sibling under same root)", sk.Root().String(), rootKey)
	}
	if sk.ChildType != 'b' {
		t.Errorf("second branch ChildType = %q, want 'b'", sk.ChildType)
	}
	if secondBranch == firstBranch {
		t.Error("second branch must not collide with first branch key")
	}

	meta, err := s.GetBranchMeta(secondBranch)
	if err != nil {
		t.Fatalf("GetBranchMeta: %v", err)
	}
	if meta == nil {
		t.Fatal("expected branch meta on second branch")
	}
	if meta.ParentKey != firstBranch {
		t.Errorf("parent_key = %q, want true parent %q", meta.ParentKey, firstBranch)
	}
}

func TestBranchParentContinuesGrowing(t *testing.T) {
	// Proves that a branch snapshot is fixed at creation time: messages appended to
	// the parent after branching are not visible when loading the branch.
	s := NewStore(t.TempDir())
	parentKey := "main/imain"
	branchKey := "main/imain/b1000000001"

	s.TestAppend(parentKey, msg("user", "one"))
	s.TestAppend(parentKey, msg("assistant", "two"))

	s.createBranchFile(parentKey, branchKey, false, "")

	s.TestAppend(parentKey, msg("user", "three"))
	s.TestAppend(parentKey, msg("assistant", "four"))
	s.TestAppend(branchKey, msg("user", "branch msg"))

	msgs, _ := s.LoadFull(branchKey)
	if len(msgs) != 3 {
		t.Fatalf("len = %d, want 3 (2 parent + 1 branch)", len(msgs))
	}
	if provider.TextOf(msgs[0].Content) != "one" {
		t.Errorf("msgs[0] = %q", provider.TextOf(msgs[0].Content))
	}
	if provider.TextOf(msgs[1].Content) != "two" {
		t.Errorf("msgs[1] = %q", provider.TextOf(msgs[1].Content))
	}
	if provider.TextOf(msgs[2].Content) != "branch msg" {
		t.Errorf("msgs[2] = %q", provider.TextOf(msgs[2].Content))
	}
}

func TestBranchFromEmptyParent(t *testing.T) {
	// Proves that branching from a parent with no messages works correctly:
	// LoadFull returns only the branch's own messages.
	s := NewStore(t.TempDir())
	parentKey := "main/imain"
	branchKey := "main/imain/b1000000001"

	s.createBranchFile(parentKey, branchKey, false, "")
	s.TestAppend(branchKey, msg("user", "branch only"))

	msgs, err := s.LoadFull(branchKey)
	if err != nil {
		t.Fatalf("LoadFull: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len = %d, want 1", len(msgs))
	}
	if provider.TextOf(msgs[0].Content) != "branch only" {
		t.Errorf("msgs[0] = %q", provider.TextOf(msgs[0].Content))
	}
}

func TestLoadFullNonBranch(t *testing.T) {
	// Proves that LoadFull on a regular (non-branch) session behaves identically
	// to a plain Load — it returns all messages without attempting parent resolution.
	s := NewStore(t.TempDir())
	key := "main/imain"

	s.TestAppend(key, msg("user", "hello"))

	msgs, err := s.LoadFull(key)
	if err != nil {
		t.Fatalf("LoadFull: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len = %d, want 1", len(msgs))
	}
}

func TestLoadFullNonexistent(t *testing.T) {
	// Proves that LoadFull on a key that has never been written returns nil
	// without error, matching the behaviour of a plain Load on a missing file.
	s := NewStore(t.TempDir())
	msgs, err := s.LoadFull("ghost/imain")
	if err != nil {
		t.Fatalf("LoadFull nonexistent: %v", err)
	}
	if msgs != nil {
		t.Errorf("LoadFull nonexistent = %v, want nil", msgs)
	}
}

func TestCreateBranchWithOptionsNoResetHook(t *testing.T) {
	// Proves that the NoResetHook option is persisted in branch metadata.
	s := NewStore(t.TempDir())
	parentKey := "main/imain"

	s.TestAppend(parentKey, msg("user", "hello"))

	branchKey, err := s.CreateBranchWithOptions(parentKey, BranchOptions{NoResetHook: true})
	if err != nil {
		t.Fatalf("CreateBranchWithOptions: %v", err)
	}

	meta, err := s.GetBranchMeta(branchKey)
	if err != nil {
		t.Fatalf("GetBranchMeta: %v", err)
	}
	if meta == nil {
		t.Fatal("expected branch meta, got nil")
	}
	if !meta.NoResetHook {
		t.Error("NoResetHook should be true")
	}
	if meta.BranchPoint != 1 {
		t.Errorf("BranchPoint = %d, want 1", meta.BranchPoint)
	}
}

func TestCreateBranchWithOptionsDefault(t *testing.T) {
	// Proves that omitting BranchOptions leaves NoResetHook as false.
	s := NewStore(t.TempDir())
	parentKey := "main/imain"

	s.TestAppend(parentKey, msg("user", "hello"))

	branchKey, err := s.CreateBranchWithOptions(parentKey, BranchOptions{})
	if err != nil {
		t.Fatalf("CreateBranchWithOptions: %v", err)
	}

	meta, err := s.GetBranchMeta(branchKey)
	if err != nil {
		t.Fatalf("GetBranchMeta: %v", err)
	}
	if meta.NoResetHook {
		t.Error("NoResetHook should default to false")
	}
}

func TestGetBranchMetaRegularSession(t *testing.T) {
	// Proves that GetBranchMeta returns nil for a regular session.
	s := NewStore(t.TempDir())
	key := "main/imain"
	s.TestAppend(key, msg("user", "hello"))

	meta, err := s.GetBranchMeta(key)
	if err != nil {
		t.Fatalf("GetBranchMeta: %v", err)
	}
	if meta != nil {
		t.Errorf("expected nil for regular session, got %+v", meta)
	}
}

func TestGetBranchMetaNonexistent(t *testing.T) {
	// Proves that GetBranchMeta returns nil without error for a nonexistent key.
	s := NewStore(t.TempDir())
	meta, err := s.GetBranchMeta("ghost/imain")
	if err != nil {
		t.Fatalf("GetBranchMeta: %v", err)
	}
	if meta != nil {
		t.Errorf("expected nil for nonexistent session, got %+v", meta)
	}
}

func TestBranchDoesNotContaminateParent(t *testing.T) {
	// Proves that messages appended to a branch are not visible in the parent.
	s := NewStore(t.TempDir())
	parentKey := "main/imain"
	branchKey := "main/imain/b1000000001"

	s.TestAppend(parentKey, msg("user", "parent msg"))
	s.TestAppend(parentKey, msg("assistant", "parent reply"))
	s.createBranchFile(parentKey, branchKey, false, "")

	s.TestAppend(branchKey, msg("user", "branch only"))
	s.TestAppend(branchKey, msg("assistant", "branch reply"))

	parentMsgs, _ := s.Load(parentKey)
	if len(parentMsgs) != 2 {
		t.Errorf("parent has %d messages, want 2", len(parentMsgs))
	}
}

func TestCreateBranchWithOrientationTemplate(t *testing.T) {
	// Proves that orientation template variables are resolved and stored in metadata.
	s := NewStore(t.TempDir())
	parentKey := "main/imain"

	s.TestAppend(parentKey, msg("user", "hello"))
	s.TestAppend(parentKey, msg("assistant", "hi"))

	template := "Type: {branch_type}, key: {branch_key}, parent: {parent_key}."
	branchKey, err := s.CreateBranchWithOptions(parentKey, BranchOptions{
		BranchType:          "test",
		OrientationTemplate: template,
	})
	if err != nil {
		t.Fatalf("CreateBranchWithOptions: %v", err)
	}

	// First call: returns resolved orientation.
	idx := newTestSessionIndex(t)
	got := s.ConsumeOrientation(branchKey, idx)
	if strings.Contains(got, "{branch_key}") {
		t.Errorf("orientation still contains {branch_key}: %q", got)
	}
	if !strings.Contains(got, branchKey) {
		t.Errorf("orientation should contain actual branch key %q, got: %q", branchKey, got)
	}
	if !strings.Contains(got, parentKey) {
		t.Errorf("orientation should contain parent key %q, got: %q", parentKey, got)
	}
	if !strings.Contains(got, "Type: test") {
		t.Errorf("orientation should contain branch type, got: %q", got)
	}

	// Second call: returns "" (consumed flag set in DB).
	got = s.ConsumeOrientation(branchKey, idx)
	if got != "" {
		t.Errorf("ConsumeOrientation second call = %q, want empty", got)
	}

	// Verify the branch meta is unmodified on disk (orientation still present).
	meta, err := s.GetBranchMeta(branchKey)
	if err != nil {
		t.Fatalf("GetBranchMeta after consume: %v", err)
	}
	if meta == nil {
		t.Fatal("branch meta should still exist after orientation consumed")
	}
	if meta.Orientation == "" {
		t.Error("stored orientation should be preserved (consumption tracked in DB, not file)")
	}

	// Without index (nil), consumption is not tracked — returns orientation again.
	got = s.ConsumeOrientation(branchKey, nil)
	if got == "" {
		t.Error("ConsumeOrientation with nil index should return orientation (no tracking)")
	}
}

func TestCreateBranchWithoutOrientation(t *testing.T) {
	// Proves that an empty orientation template produces no orientation text.
	s := NewStore(t.TempDir())
	parentKey := "main/imain"

	s.TestAppend(parentKey, msg("user", "hello"))

	branchKey, err := s.CreateBranchWithOptions(parentKey, BranchOptions{})
	if err != nil {
		t.Fatalf("CreateBranchWithOptions: %v", err)
	}

	msgs, err := s.LoadFull(branchKey)
	if err != nil {
		t.Fatalf("LoadFull: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len = %d, want 1 (1 parent, no orientation)", len(msgs))
	}
}

func TestCreateBranchCollision(t *testing.T) {
	// Proves that createBranchFile rejects a duplicate key rather than
	// silently overwriting. The original branch's metadata is preserved.
	s := NewStore(t.TempDir())
	parentKey := "main/imain"
	branchKey := "main/imain/b1000000001"

	s.TestAppend(parentKey, msg("user", "hello"))

	if err := s.createBranchFile(parentKey, branchKey, false, "first branch orientation"); err != nil {
		t.Fatalf("first createBranchFile: %v", err)
	}

	// Second creation with the same key must fail.
	err := s.createBranchFile(parentKey, branchKey, false, "OVERWRITE ATTEMPT")
	if err == nil {
		t.Fatal("expected error on duplicate branch key, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' error, got: %v", err)
	}

	// Original branch metadata is intact.
	meta, err := s.GetBranchMeta(branchKey)
	if err != nil {
		t.Fatalf("GetBranchMeta: %v", err)
	}
	if meta == nil {
		t.Fatal("branch meta is nil after collision attempt")
	}
	if meta.Orientation != "first branch orientation" {
		t.Errorf("orientation = %q, want %q", meta.Orientation, "first branch orientation")
	}

	// Verify the file on disk was not truncated.
	path, _ := s.SessionPath(branchKey)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat branch file: %v", err)
	}
	if info.Size() == 0 {
		t.Error("branch file was truncated to zero")
	}
}
