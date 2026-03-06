package session

import (
	"os"
	"testing"
	"time"

	"foci/internal/provider"
)

func TestRepairOrphansDetectsTrailingToolUse(t *testing.T) {
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

func TestInjectRestartMarkersRecentFile(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

	// Create a session (file will have recent mtime)
	s.TestAppend(key, msg("user", "hello"))
	s.TestAppend(key, msg("assistant", "hi"))

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
	if !contains(text, "SYSTEM RESTART") {
		t.Errorf("marker text = %q, want restart marker", text)
	}
}

func TestInjectRestartMarkersOldFile(t *testing.T) {
	s := NewStore(t.TempDir())
	key := "test/imain/1000000000"

	s.TestAppend(key, msg("user", "hello"))

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
	s.TestAppend(recent, msg("user", "hello"))

	// Old session
	old := "test/idaily/1000000000"
	s.TestAppend(old, msg("user", "wake"))
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

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || (len(s) > 0 && findInString(s, substr)))
}

func findInString(s, substr string) bool {
	for i := 0; i < len(s)-len(substr)+1; i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
