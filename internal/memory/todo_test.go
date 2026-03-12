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

	items, err := store.List("agent1", "", "", "", "")
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

	items, err := store.List("agent1", "", "", "", "")
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

	items, err := store.List("agent1", "done", "", "", "")
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

	items, err := store.List("agent1", "", "", "", "")
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

	items1, _ := store.List("agent1", "", "", "", "")
	items2, _ := store.List("agent2", "", "", "", "")

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

func TestTodoListFilterByStatus(t *testing.T) {
	store := newTestTodoStore(t)

	id1, _ := store.Add("agent1", "Open task", "medium", "")
	id2, _ := store.Add("agent1", "Done task", "medium", "")
	_ = id1
	store.Complete("agent1", id2, "finished")

	open, _ := store.List("agent1", "open", "", "", "")
	done, _ := store.List("agent1", "done", "", "", "")
	all, _ := store.List("agent1", "", "", "", "")

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

// Verifies the "active" status filter excludes done and dropped items.
func TestTodoListFilterActive(t *testing.T) {
	store := newTestTodoStore(t)

	store.Add("agent1", "Open task", "medium", "")
	id2, _ := store.Add("agent1", "Done task", "medium", "")
	id3, _ := store.Add("agent1", "Dropped task", "medium", "")
	id4, _ := store.Add("agent1", "WIP task", "medium", "")

	store.Complete("agent1", id2, "finished")
	store.Transition("agent1", id3, "dropped", "not needed")
	store.Transition("agent1", id4, "in_progress", "")

	active, err := store.List("agent1", "active", "", "", "")
	if err != nil {
		t.Fatalf("List active: %v", err)
	}
	if len(active) != 2 {
		t.Errorf("expected 2 active, got %d", len(active))
	}
	for _, item := range active {
		if item.Status == "done" || item.Status == "dropped" {
			t.Errorf("active filter should exclude %q items", item.Status)
		}
	}
}

func TestTodoListFilterByPriority(t *testing.T) {
	store := newTestTodoStore(t)

	store.Add("agent1", "High task 1", "high", "")
	store.Add("agent1", "Medium task", "medium", "")
	store.Add("agent1", "High task 2", "high", "")
	store.Add("agent1", "Low task", "low", "")

	// Filter by priority
	items, err := store.List("agent1", "", "", "high", "")
	if err != nil {
		t.Fatalf("List with priority: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 high items, got %d", len(items))
	}
	for _, item := range items {
		if item.Priority != "high" {
			t.Errorf("item %d priority = %q, want high", item.ID, item.Priority)
		}
	}

	// Filter by priority + status
	store.Complete("agent1", items[0].ID, "done")
	open, err := store.List("agent1", "open", "", "high", "")
	if err != nil {
		t.Fatalf("List with priority+status: %v", err)
	}
	if len(open) != 1 {
		t.Errorf("expected 1 open high item, got %d", len(open))
	}

	// No priority filter shows all
	all, err := store.List("agent1", "", "", "", "")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 4 {
		t.Errorf("expected 4 total items, got %d", len(all))
	}
}

func TestTodoPriorityOrdering(t *testing.T) {
	store := newTestTodoStore(t)

	store.Add("agent1", "Low task", "low", "")
	store.Add("agent1", "High task", "high", "")
	store.Add("agent1", "Medium task", "medium", "")

	items, _ := store.List("agent1", "open", "", "", "")
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
	items, err := store.List("agent1", "", "background", "", "")
	if err != nil {
		t.Fatalf("List with tag: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("expected 2 background items, got %d", len(items))
	}

	// Filter by tag + status
	items, err = store.List("agent1", "open", "daily", "", "")
	if err != nil {
		t.Fatalf("List with tag+status: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 daily open item, got %d", len(items))
	}

	// No tag filter shows all
	items, err = store.List("agent1", "", "", "", "")
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

	items, _ := store.List("agent1", "", "", "", "")
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
	items, _ := store.List("agent1", "", "", "", "")
	originalUpdatedAt := items[0].UpdatedAt

	time.Sleep(1100 * time.Millisecond)

	_, err := store.Edit("agent1", id, "Updated", "", "", false)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}

	items, _ = store.List("agent1", "", "", "", "")
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
	items, _ := store.List("agent1", "", "", "", "")
	originalUpdatedAt := items[0].UpdatedAt

	time.Sleep(1100 * time.Millisecond)

	err := store.Complete("agent1", id, "done")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	items, _ = store.List("agent1", "done", "", "", "")
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

func TestTodoTransitionInProgress(t *testing.T) {
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Working on it", "high", "")

	// Transition to in_progress
	if err := store.Transition("agent1", id, "in_progress", ""); err != nil {
		t.Fatalf("Transition to in_progress: %v", err)
	}

	item, err := store.Get("agent1", id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if item.Status != "in_progress" {
		t.Errorf("status = %q, want in_progress", item.Status)
	}
	if item.CompletedAt != nil {
		t.Error("completed_at should be nil for in_progress")
	}
	if item.CloseReason != "" {
		t.Errorf("close_reason should be empty for in_progress, got %q", item.CloseReason)
	}

	// Transition from in_progress to done
	if err := store.Transition("agent1", id, "done", "finished"); err != nil {
		t.Fatalf("Transition to done: %v", err)
	}
	item, _ = store.Get("agent1", id)
	if item.Status != "done" {
		t.Errorf("status = %q, want done", item.Status)
	}
	if item.CompletedAt == nil {
		t.Error("completed_at should be set after done")
	}

	// Transition from done back to in_progress
	if err := store.Transition("agent1", id, "in_progress", ""); err != nil {
		t.Fatalf("Transition back to in_progress: %v", err)
	}
	item, _ = store.Get("agent1", id)
	if item.Status != "in_progress" {
		t.Errorf("status = %q, want in_progress", item.Status)
	}
	if item.CompletedAt != nil {
		t.Error("completed_at should be nil after reverting to in_progress")
	}
}

func TestTodoSortOrderInProgress(t *testing.T) {
	store := newTestTodoStore(t)

	store.Add("agent1", "Open task", "high", "")
	id2, _ := store.Add("agent1", "In progress task", "high", "")
	store.Add("agent1", "Another open task", "high", "")

	store.Transition("agent1", id2, "in_progress", "")

	items, err := store.List("agent1", "", "", "", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	if items[0].Status != "in_progress" {
		t.Errorf("first item status = %q, want in_progress", items[0].Status)
	}
	if items[1].Status != "open" || items[2].Status != "open" {
		t.Errorf("remaining items should be open, got %q and %q", items[1].Status, items[2].Status)
	}
}

func TestTodoSortByCreated(t *testing.T) {
	// Test sorting by creation timestamp (oldest first)
	store := newTestTodoStore(t)

	id1, _ := store.Add("agent1", "First task", "high", "")
	time.Sleep(50 * time.Millisecond)
	id2, _ := store.Add("agent1", "Second task", "high", "")
	time.Sleep(50 * time.Millisecond)
	id3, _ := store.Add("agent1", "Third task", "high", "")

	items, err := store.List("agent1", "", "", "", "created")
	if err != nil {
		t.Fatalf("List with sort=created: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	// Should be ordered by creation time (oldest first)
	if items[0].ID != id1 {
		t.Errorf("first item ID = %d, want %d (oldest)", items[0].ID, id1)
	}
	if items[1].ID != id2 {
		t.Errorf("second item ID = %d, want %d", items[1].ID, id2)
	}
	if items[2].ID != id3 {
		t.Errorf("third item ID = %d, want %d (newest)", items[2].ID, id3)
	}
}

func TestTodoSortByUpdated(t *testing.T) {
	// Test sorting by updated timestamp (newest first)
	store := newTestTodoStore(t)

	// Add items with delays to ensure distinct timestamps (need >1s for RFC3339 precision)
	id1, _ := store.Add("agent1", "Task 1", "medium", "")
	time.Sleep(1100 * time.Millisecond)
	id2, _ := store.Add("agent1", "Task 2", "medium", "")
	time.Sleep(1100 * time.Millisecond)
	id3, _ := store.Add("agent1", "Task 3", "medium", "")

	// Wait and edit id1 to make it most recently updated (newer than id3)
	time.Sleep(1100 * time.Millisecond)
	store.Edit("agent1", id1, "Updated task 1", "", "", false)

	items, err := store.List("agent1", "", "", "", "updated")
	if err != nil {
		t.Fatalf("List with sort=updated: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	// Should be ordered by updated time (newest first)
	if items[0].ID != id1 {
		t.Errorf("first item ID = %d, want %d (most recently updated)", items[0].ID, id1)
	}
	if items[1].ID != id3 {
		t.Errorf("second item ID = %d, want %d", items[1].ID, id3)
	}
	if items[2].ID != id2 {
		t.Errorf("third item ID = %d, want %d", items[2].ID, id2)
	}
}

func TestTodoSortByPriorityDefault(t *testing.T) {
	// Test that priority sort is the default
	store := newTestTodoStore(t)

	store.Add("agent1", "Low task", "low", "")
	store.Add("agent1", "High task", "high", "")
	store.Add("agent1", "Medium task", "medium", "")

	// Empty sort parameter should use priority sort
	items, err := store.List("agent1", "open", "", "", "")
	if err != nil {
		t.Fatalf("List with empty sort: %v", err)
	}
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

	// Explicit priority sort should also work
	items2, err := store.List("agent1", "open", "", "", "priority")
	if err != nil {
		t.Fatalf("List with sort=priority: %v", err)
	}
	if len(items2) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items2))
	}
	if items2[0].Priority != "high" {
		t.Errorf("first item priority = %q, want high", items2[0].Priority)
	}
}

func TestTodoSortByCreatedIgnoresStatus(t *testing.T) {
	// Test that sort=created sorts purely by timestamp, ignoring status
	store := newTestTodoStore(t)

	// Create items with different statuses at different times
	id1, _ := store.Add("agent1", "First task", "high", "")
	time.Sleep(50 * time.Millisecond)
	id2, _ := store.Add("agent1", "Second task", "high", "")
	store.Transition("agent1", id2, "in_progress", "")
	time.Sleep(50 * time.Millisecond)
	id3, _ := store.Add("agent1", "Third task", "high", "")
	store.Transition("agent1", id3, "done", "completed")
	time.Sleep(50 * time.Millisecond)
	id4, _ := store.Add("agent1", "Fourth task", "high", "")

	// List with sort=created should ignore status and sort purely by creation time
	items, err := store.List("agent1", "", "", "", "created")
	if err != nil {
		t.Fatalf("List with sort=created: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("expected 4 items, got %d", len(items))
	}
	// Should be ordered by creation time only, not grouped by status
	if items[0].ID != id1 {
		t.Errorf("first item ID = %d (status=%s), want %d", items[0].ID, items[0].Status, id1)
	}
	if items[1].ID != id2 {
		t.Errorf("second item ID = %d (status=%s), want %d", items[1].ID, items[1].Status, id2)
	}
	if items[2].ID != id3 {
		t.Errorf("third item ID = %d (status=%s), want %d", items[2].ID, items[2].Status, id3)
	}
	if items[3].ID != id4 {
		t.Errorf("fourth item ID = %d (status=%s), want %d", items[3].ID, items[3].Status, id4)
	}
}

func TestTodoSortByUpdatedIgnoresStatus(t *testing.T) {
	// Test that sort=updated sorts purely by timestamp, ignoring status
	store := newTestTodoStore(t)

	// Create items with different statuses and update at different times
	id1, _ := store.Add("agent1", "Task 1", "medium", "")
	time.Sleep(1100 * time.Millisecond)
	id2, _ := store.Add("agent1", "Task 2", "medium", "")
	store.Transition("agent1", id2, "in_progress", "")
	time.Sleep(1100 * time.Millisecond)
	id3, _ := store.Add("agent1", "Task 3", "medium", "")
	store.Transition("agent1", id3, "done", "completed")

	// Update id1 to make it most recently updated
	time.Sleep(1100 * time.Millisecond)
	store.Edit("agent1", id1, "Updated task 1", "", "", false)

	// List with sort=updated should ignore status and sort purely by updated time (newest first)
	items, err := store.List("agent1", "", "", "", "updated")
	if err != nil {
		t.Fatalf("List with sort=updated: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	// Should be ordered by updated time only (newest first), not grouped by status
	if items[0].ID != id1 {
		t.Errorf("first item ID = %d (status=%s), want %d (most recently updated)", items[0].ID, items[0].Status, id1)
	}
	if items[1].ID != id3 {
		t.Errorf("second item ID = %d (status=%s), want %d", items[1].ID, items[1].Status, id3)
	}
	if items[2].ID != id2 {
		t.Errorf("third item ID = %d (status=%s), want %d", items[2].ID, items[2].Status, id2)
	}
}

func TestTodoSearchFTSStemming(t *testing.T) {
	// Verifies that FTS5 porter stemming matches morphological variants
	// (e.g. "running" matches "run", "deployments" matches "deploy").
	store := newTestTodoStore(t)

	store.Add("agent1", "Fix the running server process", "high", "")
	store.Add("agent1", "Review configuration settings carefully", "medium", "")
	store.Add("agent1", "Update the documented procedures", "low", "")

	// "run" should match "running" via stemming (step 1b: -ing removal)
	items, err := store.Search("agent1", "run")
	if err != nil {
		t.Fatalf("Search 'run': %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 match for 'run', got %d", len(items))
	} else if items[0].Text != "Fix the running server process" {
		t.Errorf("unexpected match: %q", items[0].Text)
	}

	// "configuring" should match "configuration" via stemming
	// (both stem to "configur")
	items, err = store.Search("agent1", "configuring")
	if err != nil {
		t.Fatalf("Search 'configuring': %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 match for 'configuring', got %d", len(items))
	}

	// "documenting" should match "documented" via stemming
	// (both stem to "document")
	items, err = store.Search("agent1", "documenting")
	if err != nil {
		t.Fatalf("Search 'documenting': %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 match for 'documenting', got %d", len(items))
	}
}

func TestTodoSearchFTSRanking(t *testing.T) {
	// Verifies that results with more term matches rank higher.
	store := newTestTodoStore(t)

	store.Add("agent1", "Update the server", "medium", "")
	store.Add("agent1", "Update the server configuration and server logs", "medium", "")

	// "server" appears twice in the second todo — it should rank higher.
	items, err := store.Search("agent1", "server")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(items))
	}
	if items[0].Text != "Update the server configuration and server logs" {
		t.Errorf("expected higher-ranked result first, got %q", items[0].Text)
	}
}

func TestTodoSearchFTSMultipleTerms(t *testing.T) {
	// Verifies that multiple search terms work as an implicit AND.
	store := newTestTodoStore(t)

	store.Add("agent1", "Fix the login bug", "high", "")
	store.Add("agent1", "Fix the payment processing", "medium", "")
	store.Add("agent1", "Review login page design", "low", "")

	// "fix login" should only match the item containing both terms
	items, err := store.Search("agent1", "fix login")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 match for 'fix login', got %d", len(items))
	}
	if items[0].Text != "Fix the login bug" {
		t.Errorf("unexpected match: %q", items[0].Text)
	}
}

func TestTodoSearchFTSEditUpdatesIndex(t *testing.T) {
	// Verifies that editing a todo's text updates the FTS index so the
	// old text no longer matches and the new text does.
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Buy groceries from the store", "medium", "")

	// Should find it by original text
	items, err := store.Search("agent1", "groceries")
	if err != nil {
		t.Fatalf("Search before edit: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 match before edit, got %d", len(items))
	}

	// Edit the text
	store.Edit("agent1", id, "Deploy the application to staging", "", "", false)

	// Old text should no longer match
	items, err = store.Search("agent1", "groceries")
	if err != nil {
		t.Fatalf("Search old text after edit: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 matches for old text after edit, got %d", len(items))
	}

	// New text should match
	items, err = store.Search("agent1", "staging")
	if err != nil {
		t.Fatalf("Search new text after edit: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 match for new text after edit, got %d", len(items))
	}
}

func TestTodoSearchFTSRemoveUpdatesIndex(t *testing.T) {
	// Verifies that removing a todo removes it from the FTS index.
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Investigate memory leak", "high", "")

	items, err := store.Search("agent1", "memory")
	if err != nil {
		t.Fatalf("Search before remove: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 match before remove, got %d", len(items))
	}

	store.Remove("agent1", id)

	items, err = store.Search("agent1", "memory")
	if err != nil {
		t.Fatalf("Search after remove: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 matches after remove, got %d", len(items))
	}
}

func TestTodoSearchFTSEmptyQuery(t *testing.T) {
	// Verifies that an empty query returns nil without error.
	store := newTestTodoStore(t)

	store.Add("agent1", "Some task", "medium", "")

	items, err := store.Search("agent1", "")
	if err != nil {
		t.Fatalf("Search empty: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 results for empty query, got %d", len(items))
	}

	items, err = store.Search("agent1", "   ")
	if err != nil {
		t.Fatalf("Search whitespace: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 results for whitespace query, got %d", len(items))
	}
}

func TestTodoSearchFTSSpecialCharacters(t *testing.T) {
	// Verifies that queries with special characters don't cause FTS5 syntax errors.
	store := newTestTodoStore(t)

	store.Add("agent1", "Fix bug #123 in the login page", "high", "")

	// These should not error, even if they don't match
	for _, q := range []string{"bug", `#123`, "bug #123", `"login"`, "OR AND NOT"} {
		_, err := store.Search("agent1", q)
		if err != nil {
			t.Errorf("Search(%q) errored: %v", q, err)
		}
	}
}

func TestTodoSearchFTSMigrationRebuild(t *testing.T) {
	// Verifies that opening a database with existing todos (but no FTS table)
	// correctly builds the FTS index from existing content.
	dbPath := filepath.Join(t.TempDir(), "migrate_test.db")

	// Create a store and add some todos, then close it.
	store, err := NewTodoStore(dbPath)
	if err != nil {
		t.Fatalf("NewTodoStore (first): %v", err)
	}
	store.Add("agent1", "Pre-existing task about servers", "high", "")
	store.Add("agent1", "Another old task about databases", "medium", "")
	store.Close()

	// Drop the FTS table and triggers to simulate a pre-FTS database.
	db, err := openAndExec(dbPath,
		"DROP TABLE IF EXISTS todos_fts",
		"DROP TRIGGER IF EXISTS todos_fts_ai",
		"DROP TRIGGER IF EXISTS todos_fts_ad",
		"DROP TRIGGER IF EXISTS todos_fts_au",
	)
	if err != nil {
		t.Fatalf("drop FTS: %v", err)
	}
	db.Close()

	// Re-open — should rebuild FTS from existing content.
	store, err = NewTodoStore(dbPath)
	if err != nil {
		t.Fatalf("NewTodoStore (second): %v", err)
	}
	t.Cleanup(func() { store.Close() })

	items, err := store.Search("agent1", "servers")
	if err != nil {
		t.Fatalf("Search after migration: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 match after migration rebuild, got %d", len(items))
	}

	items, err = store.Search("agent1", "databases")
	if err != nil {
		t.Fatalf("Search after migration: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 match for 'databases' after migration, got %d", len(items))
	}
}

// openAndExec opens a SQLite DB and executes statements, returning the db handle.
func openAndExec(path string, stmts ...string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
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
