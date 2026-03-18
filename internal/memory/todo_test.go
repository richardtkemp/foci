package memory

import (
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestTodoAddAndList(t *testing.T) {
	// Verifies that Add assigns a sequential ID and stores the text, priority, and default status, which are all retrievable via List.
	store := newTestTodoStore(t)

	id, err := store.Add("agent1", "Buy milk", "high", "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if id != 1 {
		t.Errorf("expected id 1, got %d", id)
	}

	items, err := store.List("agent1", "", nil, "", "", false, 0)
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
	// Verifies that an empty priority string is stored as "medium" by default.
	store := newTestTodoStore(t)

	store.Add("agent1", "Task with default priority", "", "")

	items, err := store.List("agent1", "", nil, "", "", false, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if items[0].Priority != "medium" {
		t.Errorf("priority = %q, want %q", items[0].Priority, "medium")
	}
}

func TestTodoComplete(t *testing.T) {
	// Verifies that Complete transitions a todo to "done", sets completed_at, and stores the close reason.
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Finish report", "medium", "")
	if err := store.Complete("agent1", id, "done and dusted"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	items, err := store.List("agent1", "done", nil, "", "", false, 0)
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
	// Verifies that Complete returns an error for a nonexistent todo ID.
	store := newTestTodoStore(t)

	err := store.Complete("agent1", 999, "reason")
	if err == nil {
		t.Error("expected error for nonexistent todo")
	}
}

func TestTodoRemove(t *testing.T) {
	// Verifies that Remove permanently deletes a todo so it no longer appears in List.
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Temp task", "low", "")
	if err := store.Remove("agent1", id); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	items, err := store.List("agent1", "", nil, "", "", false, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 items after remove, got %d", len(items))
	}
}

func TestTodoRemoveNotFound(t *testing.T) {
	// Verifies that Remove returns an error for a nonexistent todo ID.
	store := newTestTodoStore(t)

	err := store.Remove("agent1", 999)
	if err == nil {
		t.Error("expected error for nonexistent todo")
	}
}

func TestTodoAgentIsolation(t *testing.T) {
	// Verifies that different agents have completely separate todo lists and cannot see each other's items.
	store := newTestTodoStore(t)

	store.Add("agent1", "Agent 1 task", "high", "")
	store.Add("agent2", "Agent 2 task", "low", "")

	items1, _ := store.List("agent1", "", nil, "", "", false, 0)
	items2, _ := store.List("agent2", "", nil, "", "", false, 0)

	if len(items1) != 1 || items1[0].Text != "Agent 1 task" {
		t.Errorf("agent1 items = %v, want 1 item", items1)
	}
	if len(items2) != 1 || items2[0].Text != "Agent 2 task" {
		t.Errorf("agent2 items = %v, want 1 item", items2)
	}
}

func TestTodoPerAgentIDs(t *testing.T) {
	// Verifies that each agent has an independent ID sequence starting from 1, and that the same ID resolves to different items for different agents.
	store := newTestTodoStore(t)
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
	// Verifies that List correctly filters by status ("open", "done") and that no filter returns all items.
	store := newTestTodoStore(t)

	id1, _ := store.Add("agent1", "Open task", "medium", "")
	id2, _ := store.Add("agent1", "Done task", "medium", "")
	_ = id1
	store.Complete("agent1", id2, "finished")

	open, _ := store.List("agent1", "open", nil, "", "", false, 0)
	done, _ := store.List("agent1", "done", nil, "", "", false, 0)
	all, _ := store.List("agent1", "", nil, "", "", false, 0)

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

func TestTodoListFilterActive(t *testing.T) {
	// Verifies the "active" status filter excludes done and dropped items, returning only open and started todos.
	store := newTestTodoStore(t)

	store.Add("agent1", "Open task", "medium", "")
	id2, _ := store.Add("agent1", "Done task", "medium", "")
	id3, _ := store.Add("agent1", "Dropped task", "medium", "")
	id4, _ := store.Add("agent1", "WIP task", "medium", "")

	store.Complete("agent1", id2, "finished")
	store.Transition("agent1", id3, "dropped", "not needed")
	store.Transition("agent1", id4, "started", "")

	active, err := store.List("agent1", "active", nil, "", "", false, 0)
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
	// Verifies that List filters by priority, that combined priority+status filters work, and that no filter returns all items.
	store := newTestTodoStore(t)

	store.Add("agent1", "High task 1", "high", "")
	store.Add("agent1", "Medium task", "medium", "")
	store.Add("agent1", "High task 2", "high", "")
	store.Add("agent1", "Low task", "low", "")

	// Filter by priority
	items, err := store.List("agent1", "", nil, "high", "", false, 0)
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
	open, err := store.List("agent1", "open", nil, "high", "", false, 0)
	if err != nil {
		t.Fatalf("List with priority+status: %v", err)
	}
	if len(open) != 1 {
		t.Errorf("expected 1 open high item, got %d", len(open))
	}

	// No priority filter shows all
	all, err := store.List("agent1", "", nil, "", "", false, 0)
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 4 {
		t.Errorf("expected 4 total items, got %d", len(all))
	}
}

func TestTodoListLimit(t *testing.T) {
	// Verifies that List respects the limit parameter, returning at most N items,
	// and that limit=0 returns all items.
	store := newTestTodoStore(t)

	store.Add("agent1", "Task 1", "high", "")
	store.Add("agent1", "Task 2", "medium", "")
	store.Add("agent1", "Task 3", "low", "")
	store.Add("agent1", "Task 4", "medium", "")

	items, err := store.List("agent1", "", nil, "", "", false, 2)
	if err != nil {
		t.Fatalf("List with limit: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("expected 2 items with limit=2, got %d", len(items))
	}

	all, err := store.List("agent1", "", nil, "", "", false, 0)
	if err != nil {
		t.Fatalf("List without limit: %v", err)
	}
	if len(all) != 4 {
		t.Errorf("expected 4 items with limit=0, got %d", len(all))
	}
}

func TestTodoPriorityOrdering(t *testing.T) {
	// Verifies that List orders items by priority (high → medium → low) when no explicit sort is specified.
	store := newTestTodoStore(t)

	store.Add("agent1", "Low task", "low", "")
	store.Add("agent1", "High task", "high", "")
	store.Add("agent1", "Medium task", "medium", "")

	items, _ := store.List("agent1", "open", nil, "", "", false, 0)
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
	// Verifies that one agent cannot complete a todo owned by another agent.
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Agent 1 only", "medium", "")

	// Agent 2 should not be able to complete agent 1's todo
	err := store.Complete("agent2", id, "reason")
	if err == nil {
		t.Error("expected error when completing another agent's todo")
	}
}

func TestTodoCrossAgentRemove(t *testing.T) {
	// Verifies that one agent cannot remove a todo owned by another agent.
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Agent 1 only", "medium", "")

	// Agent 2 should not be able to remove agent 1's todo
	err := store.Remove("agent2", id)
	if err == nil {
		t.Error("expected error when removing another agent's todo")
	}
}

func TestTodoSearchRequiresIndex(t *testing.T) {
	// Verifies that Search returns an error when no search index is configured.
	store := newTestTodoStore(t)
	store.Add("agent1", "Some task", "medium", "")

	_, err := store.Search("agent1", "task", nil)
	if err == nil {
		t.Error("expected error when searching without a search index")
	}
}

func TestTodoTags(t *testing.T) {
	// Verifies that List can filter by tag, that combined tag+status filters work, and that no tag filter returns all items.
	store := newTestTodoStore(t)

	store.Add("agent1", "Check email", "medium", "background")
	store.Add("agent1", "Review PRs", "high", "background,daily")
	store.Add("agent1", "Regular task", "low", "")

	// Filter by tag
	items, err := store.List("agent1", "", []string{"background"}, "", "", false, 0)
	if err != nil {
		t.Fatalf("List with tag: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("expected 2 background items, got %d", len(items))
	}

	// Filter by tag + status
	items, err = store.List("agent1", "open", []string{"daily"}, "", "", false, 0)
	if err != nil {
		t.Fatalf("List with tag+status: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 daily open item, got %d", len(items))
	}

	// No tag filter shows all
	items, err = store.List("agent1", "", nil, "", "", false, 0)
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(items) != 3 {
		t.Errorf("expected 3 total items, got %d", len(items))
	}
}

func TestTodoCountOpenByTag(t *testing.T) {
	// Verifies that CountOpenByTag returns the count of open (non-completed) todos with a given tag, scoped to a single agent.
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

func TestTodoListNegatedTag(t *testing.T) {
	// Verifies that List with a "!"-prefixed tag filter excludes items with that tag
	// while including items with other tags or no tags.
	store := newTestTodoStore(t)

	store.Add("agent1", "Check email", "medium", "background")
	store.Add("agent1", "Review PRs", "high", "background,daily")
	store.Add("agent1", "Regular task", "low", "")
	store.Add("agent1", "Work task", "medium", "work")

	items, err := store.List("agent1", "", []string{"!background"}, "", "", false, 0)
	if err != nil {
		t.Fatalf("List with negated tag: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("expected 2 non-background items, got %d", len(items))
	}
	for _, item := range items {
		if item.Tags == "background" || item.Tags == "background,daily" {
			t.Errorf("should not include item with background tag: %q", item.Tags)
		}
	}
}

func TestTodoListNegatedPriority(t *testing.T) {
	// Verifies that List with a "!"-prefixed priority filter excludes items with that
	// priority while returning all others.
	store := newTestTodoStore(t)

	store.Add("agent1", "High task", "high", "")
	store.Add("agent1", "Medium task", "medium", "")
	store.Add("agent1", "Low task", "low", "")

	items, err := store.List("agent1", "", nil, "!low", "", false, 0)
	if err != nil {
		t.Fatalf("List with negated priority: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("expected 2 non-low items, got %d", len(items))
	}
	for _, item := range items {
		if item.Priority == "low" {
			t.Errorf("should not include low-priority item: %q", item.Text)
		}
	}
}

func TestMatchesTagFilters(t *testing.T) {
	// Verifies that matchesTagFilters with single and multiple filters correctly
	// applies AND logic: all filters must pass for the item to match.
	tests := []struct {
		tags    string
		filters []string
		want    bool
	}{
		{"background", []string{"!background"}, false},
		{"work", []string{"!background"}, true},
		{"background,daily", []string{"!background"}, false},
		{"", []string{"!background"}, true},
		{"daily", []string{"!background"}, true},
		{"background", []string{"background"}, true},
		{"work", []string{"background"}, false},
		{"", nil, true},
		// Multi-tag AND logic
		{"foci,bug", []string{"foci", "bug"}, true},
		{"foci,feature", []string{"foci", "bug"}, false},
		{"bug", []string{"foci", "bug"}, false},
		{"foci,bug,daily", []string{"foci", "bug"}, true},
		// Mixed positive + negated
		{"work,daily", []string{"work", "!background"}, true},
		{"work,background", []string{"work", "!background"}, false},
		// Multiple negated
		{"daily", []string{"!background", "!work"}, true},
		{"background", []string{"!background", "!work"}, false},
		{"work", []string{"!background", "!work"}, false},
	}
	for _, tt := range tests {
		got := matchesTagFilters(tt.tags, tt.filters)
		if got != tt.want {
			t.Errorf("matchesTagFilters(%q, %v) = %v, want %v", tt.tags, tt.filters, got, tt.want)
		}
	}
}

func TestTodoListMultipleTags(t *testing.T) {
	// Verifies that List with multiple tag filters uses AND logic: only items
	// matching all specified tags are returned.
	store := newTestTodoStore(t)

	store.Add("agent1", "Both tags", "medium", "foci,bug")
	store.Add("agent1", "Only foci", "medium", "foci,feature")
	store.Add("agent1", "Only bug", "medium", "other,bug")
	store.Add("agent1", "Neither", "medium", "other")

	items, err := store.List("agent1", "", []string{"foci", "bug"}, "", "", false, 0)
	if err != nil {
		t.Fatalf("List with multiple tags: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item with both tags, got %d", len(items))
	}
	if items[0].Text != "Both tags" {
		t.Errorf("expected 'Both tags', got %q", items[0].Text)
	}
}

func TestTodoListMixedNegatedTags(t *testing.T) {
	// Verifies that List with a mix of positive and negated tag filters works:
	// item must have the positive tag AND must not have the negated tag.
	store := newTestTodoStore(t)

	store.Add("agent1", "Work only", "medium", "work")
	store.Add("agent1", "Work+BG", "medium", "work,background")
	store.Add("agent1", "BG only", "medium", "background")
	store.Add("agent1", "Neither", "medium", "")

	items, err := store.List("agent1", "", []string{"work", "!background"}, "", "", false, 0)
	if err != nil {
		t.Fatalf("List with mixed tags: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Text != "Work only" {
		t.Errorf("expected 'Work only', got %q", items[0].Text)
	}
}

func TestMatchesPriorityFilterNegated(t *testing.T) {
	// Verifies that matchesPriorityFilter with a "!"-prefixed filter correctly negates:
	// items with that priority are rejected, others are accepted.
	tests := []struct {
		priority string
		filter   string
		want     bool
	}{
		{"low", "!low", false},
		{"high", "!low", true},
		{"medium", "!low", true},
		{"low", "low", true},
		{"high", "low", false},
		{"high", "", true},
	}
	for _, tt := range tests {
		got := matchesPriorityFilter(tt.priority, tt.filter)
		if got != tt.want {
			t.Errorf("matchesPriorityFilter(%q, %q) = %v, want %v", tt.priority, tt.filter, got, tt.want)
		}
	}
}

func TestFormatTags(t *testing.T) {
	// Verifies that FormatTags normalises whitespace, formats tags as " {tag1,tag2}", and returns empty string for no tags.
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
	// Verifies that Edit updates only the specified fields (text here) while leaving unspecified fields unchanged.
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Original text", "high", "work")
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
	// Verifies that Edit can change only the priority while leaving the text unchanged.
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
	// Verifies that Edit replaces tags when setTags is true, and clears them when both setTags is true and the new tag is empty.
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Tagged task", "medium", "old")
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
	// Verifies that Edit can simultaneously update text, priority, and tags in a single call.
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
	// Verifies that Edit returns an error for a nonexistent todo ID.
	store := newTestTodoStore(t)

	_, err := store.Edit("agent1", 999, "text", "", "", false)
	if err == nil {
		t.Error("expected error for nonexistent todo")
	}
}

func TestTodoCrossAgentEdit(t *testing.T) {
	// Verifies that one agent cannot edit a todo owned by another agent.
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Agent 1 only", "medium", "")
	_, err := store.Edit("agent2", id, "hacked", "", "", false)
	if err == nil {
		t.Error("expected error when editing another agent's todo")
	}
}

func TestTodoEditNothingToUpdate(t *testing.T) {
	// Verifies that Edit returns an error when no fields are specified to update.
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Task", "medium", "")

	_, err := store.Edit("agent1", id, "", "", "", false)
	if err == nil {
		t.Error("expected error when nothing to update")
	}
}

func TestTodoUpdatedAtOnAdd(t *testing.T) {
	// Verifies that created_at and updated_at are both set to the current time when a todo is first added.
	store := newTestTodoStore(t)

	before := time.Now().UTC().Truncate(time.Second)
	id, _ := store.Add("agent1", "Task", "medium", "")
	after := time.Now().UTC().Truncate(time.Second).Add(time.Second)

	items, _ := store.List("agent1", "", nil, "", "", false, 0)
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
	// Verifies that updated_at advances after an Edit call, proving the timestamp changes on modification.
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Original", "medium", "")
	items, _ := store.List("agent1", "", nil, "", "", false, 0)
	originalUpdatedAt := items[0].UpdatedAt

	_, err := store.Edit("agent1", id, "Updated", "", "", false)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}

	items, _ = store.List("agent1", "", nil, "", "", false, 0)
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
	// Verifies that updated_at and completed_at are both set when a todo is completed.
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Task", "medium", "")
	items, _ := store.List("agent1", "", nil, "", "", false, 0)
	originalUpdatedAt := items[0].UpdatedAt

	err := store.Complete("agent1", id, "done")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	items, _ = store.List("agent1", "done", nil, "", "", false, 0)
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

func TestTodoTransitionStarted(t *testing.T) {
	// Verifies the full status lifecycle: open → started → done → started, checking that completed_at is set/cleared appropriately.
	store := newTestTodoStore(t)

	id, _ := store.Add("agent1", "Working on it", "high", "")

	// Transition to started
	if err := store.Transition("agent1", id, "started", ""); err != nil {
		t.Fatalf("Transition to started: %v", err)
	}

	item, err := store.Get("agent1", id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if item.Status != "started" {
		t.Errorf("status = %q, want started", item.Status)
	}
	if item.CompletedAt != nil {
		t.Error("completed_at should be nil for started")
	}
	if item.CloseReason != "" {
		t.Errorf("close_reason should be empty for started, got %q", item.CloseReason)
	}

	// Transition from started to done
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

	// Transition from done back to started
	if err := store.Transition("agent1", id, "started", ""); err != nil {
		t.Fatalf("Transition back to started: %v", err)
	}
	item, _ = store.Get("agent1", id)
	if item.Status != "started" {
		t.Errorf("status = %q, want started", item.Status)
	}
	if item.CompletedAt != nil {
		t.Error("completed_at should be nil after reverting to started")
	}
}

func TestTodoSortOrderStarted(t *testing.T) {
	// Verifies that started todos are sorted before open todos in the default list order.
	store := newTestTodoStore(t)

	store.Add("agent1", "Open task", "high", "")
	id2, _ := store.Add("agent1", "Started task", "high", "")
	store.Add("agent1", "Another open task", "high", "")

	store.Transition("agent1", id2, "started", "")

	items, err := store.List("agent1", "", nil, "", "", false, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	if items[0].Status != "started" {
		t.Errorf("first item status = %q, want started", items[0].Status)
	}
	if items[1].Status != "open" || items[2].Status != "open" {
		t.Errorf("remaining items should be open, got %q and %q", items[1].Status, items[2].Status)
	}
}

func TestTodoSortByCreated(t *testing.T) {
	// Verifies that sort=created orders todos newest-first by default, and
	// reverse=true flips to oldest-first.
	store := newTestTodoStore(t)

	id1, _ := store.Add("agent1", "First task", "high", "")
	id2, _ := store.Add("agent1", "Second task", "high", "")
	id3, _ := store.Add("agent1", "Third task", "high", "")

	// Default: newest first
	items, err := store.List("agent1", "", nil, "", "created", false, 0)
	if err != nil {
		t.Fatalf("List with sort=created: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	if items[0].ID != id3 {
		t.Errorf("first item ID = %d, want %d (newest)", items[0].ID, id3)
	}
	if items[1].ID != id2 {
		t.Errorf("second item ID = %d, want %d", items[1].ID, id2)
	}
	if items[2].ID != id1 {
		t.Errorf("third item ID = %d, want %d (oldest)", items[2].ID, id1)
	}

	// reverse=true: oldest first
	items, err = store.List("agent1", "", nil, "", "created", true, 0)
	if err != nil {
		t.Fatalf("List with sort=created reverse: %v", err)
	}
	if items[0].ID != id1 {
		t.Errorf("reversed first item ID = %d, want %d (oldest)", items[0].ID, id1)
	}
	if items[2].ID != id3 {
		t.Errorf("reversed third item ID = %d, want %d (newest)", items[2].ID, id3)
	}
}

func TestTodoSortByUpdated(t *testing.T) {
	// Verifies that sort=updated orders todos newest-first by updated_at, confirming a later edit promotes an older item to the top.
	store := newTestTodoStore(t)

	id1, _ := store.Add("agent1", "Task 1", "medium", "")
	id2, _ := store.Add("agent1", "Task 2", "medium", "")
	id3, _ := store.Add("agent1", "Task 3", "medium", "")

	store.Edit("agent1", id1, "Updated task 1", "", "", false)

	items, err := store.List("agent1", "", nil, "", "updated", false, 0)
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
	// Verifies that both an empty sort parameter and an explicit "priority" sort produce the same high→medium→low ordering.
	store := newTestTodoStore(t)

	store.Add("agent1", "Low task", "low", "")
	store.Add("agent1", "High task", "high", "")
	store.Add("agent1", "Medium task", "medium", "")

	// Empty sort parameter should use priority sort
	items, err := store.List("agent1", "open", nil, "", "", false, 0)
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
	items2, err := store.List("agent1", "open", nil, "", "priority", false, 0)
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
	// Verifies that sort=created orders purely by creation timestamp regardless of status, so items of mixed statuses interleave chronologically (newest first by default).
	store := newTestTodoStore(t)

	// Create items with different statuses at different times
	id1, _ := store.Add("agent1", "First task", "high", "")
	id2, _ := store.Add("agent1", "Second task", "high", "")
	store.Transition("agent1", id2, "started", "")
	id3, _ := store.Add("agent1", "Third task", "high", "")
	store.Transition("agent1", id3, "done", "completed")
	id4, _ := store.Add("agent1", "Fourth task", "high", "")

	// sort=created, default direction = newest first
	items, err := store.List("agent1", "", nil, "", "created", false, 0)
	if err != nil {
		t.Fatalf("List with sort=created: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("expected 4 items, got %d", len(items))
	}
	// Should be ordered by creation time only, not grouped by status
	if items[0].ID != id4 {
		t.Errorf("first item ID = %d (status=%s), want %d (newest)", items[0].ID, items[0].Status, id4)
	}
	if items[1].ID != id3 {
		t.Errorf("second item ID = %d (status=%s), want %d", items[1].ID, items[1].Status, id3)
	}
	if items[2].ID != id2 {
		t.Errorf("third item ID = %d (status=%s), want %d", items[2].ID, items[2].Status, id2)
	}
	if items[3].ID != id1 {
		t.Errorf("fourth item ID = %d (status=%s), want %d (oldest)", items[3].ID, items[3].Status, id1)
	}
}

func TestTodoSortByUpdatedIgnoresStatus(t *testing.T) {
	// Verifies that sort=updated orders purely by updated_at regardless of status, so items of mixed statuses interleave by recency.
	store := newTestTodoStore(t)

	// Create items with different statuses and update at different times
	id1, _ := store.Add("agent1", "Task 1", "medium", "")
	id2, _ := store.Add("agent1", "Task 2", "medium", "")
	store.Transition("agent1", id2, "started", "")
	id3, _ := store.Add("agent1", "Task 3", "medium", "")
	store.Transition("agent1", id3, "done", "completed")

	// Update id1 to make it most recently updated
	store.Edit("agent1", id1, "Updated task 1", "", "", false)

	// List with sort=updated should ignore status and sort purely by updated time (newest first)
	items, err := store.List("agent1", "", nil, "", "updated", false, 0)
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
