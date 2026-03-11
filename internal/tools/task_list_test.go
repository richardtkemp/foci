package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/memory"
)

func testTaskListTool(t *testing.T) (*Tool, *memory.TaskListStore) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "tasklist.db")
	s, err := memory.NewTaskListStore(dbPath)
	if err != nil {
		t.Fatalf("NewTaskListStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return NewTaskListTool(s, "test"), s
}

func execTaskList(t *testing.T, tool *Tool, params any) (ToolResult, error) {
	t.Helper()
	data, _ := json.Marshal(params)
	return tool.Execute(context.Background(), data)
}

// Verifies create returns the new task ID and subject.
func TestTaskListCreate(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	result, err := execTaskList(t, tool, map[string]any{
		"action":  "create",
		"subject": "Fix the bug",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(result.Text, "#1") {
		t.Errorf("missing task ID in result: %q", result.Text)
	}
	if !strings.Contains(result.Text, "Fix the bug") {
		t.Errorf("missing subject in result: %q", result.Text)
	}
}

// Verifies create requires subject.
func TestTaskListCreateValidation(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	_, err := execTaskList(t, tool, map[string]any{"action": "create", "subject": ""})
	if err == nil {
		t.Error("expected error for empty subject")
	}
}

// Verifies get returns full task details.
func TestTaskListGet(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{
		"action":      "create",
		"subject":     "Task one",
		"description": "Details here",
	})

	result, err := execTaskList(t, tool, map[string]any{"action": "get", "id": 1})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(result.Text, "Task one") {
		t.Errorf("missing subject: %q", result.Text)
	}
	if !strings.Contains(result.Text, "Details here") {
		t.Errorf("missing description: %q", result.Text)
	}
	if !strings.Contains(result.Text, "pending") {
		t.Errorf("missing status: %q", result.Text)
	}
}

// Verifies get returns error for nonexistent task.
func TestTaskListGetNotFound(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	_, err := execTaskList(t, tool, map[string]any{"action": "get", "id": 99})
	if err == nil {
		t.Error("expected error for nonexistent task")
	}
}

// Verifies get requires id.
func TestTaskListGetRequiresID(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	_, err := execTaskList(t, tool, map[string]any{"action": "get"})
	if err == nil {
		t.Error("expected error for missing id")
	}
}

// Verifies update changes task fields and returns updated state.
func TestTaskListUpdate(t *testing.T) {
	t.Parallel()
	tool, store := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{
		"action":  "create",
		"subject": "Original",
	})

	result, err := execTaskList(t, tool, map[string]any{
		"action":  "update",
		"id":      1,
		"subject": "Updated",
		"status":  "in_progress",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(result.Text, "Updated") {
		t.Errorf("missing updated subject: %q", result.Text)
	}
	if !strings.Contains(result.Text, "in_progress") {
		t.Errorf("missing updated status: %q", result.Text)
	}

	// Verify store
	task, _ := store.Get("test", 1)
	if task.Subject != "Updated" {
		t.Errorf("store subject = %q", task.Subject)
	}
}

// Verifies update with status=deleted removes the task.
func TestTaskListUpdateDelete(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{
		"action":  "create",
		"subject": "To delete",
	})

	result, err := execTaskList(t, tool, map[string]any{
		"action": "update",
		"id":     1,
		"status": "deleted",
	})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !strings.Contains(result.Text, "deleted") {
		t.Errorf("expected delete confirmation: %q", result.Text)
	}

	// Verify gone
	_, err = execTaskList(t, tool, map[string]any{"action": "get", "id": 1})
	if err == nil {
		t.Error("expected error getting deleted task")
	}
}

// Verifies update requires id.
func TestTaskListUpdateRequiresID(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	_, err := execTaskList(t, tool, map[string]any{"action": "update", "status": "completed"})
	if err == nil {
		t.Error("expected error for missing id")
	}
}

// Verifies list returns all tasks with progress summary.
func TestTaskListList(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	// Empty list
	result, err := execTaskList(t, tool, map[string]any{"action": "list"})
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if !strings.Contains(result.Text, "No tasks") {
		t.Errorf("expected no tasks message: %q", result.Text)
	}

	// With tasks
	execTaskList(t, tool, map[string]any{"action": "create", "subject": "Task A"})
	execTaskList(t, tool, map[string]any{"action": "create", "subject": "Task B"})
	execTaskList(t, tool, map[string]any{"action": "update", "id": 1, "status": "completed"})

	result, err = execTaskList(t, tool, map[string]any{"action": "list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(result.Text, "1/2 completed") {
		t.Errorf("missing progress: %q", result.Text)
	}
	if !strings.Contains(result.Text, "✓ Task A") {
		t.Errorf("missing completed marker: %q", result.Text)
	}
	if !strings.Contains(result.Text, "Task B") {
		t.Errorf("missing task B: %q", result.Text)
	}
}

// Verifies unknown action returns an error.
func TestTaskListUnknownAction(t *testing.T) {
	t.Parallel()
	tool, _ := testTaskListTool(t)

	_, err := execTaskList(t, tool, map[string]any{"action": "reset"})
	if err == nil {
		t.Error("expected error for unknown action")
	}
	if !strings.Contains(err.Error(), "unknown action") {
		t.Errorf("error = %q", err.Error())
	}
}

// Verifies FormatTasks renders markers correctly.
func TestFormatTasks(t *testing.T) {
	t.Parallel()
	tasks := []memory.Task{
		{ID: 1, Subject: "Done task", Status: "completed"},
		{ID: 2, Subject: "Active task", Status: "in_progress"},
		{ID: 3, Subject: "Waiting task", Status: "pending"},
	}

	result := FormatTasks(tasks)
	if !strings.Contains(result, "1/3 completed") {
		t.Errorf("missing progress: %q", result)
	}
	if !strings.Contains(result, "✓ Done task") {
		t.Errorf("missing completed marker: %q", result)
	}
	if !strings.Contains(result, "→ Active task") {
		t.Errorf("missing in_progress marker: %q", result)
	}
	// Pending should have no marker
	if strings.Contains(result, "→ Waiting") || strings.Contains(result, "✓ Waiting") {
		t.Errorf("pending task should have no marker: %q", result)
	}
}

// Verifies FormatTask renders a single task with all fields.
func TestFormatTask(t *testing.T) {
	t.Parallel()
	task := &memory.Task{
		ID:          1,
		Subject:     "My task",
		Description: "Some details",
		Status:      "in_progress",
	}

	result := FormatTask(task)
	if !strings.Contains(result, "#1") {
		t.Errorf("missing ID: %q", result)
	}
	if !strings.Contains(result, "My task") {
		t.Errorf("missing subject: %q", result)
	}
	if !strings.Contains(result, "in_progress") {
		t.Errorf("missing status: %q", result)
	}
	if !strings.Contains(result, "Some details") {
		t.Errorf("missing description: %q", result)
	}
}
