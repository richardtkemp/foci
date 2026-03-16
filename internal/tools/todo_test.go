package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/memory"
)

func TestTodoToolBatchTransitionDone(t *testing.T) {
	// Proves that multiple IDs can be transitioned to done in a single call using the ids array.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id1, _ := store.Add("agent1", "Task 1", "high", "")
	id2, _ := store.Add("agent1", "Task 2", "medium", "")
	id3, _ := store.Add("agent1", "Task 3", "low", "")

	params := map[string]interface{}{
		"action": "transition",
		"state":  "done",
		"ids":    []int64{id1, id2, id3},
		"reason": "batch completed",
	}
	result, err := executeTodoTool(tool, params)
	if err != nil {
		t.Fatalf("batch transition: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}

	items, _ := store.List("agent1", "done", "", "", "", false, 0)
	if len(items) != 3 {
		t.Errorf("expected 3 done items, got %d", len(items))
	}
}

func TestTodoToolBatchEdit(t *testing.T) {
	// Proves that batch edit updates all specified items to the new priority simultaneously.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id1, _ := store.Add("agent1", "Task 1", "high", "")
	id2, _ := store.Add("agent1", "Task 2", "medium", "")
	id3, _ := store.Add("agent1", "Task 3", "low", "")

	params := map[string]interface{}{
		"action":   "edit",
		"ids":      []int64{id1, id2, id3},
		"priority": "high",
	}
	result, err := executeTodoTool(tool, params)
	if err != nil {
		t.Fatalf("batch edit: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}

	items, _ := store.List("agent1", "", "", "", "", false, 0)
	for _, item := range items {
		if item.Priority != "high" {
			t.Errorf("item %d priority = %q, want high", item.ID, item.Priority)
		}
	}
}

func TestTodoToolBatchRemove(t *testing.T) {
	// Proves that batch remove deletes only the specified items, leaving others intact.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id1, _ := store.Add("agent1", "Task 1", "high", "")
	id2, _ := store.Add("agent1", "Task 2", "medium", "")
	store.Add("agent1", "Keep me", "low", "")

	params := map[string]interface{}{
		"action": "remove",
		"ids":    []int64{id1, id2},
	}
	result, err := executeTodoTool(tool, params)
	if err != nil {
		t.Fatalf("batch remove: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}

	items, _ := store.List("agent1", "", "", "", "", false, 0)
	if len(items) != 1 {
		t.Errorf("expected 1 remaining item, got %d", len(items))
	}
}

func TestTodoToolBatchBothIdAndIds(t *testing.T) {
	// Proves that providing both id and ids in the same request is rejected as ambiguous.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id1, _ := store.Add("agent1", "Task 1", "high", "")

	params := map[string]interface{}{
		"action": "transition",
		"state":  "done",
		"id":     id1,
		"ids":    []int64{id1},
		"reason": "should fail",
	}
	_, err := executeTodoTool(tool, params)
	if err == nil {
		t.Error("expected error when both id and ids provided")
	}
}

func TestTodoToolBatchPartialFailure(t *testing.T) {
	// Proves that a batch operation with some invalid IDs succeeds for valid ones and reports
	// failures without returning a Go error, allowing partial progress.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id1, _ := store.Add("agent1", "Task 1", "high", "")
	invalidID := int64(9999)
	id2, _ := store.Add("agent1", "Task 2", "medium", "")

	params := map[string]interface{}{
		"action": "transition",
		"state":  "done",
		"ids":    []int64{id1, invalidID, id2},
		"reason": "partial test",
	}
	result, err := executeTodoTool(tool, params)
	if err != nil {
		t.Fatalf("batch transition should not return error on partial failure: %v", err)
	}

	if result == "" {
		t.Error("expected non-empty result")
	}

	items, _ := store.List("agent1", "done", "", "", "", false, 0)
	if len(items) != 2 {
		t.Errorf("expected 2 done items (valid ones), got %d", len(items))
	}
}

func TestTodoToolSingleIdStillWorks(t *testing.T) {
	// Proves backward compatibility: the singular id field still works for single-item transitions.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id, _ := store.Add("agent1", "Single task", "high", "")

	params := map[string]interface{}{
		"action": "transition",
		"state":  "done",
		"id":     id,
		"reason": "finished",
	}
	result, err := executeTodoTool(tool, params)
	if err != nil {
		t.Fatalf("single transition: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}

	items, _ := store.List("agent1", "done", "", "", "", false, 0)
	if len(items) != 1 {
		t.Errorf("expected 1 done item, got %d", len(items))
	}
}

func TestTodoToolGet(t *testing.T) {
	// Proves that get returns the full formatted representation of a single item including
	// its ID, text, priority, and tag.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id, _ := store.Add("agent1", "Test task", "high", "urgent")

	params := map[string]interface{}{
		"action": "get",
		"id":     id,
	}
	result, err := executeTodoTool(tool, params)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
	if !strings.Contains(result, "#") || !strings.Contains(result, "Test task") {
		t.Errorf("result should contain id and text, got: %s", result)
	}
	if !strings.Contains(result, "`high`") {
		t.Errorf("result should contain priority, got: %s", result)
	}
	if !strings.Contains(result, "`urgent`") {
		t.Errorf("result should contain tag, got: %s", result)
	}
}

func TestTodoToolGetCompleted(t *testing.T) {
	// Proves that getting a completed item shows the [x] marker and the close reason.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id, _ := store.Add("agent1", "Done task", "medium", "")
	store.Complete("agent1", id, "finished")

	params := map[string]interface{}{
		"action": "get",
		"id":     id,
	}
	result, err := executeTodoTool(tool, params)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(result, "[x]") {
		t.Errorf("completed item should show [x], got: %s", result)
	}
	if !strings.Contains(result, "finished") {
		t.Errorf("result should contain close reason, got: %s", result)
	}
}

func TestTodoToolGetNotFound(t *testing.T) {
	// Proves that getting a non-existent ID returns an error.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	params := map[string]interface{}{
		"action": "get",
		"id":     9999,
	}
	_, err := executeTodoTool(tool, params)
	if err == nil {
		t.Error("expected error for non-existent id")
	}
}

func TestTodoToolGetMissingId(t *testing.T) {
	// Proves that get without an id parameter returns a validation error.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	params := map[string]interface{}{
		"action": "get",
	}
	_, err := executeTodoTool(tool, params)
	if err == nil {
		t.Error("expected error when id is missing")
	}
}

func TestTodoToolGetWrongAgent(t *testing.T) {
	// Proves that an agent cannot retrieve todo items belonging to a different agent, enforcing ownership.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id, _ := store.Add("agent2", "Other agent task", "high", "")

	params := map[string]interface{}{
		"action": "get",
		"id":     id,
	}
	_, err := executeTodoTool(tool, params)
	if err == nil {
		t.Error("expected error when getting todo from different agent")
	}
}

func TestTodoToolTransitionDropped(t *testing.T) {
	// Proves that transitioning to "dropped" marks the item as dropped and it appears in the dropped list.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id, _ := store.Add("agent1", "Will drop", "medium", "")

	params := map[string]interface{}{
		"action": "transition",
		"state":  "dropped",
		"id":     id,
		"reason": "no longer relevant",
	}
	result, err := executeTodoTool(tool, params)
	if err != nil {
		t.Fatalf("transition to dropped: %v", err)
	}
	if !strings.Contains(result, "dropped") {
		t.Errorf("expected 'dropped' in result, got: %s", result)
	}

	items, _ := store.List("agent1", "dropped", "", "", "", false, 0)
	if len(items) != 1 {
		t.Errorf("expected 1 dropped item, got %d", len(items))
	}
}

func TestTodoToolTransitionReopen(t *testing.T) {
	// Proves that transitioning a completed item back to "open" clears completed_at and close_reason.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id, _ := store.Add("agent1", "Reopen me", "high", "")
	store.Complete("agent1", id, "done prematurely")

	params := map[string]interface{}{
		"action": "transition",
		"state":  "open",
		"id":     id,
	}
	result, err := executeTodoTool(tool, params)
	if err != nil {
		t.Fatalf("transition to open: %v", err)
	}
	if !strings.Contains(result, "open") {
		t.Errorf("expected 'open' in result, got: %s", result)
	}

	item, _ := store.Get("agent1", id)
	if item.Status != "open" {
		t.Errorf("status = %q, want open", item.Status)
	}
	if item.CompletedAt != nil {
		t.Error("completed_at should be nil after reopen")
	}
	if item.CloseReason != "" {
		t.Errorf("close_reason should be empty after reopen, got %q", item.CloseReason)
	}
}

func TestTodoToolTransitionAliases(t *testing.T) {
	// Proves that "cancelled" and "canceled" are accepted as aliases for "dropped".
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	for _, alias := range []string{"cancelled", "canceled"} {
		id, _ := store.Add("agent1", "Task for "+alias, "medium", "")
		params := map[string]interface{}{
			"action": "transition",
			"state":  alias,
			"id":     id,
			"reason": "testing alias",
		}
		_, err := executeTodoTool(tool, params)
		if err != nil {
			t.Errorf("alias %q failed: %v", alias, err)
			continue
		}
		item, _ := store.Get("agent1", id)
		if item.Status != "dropped" {
			t.Errorf("alias %q: status = %q, want dropped", alias, item.Status)
		}
	}
}

func TestTodoToolTransitionMissingReason(t *testing.T) {
	// Proves that transitioning to "done" without a reason is rejected as a validation error.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id, _ := store.Add("agent1", "No reason", "medium", "")

	params := map[string]interface{}{
		"action": "transition",
		"state":  "done",
		"id":     id,
	}
	_, err := executeTodoTool(tool, params)
	if err == nil {
		t.Error("expected error when reason is missing for done transition")
	}
}

func TestTodoToolCompleteBackCompat(t *testing.T) {
	// Proves that the legacy "complete" action still works as an alias for transitioning to "done".
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id, _ := store.Add("agent1", "Old style", "medium", "")

	params := map[string]interface{}{
		"action": "complete",
		"id":     id,
		"reason": "back compat",
	}
	_, err := executeTodoTool(tool, params)
	if err != nil {
		t.Fatalf("complete back-compat: %v", err)
	}

	item, _ := store.Get("agent1", id)
	if item.Status != "done" {
		t.Errorf("status = %q, want done", item.Status)
	}
}

func TestTodoToolInProgressAliases(t *testing.T) {
	// Proves that all in_progress aliases (wip, started, working, in-progress, etc.) normalize to "in_progress".
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	for _, alias := range []string{"in_progress", "in-progress", "wip", "started", "working"} {
		id, _ := store.Add("agent1", "Task for "+alias, "medium", "")
		params := map[string]interface{}{
			"action": "transition",
			"state":  alias,
			"id":     id,
		}
		_, err := executeTodoTool(tool, params)
		if err != nil {
			t.Errorf("alias %q failed: %v", alias, err)
			continue
		}
		item, _ := store.Get("agent1", id)
		if item.Status != "in_progress" {
			t.Errorf("alias %q: status = %q, want in_progress", alias, item.Status)
		}
	}
}

func TestTodoToolInProgressMarker(t *testing.T) {
	// Proves that an in_progress item shows the [>] marker when retrieved.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id, _ := store.Add("agent1", "Active task", "high", "")
	store.Transition("agent1", id, "in_progress", "")

	params := map[string]interface{}{
		"action": "get",
		"id":     id,
	}
	result, err := executeTodoTool(tool, params)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(result, "[>]") {
		t.Errorf("in_progress item should show [>], got: %s", result)
	}
}

func TestTodoToolInProgressNoReasonRequired(t *testing.T) {
	// Proves that transitioning to in_progress does not require a reason, unlike done/dropped.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id, _ := store.Add("agent1", "Start working", "medium", "")

	params := map[string]interface{}{
		"action": "transition",
		"state":  "in_progress",
		"id":     id,
	}
	_, err := executeTodoTool(tool, params)
	if err != nil {
		t.Errorf("in_progress should not require reason, got: %v", err)
	}
}

func TestTodoToolStatusFilterInProgress(t *testing.T) {
	// Proves that filtering by in_progress (and its aliases) shows only in_progress items
	// and excludes open items.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id1, _ := store.Add("agent1", "Open task", "medium", "")
	id2, _ := store.Add("agent1", "WIP task", "medium", "")
	_ = id1
	store.Transition("agent1", id2, "in_progress", "")

	for _, alias := range []string{"in_progress", "wip", "in-progress"} {
		params := map[string]interface{}{
			"action": "list",
			"status": alias,
		}
		result, err := executeTodoTool(tool, params)
		if err != nil {
			t.Errorf("list with status %q: %v", alias, err)
			continue
		}
		if !strings.Contains(result, "WIP task") {
			t.Errorf("list with status %q should show WIP task, got: %s", alias, result)
		}
		if strings.Contains(result, "Open task") {
			t.Errorf("list with status %q should not show Open task, got: %s", alias, result)
		}
	}
}

func executeTodoTool(tool *Tool, params map[string]interface{}) (string, error) {
	raw, _ := json.Marshal(params)
	result, err := tool.Execute(context.Background(), raw)
	return result.Text, err
}

func TestTodoToolListWithSort(t *testing.T) {
	// Proves that sort=created returns items newest-first by default,
	// reverse=true flips to oldest-first, and sort=updated returns
	// most-recently-modified first.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id1, _ := store.Add("agent1", "First", "medium", "")
	time.Sleep(1100 * time.Millisecond)
	store.Add("agent1", "Second", "medium", "")
	time.Sleep(1100 * time.Millisecond)
	store.Add("agent1", "Third", "medium", "")

	// Test sort by created (default: newest first)
	params := map[string]interface{}{
		"action": "list",
		"sort":   "created",
	}
	result, err := executeTodoTool(tool, params)
	if err != nil {
		t.Fatalf("list with sort=created: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) < 1 {
		t.Fatal("expected at least one line in result")
	}
	// First line should be the newest (Third).
	if !strings.Contains(lines[0], "Third") {
		t.Errorf("first line should contain Third (newest), got: %s", lines[0])
	}

	// Test sort by created with reverse=true (oldest first)
	params = map[string]interface{}{
		"action":  "list",
		"sort":    "created",
		"reverse": true,
	}
	result, err = executeTodoTool(tool, params)
	if err != nil {
		t.Fatalf("list with sort=created reverse: %v", err)
	}
	lines = strings.Split(strings.TrimSpace(result), "\n")
	if !strings.Contains(lines[0], "First") {
		t.Errorf("first line should contain First (oldest), got: %s", lines[0])
	}

	// Test sort by updated (edit one task to make it most recent)
	time.Sleep(1100 * time.Millisecond)
	store.Edit("agent1", id1, "Updated First", "", "", false)
	params = map[string]interface{}{
		"action": "list",
		"sort":   "updated",
	}
	result, err = executeTodoTool(tool, params)
	if err != nil {
		t.Fatalf("list with sort=updated: %v", err)
	}
	lines = strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) < 1 {
		t.Fatal("expected at least one line in result")
	}
	// First line should contain the updated task (newest)
	if !strings.Contains(lines[0], "Updated First") {
		t.Errorf("first line should contain Updated First (newest), got: %s", lines[0])
	}
}

func TestTodoToolListDefaultExcludesDoneDropped(t *testing.T) {
	// Verifies that list without a status filter excludes done and dropped items.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	store.Add("agent1", "Open task", "medium", "")
	id2, _ := store.Add("agent1", "Done task", "medium", "")
	id3, _ := store.Add("agent1", "Dropped task", "medium", "")
	store.Add("agent1", "In-progress task", "medium", "")

	store.Complete("agent1", id2, "finished")
	store.Transition("agent1", id3, "dropped", "not needed")
	store.Transition("agent1", 4, "in_progress", "")

	result, err := executeTodoTool(tool, map[string]interface{}{"action": "list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(result, "Open task") {
		t.Errorf("should include open task, got: %s", result)
	}
	if !strings.Contains(result, "In-progress task") {
		t.Errorf("should include in-progress task, got: %s", result)
	}
	if strings.Contains(result, "Done task") {
		t.Errorf("should exclude done task, got: %s", result)
	}
	if strings.Contains(result, "Dropped task") {
		t.Errorf("should exclude dropped task, got: %s", result)
	}
}

func TestTodoToolListAllIncludesDoneDropped(t *testing.T) {
	// Verifies that list with status=all includes done and dropped items.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	store.Add("agent1", "Open task", "medium", "")
	id2, _ := store.Add("agent1", "Done task", "medium", "")
	id3, _ := store.Add("agent1", "Dropped task", "medium", "")

	store.Complete("agent1", id2, "finished")
	store.Transition("agent1", id3, "dropped", "not needed")

	result, err := executeTodoTool(tool, map[string]interface{}{"action": "list", "status": "all"})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if !strings.Contains(result, "Open task") {
		t.Errorf("should include open task, got: %s", result)
	}
	if !strings.Contains(result, "Done task") {
		t.Errorf("should include done task, got: %s", result)
	}
	if !strings.Contains(result, "Dropped task") {
		t.Errorf("should include dropped task, got: %s", result)
	}
}

func TestTodoToolListDefaultNoActive(t *testing.T) {
	// Verifies that list with no active items shows appropriate message.
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id1, _ := store.Add("agent1", "Done task", "medium", "")
	store.Complete("agent1", id1, "finished")

	result, err := executeTodoTool(tool, map[string]interface{}{"action": "list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(result, "No active todos") {
		t.Errorf("expected 'No active todos', got: %s", result)
	}
}

func TestNormalizeStatusFilter(t *testing.T) {
	// Verifies normalizeStatusFilter maps correctly, including new "all" and "active" values.
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"", "active"},
		{"all", ""},
		{"active", "active"},
		{"open", "open"},
		{"in_progress", "in_progress"},
		{"wip", "in_progress"},
		{"done", "done"},
		{"completed", "done"},
		{"dropped", "dropped"},
		{"cancelled", "dropped"},
	}
	for _, tt := range tests {
		got := normalizeStatusFilter(tt.input)
		if got != tt.want {
			t.Errorf("normalizeStatusFilter(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}


func newTestTodoStore(t *testing.T) *memory.TodoStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "todo_test.db")
	store, err := memory.NewTodoStore(dbPath)
	if err != nil {
		t.Fatalf("NewTodoStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
