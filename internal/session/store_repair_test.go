package session

import (
	"testing"
)

func TestRepairOrphansDetectsTrailingToolUse(t *testing.T) {
	// Proves that RepairOrphans detects a session ending with an unanswered
	// tool_use message and appends a synthetic tool_result + assistant ack,
	// maintaining role alternation for the next user message.
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

	s.TestAppend(key, msg("user", "hello"))
	s.TestAppend(key, toolUseMsg("toolu_123"))

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
	if len(msgs) != 4 {
		t.Fatalf("len = %d, want 4 (user, assistant, user(repair), assistant(ack))", len(msgs))
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
	if block.Content != "Tool call interrupted" {
		t.Errorf("content = %q", block.Content)
	}

	ack := msgs[3]
	if ack.Role != "assistant" {
		t.Errorf("ack role = %q, want assistant", ack.Role)
	}
}

func TestRepairOrphansNoOpWhenClean(t *testing.T) {
	// Proves that RepairOrphans leaves a structurally sound session untouched
	// and reports zero repaired sessions.
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

	s.TestAppend(key, msg("user", "hello"))
	s.TestAppend(key, msg("assistant", "hi"))
	s.TestAppend(key, msg("user", "bye"))

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
	// Proves that RepairOrphans correctly repairs only the broken sessions across
	// multiple sessions in the store, leaving clean sessions unchanged.
	s := NewStore(t.TempDir())

	// Broken session
	broken := "test/imain/1000000000"
	s.TestAppend(broken, msg("user", "hello"))
	s.TestAppend(broken, toolUseMsg("toolu_aaa"))

	// Clean session
	clean := "test/idaily/1000000000"
	s.TestAppend(clean, msg("user", "wake"))
	s.TestAppend(clean, msg("assistant", "done"))

	n, err := s.RepairOrphans()
	if err != nil {
		t.Fatalf("RepairOrphans: %v", err)
	}
	if n != 1 {
		t.Errorf("repaired = %d, want 1", n)
	}

	// Broken should be repaired (user, assistant(tool_use), user(repair), assistant(ack))
	msgs, _ := s.Load(broken)
	if len(msgs) != 4 {
		t.Errorf("broken len = %d, want 4", len(msgs))
	}

	// Clean should be unchanged
	msgs, _ = s.Load(clean)
	if len(msgs) != 2 {
		t.Errorf("clean len = %d, want 2", len(msgs))
	}
}

func TestRepairOrphansMultipleToolUse(t *testing.T) {
	// Proves that when a trailing assistant message contains multiple tool_use
	// blocks, RepairOrphans injects a single repair message with one tool_result
	// per block, preserving each tool_use ID.
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

	s.TestAppend(key, msg("user", "do things"))
	s.TestAppend(key, toolUseMsg("toolu_one", "toolu_two"))

	n, err := s.RepairOrphans()
	if err != nil {
		t.Fatalf("RepairOrphans: %v", err)
	}
	if n != 1 {
		t.Fatalf("repaired = %d, want 1", n)
	}

	msgs, _ := s.Load(key)
	if len(msgs) != 4 {
		t.Fatalf("len = %d, want 4", len(msgs))
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
	if msgs[3].Role != "assistant" {
		t.Errorf("ack role = %q, want assistant", msgs[3].Role)
	}
}

func TestRepairOrphansEmptyDir(t *testing.T) {
	// Proves that RepairOrphans is a no-op on an empty store and returns no error.
	s := NewStore(t.TempDir())

	n, err := s.RepairOrphans()
	if err != nil {
		t.Fatalf("RepairOrphans: %v", err)
	}
	if n != 0 {
		t.Errorf("repaired = %d, want 0", n)
	}
}

