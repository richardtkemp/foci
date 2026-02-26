package memory

import (
	"path/filepath"
	"testing"
)

func TestTodoAddAndList(t *testing.T) {
	store := newTestTodoStore(t)

	id, err := store.Add("agent1", "Buy milk", "high", "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if id != 1 {
		t.Errorf("expected id 1, got %d", id)
	}

	items, err := store.List("agent1", "", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Text != "Buy milk" {
		t.Errorf("text = %q, want %q", items[0].Text, "Buy milk")
	}
	if items[0].Priority != "high" {
		t.Errorf("priority = %q, want %q", items[0].Priority, "high")
	}
	if items[0].Status != "open" {
		t.Errorf("status = %q, want %q", items[0].Status, "open")
	}
}

func TestTodoDefaultPriority(t *testing.T) {
	store := newTestTodoStore(t)

	store.Add("agent1", "Task with default priority", "", "")

	items, err := store.List("agent1", "", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if items[0].Priority != "medium" {
		t.Errorf("priority = %q, want %q", items[0].Priority, "medium")
	}
}

func TestTodoComplete(t *testing.T) {
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Finish report", "medium", "")
	if err := store.Complete("agent1", id); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	items, err := store.List("agent1", "done", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 done item, got %d", len(items))
	}
	if items[0].CompletedAt == nil {
		t.Error("completed_at should be set")
	}
}

func TestTodoCompleteNotFound(t *testing.T) {
	store := newTestTodoStore(t)

	err := store.Complete("agent1", 999)
	if err == nil {
		t.Error("expected error for nonexistent todo")
	}
}

func TestTodoRemove(t *testing.T) {
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Temp task", "low", "")
	if err := store.Remove("agent1", id); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	items, err := store.List("agent1", "", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 items after remove, got %d", len(items))
	}
}

func TestTodoRemoveNotFound(t *testing.T) {
	store := newTestTodoStore(t)

	err := store.Remove("agent1", 999)
	if err == nil {
		t.Error("expected error for nonexistent todo")
	}
}

func TestTodoAgentIsolation(t *testing.T) {
	store := newTestTodoStore(t)

	store.Add("agent1", "Agent 1 task", "high", "")
	store.Add("agent2", "Agent 2 task", "low", "")

	items1, _ := store.List("agent1", "", "")
	items2, _ := store.List("agent2", "", "")

	if len(items1) != 1 || items1[0].Text != "Agent 1 task" {
		t.Errorf("agent1 items = %v, want 1 item", items1)
	}
	if len(items2) != 1 || items2[0].Text != "Agent 2 task" {
		t.Errorf("agent2 items = %v, want 1 item", items2)
	}
}

func TestTodoListFilterByStatus(t *testing.T) {
	store := newTestTodoStore(t)

	id1, _ := store.Add("agent1", "Open task", "medium", "")
	id2, _ := store.Add("agent1", "Done task", "medium", "")
	_ = id1
	store.Complete("agent1", id2)

	open, _ := store.List("agent1", "open", "")
	done, _ := store.List("agent1", "done", "")
	all, _ := store.List("agent1", "", "")

	if len(open) != 1 {
		t.Errorf("expected 1 open, got %d", len(open))
	}
	if len(done) != 1 {
		t.Errorf("expected 1 done, got %d", len(done))
	}
	if len(all) != 2 {
		t.Errorf("expected 2 total, got %d", len(all))
	}
}

func TestTodoPriorityOrdering(t *testing.T) {
	store := newTestTodoStore(t)

	store.Add("agent1", "Low task", "low", "")
	store.Add("agent1", "High task", "high", "")
	store.Add("agent1", "Medium task", "medium", "")

	items, _ := store.List("agent1", "open", "")
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	if items[0].Priority != "high" {
		t.Errorf("first item priority = %q, want high", items[0].Priority)
	}
	if items[1].Priority != "medium" {
		t.Errorf("second item priority = %q, want medium", items[1].Priority)
	}
	if items[2].Priority != "low" {
		t.Errorf("third item priority = %q, want low", items[2].Priority)
	}
}

func TestTodoCrossAgentComplete(t *testing.T) {
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Agent 1 only", "medium", "")

	// Agent 2 should not be able to complete agent 1's todo
	err := store.Complete("agent2", id)
	if err == nil {
		t.Error("expected error when completing another agent's todo")
	}
}

func TestTodoCrossAgentRemove(t *testing.T) {
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Agent 1 only", "medium", "")

	// Agent 2 should not be able to remove agent 1's todo
	err := store.Remove("agent2", id)
	if err == nil {
		t.Error("expected error when removing another agent's todo")
	}
}

func TestTodoSearch(t *testing.T) {
	store := newTestTodoStore(t)

	store.Add("agent1", "Buy milk from store", "high", "")
	store.Add("agent1", "Fix login bug", "medium", "")
	store.Add("agent1", "Buy groceries", "low", "")
	store.Add("agent2", "Buy something for agent2", "medium", "")

	// Case-insensitive substring match
	items, err := store.Search("agent1", "buy")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(items))
	}

	// No matches
	items, err = store.Search("agent1", "deploy")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 matches, got %d", len(items))
	}

	// Agent isolation — agent2's item should not appear
	items, err = store.Search("agent1", "agent2")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 matches (agent isolation), got %d", len(items))
	}
}

func TestTodoTags(t *testing.T) {
	store := newTestTodoStore(t)

	store.Add("agent1", "Check email", "medium", "background")
	store.Add("agent1", "Review PRs", "high", "background,daily")
	store.Add("agent1", "Regular task", "low", "")

	// Filter by tag
	items, err := store.List("agent1", "", "background")
	if err != nil {
		t.Fatalf("List with tag: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("expected 2 background items, got %d", len(items))
	}

	// Filter by tag + status
	items, err = store.List("agent1", "open", "daily")
	if err != nil {
		t.Fatalf("List with tag+status: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 daily open item, got %d", len(items))
	}

	// No tag filter shows all
	items, err = store.List("agent1", "", "")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(items) != 3 {
		t.Errorf("expected 3 total items, got %d", len(items))
	}
}

func TestTodoCountOpenByTag(t *testing.T) {
	store := newTestTodoStore(t)

	store.Add("agent1", "BG task 1", "medium", "background")
	id2, _ := store.Add("agent1", "BG task 2", "high", "background")
	store.Add("agent1", "Regular", "low", "")
	store.Complete("agent1", id2)

	count, err := store.CountOpenByTag("agent1", "background")
	if err != nil {
		t.Fatalf("CountOpenByTag: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 open background todo, got %d", count)
	}

	// Agent isolation
	count, err = store.CountOpenByTag("agent2", "background")
	if err != nil {
		t.Fatalf("CountOpenByTag agent2: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 for agent2, got %d", count)
	}
}

func TestFormatTags(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"background", " {background}"},
		{"background,daily", " {background,daily}"},
		{" background , daily ", " {background,daily}"},
	}
	for _, tt := range tests {
		got := FormatTags(tt.input)
		if got != tt.want {
			t.Errorf("FormatTags(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func newTestTodoStore(t *testing.T) *TodoStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "todo_test.db")
	store, err := NewTodoStore(dbPath)
	if err != nil {
		t.Fatalf("NewTodoStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
