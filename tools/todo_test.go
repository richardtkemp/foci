package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"foci/memory"
)

func TestTodoToolBatchComplete(t *testing.T) {
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id1, _ := store.Add("agent1", "Task 1", "high", "")
	id2, _ := store.Add("agent1", "Task 2", "medium", "")
	id3, _ := store.Add("agent1", "Task 3", "low", "")

	params := map[string]interface{}{
		"action": "complete",
		"ids":    []int64{id1, id2, id3},
		"reason": "batch completed",
	}
	result, err := executeTodoTool(tool, params)
	if err != nil {
		t.Fatalf("batch complete: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}

	items, _ := store.List("agent1", "done", "")
	if len(items) != 3 {
		t.Errorf("expected 3 done items, got %d", len(items))
	}
}

func TestTodoToolBatchEdit(t *testing.T) {
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

	items, _ := store.List("agent1", "", "")
	for _, item := range items {
		if item.Priority != "high" {
			t.Errorf("item %d priority = %q, want high", item.ID, item.Priority)
		}
	}
}

func TestTodoToolBatchRemove(t *testing.T) {
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

	items, _ := store.List("agent1", "", "")
	if len(items) != 1 {
		t.Errorf("expected 1 remaining item, got %d", len(items))
	}
}

func TestTodoToolBatchBothIdAndIds(t *testing.T) {
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id1, _ := store.Add("agent1", "Task 1", "high", "")

	params := map[string]interface{}{
		"action": "complete",
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
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id1, _ := store.Add("agent1", "Task 1", "high", "")
	invalidID := int64(9999)
	id2, _ := store.Add("agent1", "Task 2", "medium", "")

	params := map[string]interface{}{
		"action": "complete",
		"ids":    []int64{id1, invalidID, id2},
		"reason": "partial test",
	}
	result, err := executeTodoTool(tool, params)
	if err != nil {
		t.Fatalf("batch complete should not return error on partial failure: %v", err)
	}

	if result == "" {
		t.Error("expected non-empty result")
	}

	items, _ := store.List("agent1", "done", "")
	if len(items) != 2 {
		t.Errorf("expected 2 done items (valid ones), got %d", len(items))
	}
}

func TestTodoToolSingleIdStillWorks(t *testing.T) {
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	id, _ := store.Add("agent1", "Single task", "high", "")

	params := map[string]interface{}{
		"action": "complete",
		"id":     id,
		"reason": "done",
	}
	result, err := executeTodoTool(tool, params)
	if err != nil {
		t.Fatalf("single complete: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}

	items, _ := store.List("agent1", "done", "")
	if len(items) != 1 {
		t.Errorf("expected 1 done item, got %d", len(items))
	}
}

func executeTodoTool(tool *Tool, params map[string]interface{}) (string, error) {
	raw, _ := json.Marshal(params)
	return tool.Execute(context.Background(), raw)
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
