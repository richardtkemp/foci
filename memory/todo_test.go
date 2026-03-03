package memory

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
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
	if err := store.Complete("agent1", id, "done and dusted"); err != nil {
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
	if items[0].CloseReason != "done and dusted" {
		t.Errorf("close_reason = %q, want %q", items[0].CloseReason, "done and dusted")
	}
}

func TestTodoCompleteNotFound(t *testing.T) {
	store := newTestTodoStore(t)

	err := store.Complete("agent1", 999, "reason")
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

func TestTodoPerAgentIDs(t *testing.T) {
	store := newTestTodoStore(t)

	// Each agent gets its own ID sequence starting from 1.
	id1a, _ := store.Add("agent1", "A1 first", "medium", "")
	id1b, _ := store.Add("agent1", "A1 second", "medium", "")
	id2a, _ := store.Add("agent2", "A2 first", "medium", "")
	id2b, _ := store.Add("agent2", "A2 second", "medium", "")

	if id1a != 1 || id1b != 2 {
		t.Errorf("agent1 IDs = (%d, %d), want (1, 2)", id1a, id1b)
	}
	if id2a != 1 || id2b != 2 {
		t.Errorf("agent2 IDs = (%d, %d), want (1, 2)", id2a, id2b)
	}

	// Both agents have ID 1 — they should resolve to different items.
	item1, err := store.Get("agent1", 1)
	if err != nil {
		t.Fatalf("Get agent1 #1: %v", err)
	}
	if item1.Text != "A1 first" {
		t.Errorf("agent1 #1 text = %q, want %q", item1.Text, "A1 first")
	}

	item2, err := store.Get("agent2", 1)
	if err != nil {
		t.Fatalf("Get agent2 #1: %v", err)
	}
	if item2.Text != "A2 first" {
		t.Errorf("agent2 #1 text = %q, want %q", item2.Text, "A2 first")
	}
}

func TestTodoMigrationFromAutoincrement(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "todo_migrate.db")

	// Create an old-schema table with AUTOINCREMENT.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE todos (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		text         TEXT    NOT NULL,
		status       TEXT    NOT NULL DEFAULT 'open',
		priority     TEXT    NOT NULL DEFAULT 'medium',
		tags         TEXT    NOT NULL DEFAULT '',
		close_reason TEXT    NOT NULL DEFAULT '',
		agent_id     TEXT    NOT NULL,
		created_at   TEXT    NOT NULL,
		completed_at TEXT,
		updated_at   TEXT
	)`)
	if err != nil {
		t.Fatalf("create old table: %v", err)
	}

	// Insert data with global IDs (1, 2, 3).
	now := time.Now().UTC().Format(time.RFC3339)
	db.Exec(`INSERT INTO todos (text, status, priority, tags, agent_id, created_at, updated_at) VALUES ('Task A1', 'open', 'high', '', 'agent1', ?, ?)`, now, now)
	db.Exec(`INSERT INTO todos (text, status, priority, tags, agent_id, created_at, updated_at) VALUES ('Task A2', 'open', 'medium', '', 'agent2', ?, ?)`, now, now)
	db.Exec(`INSERT INTO todos (text, status, priority, tags, agent_id, created_at, updated_at) VALUES ('Task A3', 'open', 'low', '', 'agent1', ?, ?)`, now, now)
	_ = db.Close()

	// Open with NewTodoStore — should trigger migration.
	store, err := NewTodoStore(dbPath)
	if err != nil {
		t.Fatalf("NewTodoStore after migration: %v", err)
	}
	defer store.Close()

	// Existing IDs should be preserved.
	items1, _ := store.List("agent1", "", "")
	if len(items1) != 2 {
		t.Fatalf("agent1: expected 2 items, got %d", len(items1))
	}
	// Old global IDs 1 and 3 are preserved.
	if items1[0].ID != 1 || items1[1].ID != 3 {
		t.Errorf("agent1 IDs = (%d, %d), want (1, 3)", items1[0].ID, items1[1].ID)
	}

	items2, _ := store.List("agent2", "", "")
	if len(items2) != 1 {
		t.Fatalf("agent2: expected 1 item, got %d", len(items2))
	}
	if items2[0].ID != 2 {
		t.Errorf("agent2 ID = %d, want 2", items2[0].ID)
	}

	// New IDs continue from max(id)+1 per agent.
	newID1, _ := store.Add("agent1", "New A1", "medium", "")
	if newID1 != 4 {
		t.Errorf("agent1 new ID = %d, want 4 (max was 3)", newID1)
	}
	newID2, _ := store.Add("agent2", "New A2", "medium", "")
	if newID2 != 3 {
		t.Errorf("agent2 new ID = %d, want 3 (max was 2)", newID2)
	}
}

func TestTodoListFilterByStatus(t *testing.T) {
	store := newTestTodoStore(t)

	id1, _ := store.Add("agent1", "Open task", "medium", "")
	id2, _ := store.Add("agent1", "Done task", "medium", "")
	_ = id1
	store.Complete("agent1", id2, "finished")

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
	err := store.Complete("agent2", id, "reason")
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
	store.Complete("agent1", id2, "completed")

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

func TestTodoEdit(t *testing.T) {
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Original text", "high", "work")

	// Edit text only — priority and tags stay unchanged.
	item, err := store.Edit("agent1", id, "Updated text", "", "", false)
	if err != nil {
		t.Fatalf("Edit text: %v", err)
	}
	if item.Text != "Updated text" {
		t.Errorf("text = %q, want %q", item.Text, "Updated text")
	}
	if item.Priority != "high" {
		t.Errorf("priority = %q, want %q (unchanged)", item.Priority, "high")
	}
	if item.Tags != "work" {
		t.Errorf("tags = %q, want %q (unchanged)", item.Tags, "work")
	}
}

func TestTodoEditPriority(t *testing.T) {
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "My task", "high", "")

	item, err := store.Edit("agent1", id, "", "low", "", false)
	if err != nil {
		t.Fatalf("Edit priority: %v", err)
	}
	if item.Priority != "low" {
		t.Errorf("priority = %q, want %q", item.Priority, "low")
	}
	if item.Text != "My task" {
		t.Errorf("text = %q, want %q (unchanged)", item.Text, "My task")
	}
}

func TestTodoEditTags(t *testing.T) {
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Tagged task", "medium", "old")

	// Set new tag.
	item, err := store.Edit("agent1", id, "", "", "new", true)
	if err != nil {
		t.Fatalf("Edit tags: %v", err)
	}
	if item.Tags != "new" {
		t.Errorf("tags = %q, want %q", item.Tags, "new")
	}

	// Clear tags by setting to empty with setTags=true.
	item, err = store.Edit("agent1", id, "", "", "", true)
	if err != nil {
		t.Fatalf("Edit clear tags: %v", err)
	}
	if item.Tags != "" {
		t.Errorf("tags = %q, want empty", item.Tags)
	}
}

func TestTodoEditMultipleFields(t *testing.T) {
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Original", "low", "a")

	item, err := store.Edit("agent1", id, "New text", "high", "b,c", true)
	if err != nil {
		t.Fatalf("Edit multiple: %v", err)
	}
	if item.Text != "New text" {
		t.Errorf("text = %q, want %q", item.Text, "New text")
	}
	if item.Priority != "high" {
		t.Errorf("priority = %q, want %q", item.Priority, "high")
	}
	if item.Tags != "b,c" {
		t.Errorf("tags = %q, want %q", item.Tags, "b,c")
	}
}

func TestTodoEditNotFound(t *testing.T) {
	store := newTestTodoStore(t)

	_, err := store.Edit("agent1", 999, "text", "", "", false)
	if err == nil {
		t.Error("expected error for nonexistent todo")
	}
}

func TestTodoCrossAgentEdit(t *testing.T) {
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Agent 1 only", "medium", "")

	// Agent 2 should not be able to edit agent 1's todo.
	_, err := store.Edit("agent2", id, "hacked", "", "", false)
	if err == nil {
		t.Error("expected error when editing another agent's todo")
	}
}

func TestTodoEditNothingToUpdate(t *testing.T) {
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Task", "medium", "")

	_, err := store.Edit("agent1", id, "", "", "", false)
	if err == nil {
		t.Error("expected error when nothing to update")
	}
}

func TestTodoUpdatedAtOnAdd(t *testing.T) {
	store := newTestTodoStore(t)

	before := time.Now().UTC().Truncate(time.Second)
	id, _ := store.Add("agent1", "Task", "medium", "")
	after := time.Now().UTC().Truncate(time.Second).Add(time.Second)

	items, _ := store.List("agent1", "", "")
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.ID != id {
		t.Errorf("ID = %d, want %d", item.ID, id)
	}
	if item.CreatedAt.IsZero() {
		t.Error("created_at should be set")
	}
	if item.UpdatedAt.IsZero() {
		t.Error("updated_at should be set")
	}
	if item.CreatedAt.Before(before) || item.CreatedAt.After(after) {
		t.Errorf("created_at = %v, expected between %v and %v", item.CreatedAt, before, after)
	}
	if item.UpdatedAt.Before(before) || item.UpdatedAt.After(after) {
		t.Errorf("updated_at = %v, expected between %v and %v", item.UpdatedAt, before, after)
	}
}

func TestTodoUpdatedAtOnEdit(t *testing.T) {
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Original", "medium", "")
	items, _ := store.List("agent1", "", "")
	originalUpdatedAt := items[0].UpdatedAt

	time.Sleep(1100 * time.Millisecond)

	_, err := store.Edit("agent1", id, "Updated", "", "", false)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}

	items, _ = store.List("agent1", "", "")
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].UpdatedAt.Equal(originalUpdatedAt) {
		t.Errorf("updated_at should change on edit, got %v (same as before)", items[0].UpdatedAt)
	}
	if items[0].Text != "Updated" {
		t.Errorf("text = %q, want %q", items[0].Text, "Updated")
	}
}

func TestTodoUpdatedAtOnComplete(t *testing.T) {
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Task", "medium", "")
	items, _ := store.List("agent1", "", "")
	originalUpdatedAt := items[0].UpdatedAt

	time.Sleep(1100 * time.Millisecond)

	err := store.Complete("agent1", id, "done")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	items, _ = store.List("agent1", "done", "")
	if len(items) != 1 {
		t.Fatalf("expected 1 done item, got %d", len(items))
	}
	if items[0].UpdatedAt.Equal(originalUpdatedAt) {
		t.Errorf("updated_at should change on complete, got %v (same as before)", items[0].UpdatedAt)
	}
	if items[0].CompletedAt == nil {
		t.Error("completed_at should be set")
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
