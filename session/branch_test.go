package session

import (
	"testing"

	"foci/provider"
)

func TestCreateBranchAndLoadFull(t *testing.T) {
	s := NewStore(t.TempDir())
	parentKey := "main/imain/1000000000"
	branchKey := "main/imorning/1000000000"

	// Build parent session with 4 messages
	s.Append(parentKey, msg("user", "hello"))
	s.Append(parentKey, msg("assistant", "hi"))
	s.Append(parentKey, msg("user", "how are you"))
	s.Append(parentKey, msg("assistant", "good"))

	// Create branch at current point (4 messages)
	if err := s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{}); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}

	// Append branch-only messages
	s.Append(branchKey, msg("user", "branch question"))
	s.Append(branchKey, msg("assistant", "branch answer"))

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
	s := NewStore(t.TempDir())
	parentKey := "main/imain/1000000000"
	branchKey := "main/itest/1000000000"

	s.Append(parentKey, msg("user", "one"))
	s.Append(parentKey, msg("assistant", "two"))

	// Branch at 2 messages
	s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{})

	// Parent grows after branching
	s.Append(parentKey, msg("user", "three"))
	s.Append(parentKey, msg("assistant", "four"))

	// Branch should only see the first 2 parent messages
	s.Append(branchKey, msg("user", "branch msg"))

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
	s := NewStore(t.TempDir())
	parentKey := "main/imain/1000000000"
	branchKey := "main/iempty/1000000000"

	// Don't add any messages to parent — branch from empty
	s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{})
	s.Append(branchKey, msg("user", "branch only"))

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
	s := NewStore(t.TempDir())
	key := "main/imain/1000000000"

	s.Append(key, msg("user", "hello"))

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
	s := NewStore(t.TempDir())
	parentKey := "main/imain/1000000000"
	branchKey := "main/iopts/1000000000"

	s.Append(parentKey, msg("user", "hello"))

	err := s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{NoResetHook: true})
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
	s := NewStore(t.TempDir())
	parentKey := "main/imain/1000000000"
	branchKey := "main/idefault/1000000000"

	s.Append(parentKey, msg("user", "hello"))

	err := s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{})
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
	s := NewStore(t.TempDir())
	key := "main/imain/1000000000"

	s.Append(key, msg("user", "hello"))

	meta, err := s.GetBranchMeta(key)
	if err != nil {
		t.Fatalf("GetBranchMeta: %v", err)
	}
	if meta != nil {
		t.Errorf("expected nil for regular session, got %+v", meta)
	}
}

func TestGetBranchMetaNonexistent(t *testing.T) {
	s := NewStore(t.TempDir())

	meta, err := s.GetBranchMeta("ghost/imain/1000000000")
	if err != nil {
		t.Fatalf("GetBranchMeta: %v", err)
	}
	if meta != nil {
		t.Errorf("expected nil for nonexistent session, got %+v", meta)
	}
}

func TestBranchMetaBackwardCompat(t *testing.T) {
	s := NewStore(t.TempDir())
	parentKey := "main/imain/1000000000"
	branchKey := "main/iold/1000000000"

	s.Append(parentKey, msg("user", "hello"))

	// CreateBranch (old method) doesn't set NoResetHook
	if err := s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{}); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}

	meta, err := s.GetBranchMeta(branchKey)
	if err != nil {
		t.Fatalf("GetBranchMeta: %v", err)
	}
	if meta == nil {
		t.Fatal("expected branch meta")
	}
	if meta.NoResetHook {
		t.Error("NoResetHook should be false for old-style branches")
	}
}

func TestBranchDoesNotContaminateParent(t *testing.T) {
	s := NewStore(t.TempDir())
	parentKey := "main/imain/1000000000"
	branchKey := "main/itest/1000000000"

	s.Append(parentKey, msg("user", "parent msg"))
	s.Append(parentKey, msg("assistant", "parent reply"))
	s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{})

	// Add messages to branch
	s.Append(branchKey, msg("user", "branch only"))
	s.Append(branchKey, msg("assistant", "branch reply"))

	// Parent should still have only 2 messages
	parentMsgs, _ := s.Load(parentKey)
	if len(parentMsgs) != 2 {
		t.Errorf("parent has %d messages, want 2", len(parentMsgs))
	}
}

func TestCreateBranchWithOrientationMessage(t *testing.T) {
	s := NewStore(t.TempDir())
	parentKey := "main/imain/1000000000"
	branchKey := "main/iorient/1000000000"

	s.Append(parentKey, msg("user", "hello"))
	s.Append(parentKey, msg("assistant", "hi"))

	orientText := "You are a cron branch. Do not message Telegram directly."
	err := s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{
		OrientationMessage: orientText,
	})
	if err != nil {
		t.Fatalf("CreateBranchWithOptions: %v", err)
	}

	// LoadFull should include parent msgs + orientation message
	msgs, err := s.LoadFull(branchKey)
	if err != nil {
		t.Fatalf("LoadFull: %v", err)
	}

	// 2 parent + 1 orientation
	if len(msgs) != 3 {
		t.Fatalf("len = %d, want 3 (2 parent + 1 orientation)", len(msgs))
	}

	// Orientation should be the first branch message (index 2)
	if msgs[2].Role != "user" {
		t.Errorf("orientation role = %q, want user", msgs[2].Role)
	}
	if provider.TextOf(msgs[2].Content) != orientText {
		t.Errorf("orientation text = %q, want %q", provider.TextOf(msgs[2].Content), orientText)
	}
}

func TestCreateBranchWithoutOrientationMessage(t *testing.T) {
	s := NewStore(t.TempDir())
	parentKey := "main/imain/1000000000"
	branchKey := "main/inoorient/1000000000"

	s.Append(parentKey, msg("user", "hello"))

	err := s.CreateBranchWithOptions(parentKey, branchKey, BranchOptions{
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
