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
	return NewTaskListTool(s, "test", nil), s
}

func execTaskList(t *testing.T, tool *Tool, params any) (ToolResult, error) {
	t.Helper()
	data, _ := json.Marshal(params)
	return tool.Execute(context.Background(), data)
}

func TestTaskListCreate(t *testing.T) {
	// Verifies create returns the new task ID and subject.
	t.Parallel()
	tool, _ := testTaskListTool(t)

	result, err := execTaskList(t, tool, map[string]any{
		"action": "create",
		"tasks":  []map[string]any{{"subject": "Fix the bug"}},
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

func TestTaskListCreateValidation(t *testing.T) {
	// Verifies create requires a non-empty tasks array.
	t.Parallel()
	tool, _ := testTaskListTool(t)

	_, err := execTaskList(t, tool, map[string]any{"action": "create", "tasks": []map[string]any{}})
	if err == nil {
		t.Error("expected error for empty tasks array")
	}
}

func TestTaskListGet(t *testing.T) {
	// Verifies get returns full task details.
	t.Parallel()
	tool, _ := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{
		"action": "create",
		"tasks":  []map[string]any{{"subject": "Task one", "description": "Details here"}},
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

func TestTaskListGetNotFound(t *testing.T) {
	// Verifies get returns error for nonexistent task.
	t.Parallel()
	tool, _ := testTaskListTool(t)

	_, err := execTaskList(t, tool, map[string]any{"action": "get", "id": 99})
	if err == nil {
		t.Error("expected error for nonexistent task")
	}
}

func TestTaskListGetRequiresID(t *testing.T) {
	// Verifies get requires id.
	t.Parallel()
	tool, _ := testTaskListTool(t)

	_, err := execTaskList(t, tool, map[string]any{"action": "get"})
	if err == nil {
		t.Error("expected error for missing id")
	}
}

func TestTaskListUpdate(t *testing.T) {
	// Verifies update changes task fields and returns updated state.
	t.Parallel()
	tool, store := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{
		"action": "create",
		"tasks":  []map[string]any{{"subject": "Original"}},
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

func TestTaskListUpdateDelete(t *testing.T) {
	// Verifies update with status=deleted removes the task.
	t.Parallel()
	tool, _ := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{
		"action": "create",
		"tasks":  []map[string]any{{"subject": "To delete"}},
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

func TestTaskListUpdateRequiresID(t *testing.T) {
	// Verifies update requires id.
	t.Parallel()
	tool, _ := testTaskListTool(t)

	_, err := execTaskList(t, tool, map[string]any{"action": "update", "status": "completed"})
	if err == nil {
		t.Error("expected error for missing id")
	}
}

func TestTaskListList(t *testing.T) {
	// Verifies list returns all tasks with progress summary.
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
	execTaskList(t, tool, map[string]any{"action": "create", "tasks": []map[string]any{{"subject": "Task A"}}})
	execTaskList(t, tool, map[string]any{"action": "create", "tasks": []map[string]any{{"subject": "Task B"}}})
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

func TestTaskListNotifications(t *testing.T) {
	// Verifies notifications fire on create and status-changing update.
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "tasklist.db")
	s, err := memory.NewTaskListStore(dbPath)
	if err != nil {
		t.Fatalf("NewTaskListStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	var notifications []string
	notify := func(sk, msg string) {
		notifications = append(notifications, sk+"|"+msg)
	}
	tool := NewTaskListTool(s, "test", notify)

	ctx := WithSessionKey(context.Background(), "agent:chat:123:v1")
	data, _ := json.Marshal(map[string]any{"action": "create", "tasks": []map[string]any{{"subject": "Review cache"}}})
	if _, err := tool.Execute(ctx, data); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notifications))
	}
	if !strings.Contains(notifications[0], "📋 Created #1: Review cache") {
		t.Errorf("unexpected create notification: %q", notifications[0])
	}
	if !strings.HasPrefix(notifications[0], "agent:chat:123:v1|") {
		t.Errorf("notification missing session key: %q", notifications[0])
	}

	// Mark in_progress — should fire
	data, _ = json.Marshal(map[string]any{"action": "update", "id": 1, "status": "in_progress"})
	if _, err := tool.Execute(ctx, data); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(notifications) != 2 {
		t.Fatalf("expected 2 notifications, got %d", len(notifications))
	}
	if !strings.Contains(notifications[1], "🔄 0/1: Review cache") {
		t.Errorf("unexpected in_progress notification: %q", notifications[1])
	}

	// Mark completed — should fire with progress
	data, _ = json.Marshal(map[string]any{"action": "create", "tasks": []map[string]any{{"subject": "Write tests"}}})
	tool.Execute(ctx, data) // creates #2, fires notification
	data, _ = json.Marshal(map[string]any{"action": "update", "id": 1, "status": "completed"})
	if _, err := tool.Execute(ctx, data); err != nil {
		t.Fatalf("complete: %v", err)
	}
	last := notifications[len(notifications)-1]
	if !strings.Contains(last, "✅ 1/2: Review cache") {
		t.Errorf("unexpected completed notification: %q", last)
	}
	if !strings.Contains(last, "up next: 2/2 Write tests") {
		t.Errorf("expected 'up next' in completed notification: %q", last)
	}

	// Update without status change — should NOT fire
	countBefore := len(notifications)
	data, _ = json.Marshal(map[string]any{"action": "update", "id": 2, "subject": "Write more tests"})
	if _, err := tool.Execute(ctx, data); err != nil {
		t.Fatalf("update subject: %v", err)
	}
	if len(notifications) != countBefore {
		t.Errorf("subject-only update should not fire notification")
	}
}

func TestTaskListBatchNotifyListsTasks(t *testing.T) {
	// Verifies batch create notification lists each task subject (not just a count).
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "tasklist.db")
	s, err := memory.NewTaskListStore(dbPath)
	if err != nil {
		t.Fatalf("NewTaskListStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	var notifications []string
	notify := func(_, msg string) { notifications = append(notifications, msg) }
	tool := NewTaskListTool(s, "test", notify)

	ctx := WithSessionKey(context.Background(), "agent:chat:1:v1")
	data, _ := json.Marshal(map[string]any{
		"action": "create",
		"tasks": []map[string]any{
			{"subject": "Alpha"},
			{"subject": "Beta"},
			{"subject": "Gamma"},
		},
	})
	if _, err := tool.Execute(ctx, data); err != nil {
		t.Fatalf("batch create: %v", err)
	}
	if len(notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notifications))
	}
	n := notifications[0]
	if !strings.Contains(n, "Created 3 tasks") {
		t.Errorf("missing count header: %q", n)
	}
	for _, subj := range []string{"Alpha", "Beta", "Gamma"} {
		if !strings.Contains(n, subj) {
			t.Errorf("missing subject %q in notification: %q", subj, n)
		}
	}
}

func TestTaskListCompletedUpNext(t *testing.T) {
	// Verifies completed notification shows the next pending task.
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "tasklist.db")
	s, err := memory.NewTaskListStore(dbPath)
	if err != nil {
		t.Fatalf("NewTaskListStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	var notifications []string
	notify := func(_, msg string) { notifications = append(notifications, msg) }
	tool := NewTaskListTool(s, "test", notify)

	ctx := WithSessionKey(context.Background(), "agent:chat:1:v1")

	// Create 3 tasks.
	data, _ := json.Marshal(map[string]any{
		"action": "create",
		"tasks": []map[string]any{
			{"subject": "First"},
			{"subject": "Second"},
			{"subject": "Third"},
		},
	})
	tool.Execute(ctx, data)

	// Complete task #1 — should show "up next: 2/3 Second".
	data, _ = json.Marshal(map[string]any{"action": "update", "id": 1, "status": "completed"})
	if _, err := tool.Execute(ctx, data); err != nil {
		t.Fatalf("complete: %v", err)
	}
	last := notifications[len(notifications)-1]
	if !strings.Contains(last, "✅ 1/3: First") {
		t.Errorf("unexpected completed notification: %q", last)
	}
	if !strings.Contains(last, "up next: 2/3 Second") {
		t.Errorf("expected 'up next' line: %q", last)
	}

	// Complete task #2 — should show "up next: 3/3 Third".
	data, _ = json.Marshal(map[string]any{"action": "update", "id": 2, "status": "completed"})
	if _, err := tool.Execute(ctx, data); err != nil {
		t.Fatalf("complete: %v", err)
	}
	last = notifications[len(notifications)-1]
	if !strings.Contains(last, "up next: 3/3 Third") {
		t.Errorf("expected 'up next: 3/3 Third': %q", last)
	}

	// Complete task #3 — no more pending, no "up next".
	data, _ = json.Marshal(map[string]any{"action": "update", "id": 3, "status": "completed"})
	if _, err := tool.Execute(ctx, data); err != nil {
		t.Fatalf("complete: %v", err)
	}
	last = notifications[len(notifications)-1]
	if strings.Contains(last, "up next") {
		t.Errorf("should not have 'up next' when no pending tasks: %q", last)
	}
}

func TestTaskListUnknownAction(t *testing.T) {
	// Verifies unknown action returns an error.
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

func TestTaskListBatchCreate(t *testing.T) {
	// Verifies that a tasks array with multiple items creates one task per item,
	// each with its own description.
	t.Parallel()
	tool, store := testTaskListTool(t)

	result, err := execTaskList(t, tool, map[string]any{
		"action": "create",
		"tasks": []map[string]any{
			{"subject": "Task A", "description": "Details A"},
			{"subject": "Task B", "description": "Details B"},
			{"subject": "Task C", "description": "Details C"},
		},
	})
	if err != nil {
		t.Fatalf("batch create: %v", err)
	}
	if !strings.Contains(result.Text, "#1") || !strings.Contains(result.Text, "#2") || !strings.Contains(result.Text, "#3") {
		t.Errorf("expected three task IDs: %q", result.Text)
	}
	if !strings.Contains(result.Text, "Task A") || !strings.Contains(result.Text, "Task C") {
		t.Errorf("missing task subjects: %q", result.Text)
	}

	tasks, _ := store.List("test")
	if len(tasks) != 3 {
		t.Fatalf("got %d tasks, want 3", len(tasks))
	}
	if tasks[0].Subject != "Task A" || tasks[1].Subject != "Task B" || tasks[2].Subject != "Task C" {
		t.Errorf("subjects = %q, %q, %q", tasks[0].Subject, tasks[1].Subject, tasks[2].Subject)
	}
	if tasks[0].Description != "Details A" || tasks[1].Description != "Details B" || tasks[2].Description != "Details C" {
		t.Errorf("descriptions = %q, %q, %q", tasks[0].Description, tasks[1].Description, tasks[2].Description)
	}
}

func TestTaskListCreateSingle(t *testing.T) {
	// Verifies that a single-element tasks array works normally.
	t.Parallel()
	tool, _ := testTaskListTool(t)

	result, err := execTaskList(t, tool, map[string]any{
		"action": "create",
		"tasks":  []map[string]any{{"subject": "Just one task"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(result.Text, "#1") || !strings.Contains(result.Text, "Just one task") {
		t.Errorf("unexpected result: %q", result.Text)
	}
}

func TestTaskListAutoClearOnAllCompleted(t *testing.T) {
	// Verifies that completing the last pending task clears the entire list.
	t.Parallel()
	tool, store := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{"action": "create", "tasks": []map[string]any{{"subject": "Task A"}}})
	execTaskList(t, tool, map[string]any{"action": "create", "tasks": []map[string]any{{"subject": "Task B"}}})

	// Complete first — list should NOT be cleared yet.
	execTaskList(t, tool, map[string]any{"action": "update", "id": 1, "status": "completed"})
	tasks, _ := store.List("test")
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks after partial completion, got %d", len(tasks))
	}

	// Complete second — list should be cleared.
	result, err := execTaskList(t, tool, map[string]any{"action": "update", "id": 2, "status": "completed"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !strings.Contains(result.Text, "list cleared") {
		t.Errorf("expected clear message: %q", result.Text)
	}

	tasks, _ = store.List("test")
	if len(tasks) != 0 {
		t.Errorf("expected 0 tasks after auto-clear, got %d", len(tasks))
	}
}

func TestTaskListNoClearWithPending(t *testing.T) {
	// Verifies that the list is NOT cleared when non-completed tasks remain.
	t.Parallel()
	tool, store := testTaskListTool(t)

	execTaskList(t, tool, map[string]any{"action": "create", "tasks": []map[string]any{{"subject": "Task A"}}})
	execTaskList(t, tool, map[string]any{"action": "create", "tasks": []map[string]any{{"subject": "Task B"}}})
	execTaskList(t, tool, map[string]any{"action": "update", "id": 2, "status": "in_progress"})

	result, _ := execTaskList(t, tool, map[string]any{"action": "update", "id": 1, "status": "completed"})
	if strings.Contains(result.Text, "list cleared") {
		t.Errorf("should not clear with in_progress tasks remaining: %q", result.Text)
	}

	tasks, _ := store.List("test")
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestFormatTasks(t *testing.T) {
	// Verifies FormatTasks renders markers correctly.
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

func TestFormatTask(t *testing.T) {
	// Verifies FormatTask renders a single task with all fields.
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
