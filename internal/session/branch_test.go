package session

import (
	"os"
	"strings"
	"testing"

	"foci/internal/provider"
)

func TestCreateBranchAndLoadFull(t *testing.T) {
	// Proves that LoadFull on a branch returns the parent messages at the branch point
	// followed by the branch's own messages — in the correct order and count.
	s := NewStore(t.TempDir())
	parentKey := "main/imain/1000000000"
	branchKey := "main/imorning/1000000000"

	// Build parent session with 4 messages
	s.TestAppend(parentKey, msg("user", "hello"))
	s.TestAppend(parentKey, msg("assistant", "hi"))
	s.TestAppend(parentKey, msg("user", "how are you"))
	s.TestAppend(parentKey, msg("assistant", "good"))

	// Create branch at current point (4 messages)
	if _, err := s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{}); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}

	// Append branch-only messages
	s.TestAppend(branchKey, msg("user", "branch question"))
	s.TestAppend(branchKey, msg("assistant", "branch answer"))

	// LoadFull should return parent prefix + branch messages
	msgs, err := s.LoadFull(branchKey)
	if err != nil {
		t.Fatalf("LoadFull: %v", err)
	}

	if len(msgs) != 6 {
		t.Fatalf("len = %d, want 6 (4 parent + 2 branch)", len(msgs))
	}

	// First 4 from parent
	if provider.TextOf(msgs[0].Content) != "hello" {
		t.Errorf("msgs[0] = %q", provider.TextOf(msgs[0].Content))
	}
	if provider.TextOf(msgs[3].Content) != "good" {
		t.Errorf("msgs[3] = %q", provider.TextOf(msgs[3].Content))
	}
	// Last 2 from branch
	if provider.TextOf(msgs[4].Content) != "branch question" {
		t.Errorf("msgs[4] = %q", provider.TextOf(msgs[4].Content))
	}
	if provider.TextOf(msgs[5].Content) != "branch answer" {
		t.Errorf("msgs[5] = %q", provider.TextOf(msgs[5].Content))
	}
}

func TestBranchParentContinuesGrowing(t *testing.T) {
	// Proves that a branch snapshot is fixed at creation time: messages appended to
	// the parent after branching are not visible when loading the branch.
	s := NewStore(t.TempDir())
	parentKey := "main/imain/1000000000"
	branchKey := "main/itest/1000000000"

	s.TestAppend(parentKey, msg("user", "one"))
	s.TestAppend(parentKey, msg("assistant", "two"))

	// Branch at 2 messages
	s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{})

	// Parent grows after branching
	s.TestAppend(parentKey, msg("user", "three"))
	s.TestAppend(parentKey, msg("assistant", "four"))

	// Branch should only see the first 2 parent messages
	s.TestAppend(branchKey, msg("user", "branch msg"))

	msgs, _ := s.LoadFull(branchKey)
	if len(msgs) != 3 {
		t.Fatalf("len = %d, want 3 (2 parent + 1 branch)", len(msgs))
	}

	// Should see parent[0:2], not parent[0:4]
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
	parentKey := "main/imain/1000000000"
	branchKey := "main/iempty/1000000000"

	// Don't add any messages to parent — branch from empty
	s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{})
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
	key := "main/imain/1000000000"

	s.TestAppend(key, msg("user", "hello"))

	// LoadFull on a regular session should work like Load
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
	msgs, err := s.LoadFull("ghost/imain/1000000000")
	if err != nil {
		t.Fatalf("LoadFull nonexistent: %v", err)
	}
	if msgs != nil {
		t.Errorf("LoadFull nonexistent = %v, want nil", msgs)
	}
}

func TestCreateBranchWithOptionsNoResetHook(t *testing.T) {
	// Proves that the NoResetHook option is persisted in branch metadata and can
	// be read back via GetBranchMeta, with the correct BranchPoint recorded.
	s := NewStore(t.TempDir())
	parentKey := "main/imain/1000000000"
	branchKey := "main/iopts/1000000000"

	s.TestAppend(parentKey, msg("user", "hello"))

	_, err := s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{NoResetHook: true})
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
	// Proves that omitting BranchOptions leaves NoResetHook as false in the
	// persisted metadata — the safe default for normal branches.
	s := NewStore(t.TempDir())
	parentKey := "main/imain/1000000000"
	branchKey := "main/idefault/1000000000"

	s.TestAppend(parentKey, msg("user", "hello"))

	_, err := s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{})
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
	// Proves that GetBranchMeta returns nil for a regular session (no branch_meta
	// header), allowing callers to use nil as a "not a branch" signal.
	s := NewStore(t.TempDir())
	key := "main/imain/1000000000"

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
	// Proves that GetBranchMeta returns nil without error for a session key
	// that has never been written to disk.
	s := NewStore(t.TempDir())

	meta, err := s.GetBranchMeta("ghost/imain/1000000000")
	if err != nil {
		t.Fatalf("GetBranchMeta: %v", err)
	}
	if meta != nil {
		t.Errorf("expected nil for nonexistent session, got %+v", meta)
	}
}

func TestBranchDoesNotContaminateParent(t *testing.T) {
	// Proves that messages appended to a branch are not visible in the parent
	// session — branches are one-way forks that never write back.
	s := NewStore(t.TempDir())
	parentKey := "main/imain/1000000000"
	branchKey := "main/itest/1000000000"

	s.TestAppend(parentKey, msg("user", "parent msg"))
	s.TestAppend(parentKey, msg("assistant", "parent reply"))
	s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{})

	// Add messages to branch
	s.TestAppend(branchKey, msg("user", "branch only"))
	s.TestAppend(branchKey, msg("assistant", "branch reply"))

	// Parent should still have only 2 messages
	parentMsgs, _ := s.Load(parentKey)
	if len(parentMsgs) != 2 {
		t.Errorf("parent has %d messages, want 2", len(parentMsgs))
	}
}

func TestCreateBranchWithOrientationMessage(t *testing.T) {
	// Proves that orientation text is stored in branch metadata and retrievable
	// via PendingOrientation. LoadFull returns only the parent messages — the
	// orientation is consumed by the agent loop on the first turn, not by LoadFull.
	s := NewStore(t.TempDir())
	parentKey := "main/imain/1000000000"
	branchKey := "main/iorient/1000000000"

	s.TestAppend(parentKey, msg("user", "hello"))
	s.TestAppend(parentKey, msg("assistant", "hi"))

	orientText := "You are a cron branch. Do not message Telegram directly."
	_, err := s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{
		OrientationMessage: orientText,
	})
	if err != nil {
		t.Fatalf("CreateBranchWithOptions: %v", err)
	}

	// LoadFull returns only parent messages (no orientation injected here).
	msgs, err := s.LoadFull(branchKey)
	if err != nil {
		t.Fatalf("LoadFull: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2 (parent messages only)", len(msgs))
	}

	// Orientation is available via PendingOrientation for the first turn.
	got := s.PendingOrientation(branchKey)
	if got != orientText {
		t.Errorf("PendingOrientation = %q, want %q", got, orientText)
	}

	// After appending a message (first turn consumed), orientation is gone.
	s.TestAppend(branchKey, msg("user", "first turn"))
	got = s.PendingOrientation(branchKey)
	if got != "" {
		t.Errorf("PendingOrientation after first turn = %q, want empty", got)
	}
}

func TestCreateBranchWithoutOrientationMessage(t *testing.T) {
	// Proves that an empty orientation message adds no extra messages — the branch
	// starts clean with only the inherited parent messages.
	s := NewStore(t.TempDir())
	parentKey := "main/imain/1000000000"
	branchKey := "main/inoorient/1000000000"

	s.TestAppend(parentKey, msg("user", "hello"))

	_, err := s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{
		OrientationMessage: "", // empty — no orientation
	})
	if err != nil {
		t.Fatalf("CreateBranchWithOptions: %v", err)
	}

	// LoadFull should include only parent msgs, no extra message
	msgs, err := s.LoadFull(branchKey)
	if err != nil {
		t.Fatalf("LoadFull: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len = %d, want 1 (1 parent, no orientation)", len(msgs))
	}
}

func TestCreateBranchCollision(t *testing.T) {
	// Proves that CreateBranchWithOptions rejects a duplicate key rather than
	// silently overwriting the existing branch file. The original branch's
	// metadata must be preserved intact.
	s := NewStore(t.TempDir())
	parentKey := "main/imain/1000000000"
	branchKey := "main/ibranch/1000000000"

	s.TestAppend(parentKey, msg("user", "hello"))

	// First creation succeeds.
	key1, err := s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{
		OrientationMessage: "first branch orientation",
	})
	if err != nil {
		t.Fatalf("first CreateBranchWithOptions: %v", err)
	}
	if key1 != branchKey {
		t.Errorf("first key = %q, want %q", key1, branchKey)
	}

	// Second creation with the same key must fail (collision detected).
	// Use createBranchFile directly to avoid the retry+sleep loop.
	err = s.createBranchFile(parentKey, branchKey, BranchOptions{
		OrientationMessage: "OVERWRITE ATTEMPT",
	})
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
