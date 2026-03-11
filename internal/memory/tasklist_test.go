package memory

import (
	"path/filepath"
	"testing"
)

func testTaskListStore(t *testing.T) *TaskListStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "tasklist.db")
	s, err := NewTaskListStore(dbPath)
	if err != nil {
		t.Fatalf("NewTaskListStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Verifies basic set and get round-trip with goal and steps preserved.
func TestTaskListSetGet(t *testing.T) {
	s := testTaskListStore(t)

	steps := []TaskStep{
		{Text: "Step one", Status: "pending"},
		{Text: "Step two", Status: "pending"},
	}
	if err := s.Set("test", "Boil an egg", steps); err != nil {
		t.Fatalf("Set: %v", err)
	}

	tl, err := s.Get("test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tl == nil {
		t.Fatal("expected task list, got nil")
	}
	if tl.Goal != "Boil an egg" {
		t.Errorf("goal = %q, want %q", tl.Goal, "Boil an egg")
	}
	if len(tl.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(tl.Steps))
	}
	if tl.Steps[0].Text != "Step one" || tl.Steps[0].Status != "pending" {
		t.Errorf("step[0] = %+v", tl.Steps[0])
	}
	if tl.Updated.IsZero() {
		t.Error("Updated should not be zero")
	}
}

// Verifies Get returns nil (not error) when no task list exists.
func TestTaskListGetMissing(t *testing.T) {
	s := testTaskListStore(t)

	tl, err := s.Get("nonexistent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tl != nil {
		t.Errorf("expected nil, got %+v", tl)
	}
}

// Verifies Set replaces an existing task list (upsert).
func TestTaskListSetReplace(t *testing.T) {
	s := testTaskListStore(t)

	s.Set("test", "First goal", []TaskStep{{Text: "A", Status: "pending"}})
	s.Set("test", "New goal", []TaskStep{
		{Text: "X", Status: "done"},
		{Text: "Y", Status: "pending"},
	})

	tl, err := s.Get("test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tl.Goal != "New goal" {
		t.Errorf("goal = %q, want %q", tl.Goal, "New goal")
	}
	if len(tl.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(tl.Steps))
	}
	if tl.Steps[0].Status != "done" {
		t.Errorf("step[0].Status = %q, want done", tl.Steps[0].Status)
	}
}

// Verifies Clear removes the task list and subsequent Get returns nil.
func TestTaskListClear(t *testing.T) {
	s := testTaskListStore(t)

	s.Set("test", "Goal", []TaskStep{{Text: "A", Status: "pending"}})
	if err := s.Clear("test"); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	tl, err := s.Get("test")
	if err != nil {
		t.Fatalf("Get after clear: %v", err)
	}
	if tl != nil {
		t.Errorf("expected nil after clear, got %+v", tl)
	}
}

// Verifies clearing a nonexistent agent doesn't error.
func TestTaskListClearNonexistent(t *testing.T) {
	s := testTaskListStore(t)

	if err := s.Clear("nonexistent"); err != nil {
		t.Fatalf("Clear nonexistent: %v", err)
	}
}

// Verifies different agents have isolated task lists.
func TestTaskListAgentIsolation(t *testing.T) {
	s := testTaskListStore(t)

	s.Set("agent1", "Goal A", []TaskStep{{Text: "A1", Status: "pending"}})
	s.Set("agent2", "Goal B", []TaskStep{{Text: "B1", Status: "done"}})

	tl1, _ := s.Get("agent1")
	tl2, _ := s.Get("agent2")

	if tl1.Goal != "Goal A" {
		t.Errorf("agent1 goal = %q", tl1.Goal)
	}
	if tl2.Goal != "Goal B" {
		t.Errorf("agent2 goal = %q", tl2.Goal)
	}

	// Clear one doesn't affect the other
	s.Clear("agent1")
	tl1, _ = s.Get("agent1")
	tl2, _ = s.Get("agent2")
	if tl1 != nil {
		t.Error("agent1 should be nil after clear")
	}
	if tl2 == nil {
		t.Error("agent2 should still exist")
	}
}
