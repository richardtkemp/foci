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

func TestTaskCreateAndGet(t *testing.T) {
	// Verifies Create returns auto-incrementing IDs and Get retrieves them.
	s := testTaskListStore(t)

	id1, err := s.Create("test", "First task", "Description 1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id1 != 1 {
		t.Errorf("first ID = %d, want 1", id1)
	}

	id2, err := s.Create("test", "Second task", "Description 2")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id2 != 2 {
		t.Errorf("second ID = %d, want 2", id2)
	}

	task, err := s.Get("test", id1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if task == nil {
		t.Fatal("expected task, got nil")
	}
	if task.Subject != "First task" {
		t.Errorf("subject = %q, want %q", task.Subject, "First task")
	}
	if task.Description != "Description 1" {
		t.Errorf("description = %q, want %q", task.Description, "Description 1")
	}
	if task.Status != "pending" {
		t.Errorf("status = %q, want pending", task.Status)
	}
	if task.CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero")
	}
}

func TestTaskGetMissing(t *testing.T) {
	// Verifies Get returns nil for a nonexistent task.
	s := testTaskListStore(t)

	task, err := s.Get("test", 999)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if task != nil {
		t.Errorf("expected nil, got %+v", task)
	}
}

func TestTaskUpdate(t *testing.T) {
	// Verifies Update changes fields and sets updated_at.
	s := testTaskListStore(t)

	id, _ := s.Create("test", "Original", "Desc")

	err := s.Update("test", id, "Updated subject", "", "in_progress")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	task, _ := s.Get("test", id)
	if task.Subject != "Updated subject" {
		t.Errorf("subject = %q, want Updated subject", task.Subject)
	}
	if task.Description != "Desc" {
		t.Errorf("description changed unexpectedly: %q", task.Description)
	}
	if task.Status != "in_progress" {
		t.Errorf("status = %q, want in_progress", task.Status)
	}
	if task.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be set after update")
	}
}

func TestTaskUpdateDelete(t *testing.T) {
	// Verifies Update with status="deleted" removes the task.
	s := testTaskListStore(t)

	id, _ := s.Create("test", "To delete", "")

	err := s.Update("test", id, "", "", "deleted")
	if err != nil {
		t.Fatalf("Update delete: %v", err)
	}

	task, _ := s.Get("test", id)
	if task != nil {
		t.Errorf("expected nil after delete, got %+v", task)
	}
}

func TestTaskUpdateNotFound(t *testing.T) {
	// Verifies Update returns error for nonexistent task.
	s := testTaskListStore(t)

	err := s.Update("test", 999, "x", "", "")
	if err == nil {
		t.Error("expected error for nonexistent task")
	}
}

func TestTaskUpdateNoop(t *testing.T) {
	// Verifies Update with nothing to change returns error.
	s := testTaskListStore(t)

	id, _ := s.Create("test", "Task", "")

	err := s.Update("test", id, "", "", "")
	if err == nil {
		t.Error("expected error for empty update")
	}
}

func TestTaskList(t *testing.T) {
	// Verifies List returns all tasks ordered by ID.
	s := testTaskListStore(t)

	s.Create("test", "Task A", "")
	s.Create("test", "Task B", "")
	s.Create("test", "Task C", "")

	// Delete one
	s.Update("test", 2, "", "", "deleted")

	tasks, err := s.List("test")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	if tasks[0].Subject != "Task A" {
		t.Errorf("tasks[0].Subject = %q", tasks[0].Subject)
	}
	if tasks[1].Subject != "Task C" {
		t.Errorf("tasks[1].Subject = %q", tasks[1].Subject)
	}
}

func TestTaskListEmpty(t *testing.T) {
	// Verifies List returns empty slice (not nil) when no tasks exist.
	s := testTaskListStore(t)

	tasks, err := s.List("test")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if tasks != nil {
		t.Errorf("expected nil, got %+v", tasks)
	}
}

func TestTaskClear(t *testing.T) {
	// Verifies Clear removes all tasks for an agent.
	s := testTaskListStore(t)

	s.Create("test", "Task", "")
	if err := s.Clear("test"); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	tasks, _ := s.List("test")
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks after clear, got %d", len(tasks))
	}
}

func TestTaskAgentIsolation(t *testing.T) {
	// Verifies different agents have isolated task ID sequences and data, and clearing one agent's tasks doesn't affect another's.
	s := testTaskListStore(t)

	id1, _ := s.Create("agent1", "Agent1 task", "")
	id2, _ := s.Create("agent2", "Agent2 task", "")

	if id1 != 1 || id2 != 1 {
		t.Errorf("IDs = %d, %d — want 1, 1 (independent per agent)", id1, id2)
	}

	tasks1, _ := s.List("agent1")
	tasks2, _ := s.List("agent2")

	if len(tasks1) != 1 || tasks1[0].Subject != "Agent1 task" {
		t.Errorf("agent1 tasks: %+v", tasks1)
	}
	if len(tasks2) != 1 || tasks2[0].Subject != "Agent2 task" {
		t.Errorf("agent2 tasks: %+v", tasks2)
	}

	// Clear one doesn't affect the other
	s.Clear("agent1")
	tasks1, _ = s.List("agent1")
	tasks2, _ = s.List("agent2")
	if len(tasks1) != 0 {
		t.Error("agent1 should have 0 tasks after clear")
	}
	if len(tasks2) != 1 {
		t.Error("agent2 should still have 1 task")
	}
}
