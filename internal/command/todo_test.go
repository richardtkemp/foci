package command

import (
	"context"
	"path/filepath"
	"testing"

	"foci/internal/config"
	"foci/internal/memory"
)

// newTestTodoStore creates a real *memory.TodoStore backed by a temp dir.
func newTestTodoStore(t *testing.T) *memory.TodoStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "todo.db")
	store, err := memory.NewTodoStore(dbPath)
	if err != nil {
		t.Fatalf("NewTodoStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newTestCC creates a minimal CommandContext with the given TodoStore.
func newTestCC(store *memory.TodoStore) CommandContext {
	return CommandContext{
		AgentConfig: config.AgentConfig{ID: "test"},
		TodoStore:   store,
	}
}

const testAgent = "test"

// --- Parser Tests ---

// TestParseTodoArgs verifies the argument parser handles all token combinations
// correctly, including defaults, status/sort/reverse tokens, t:/p: prefixes,
// subcommand detection, and the done-ambiguity rule.
func TestParseTodoArgs(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want todoArgs
	}{
		{
			name: "empty defaults",
			raw:  "",
			want: todoArgs{status: "active", sort: "priority", limit: 15},
		},
		{
			name: "bare number sets limit",
			raw:  "30",
			want: todoArgs{status: "active", sort: "priority", limit: 30},
		},
		{
			name: "all status",
			raw:  "all",
			want: todoArgs{status: "", sort: "priority", limit: 15},
		},
		{
			name: "done status filter",
			raw:  "done",
			want: todoArgs{status: "done", sort: "priority", limit: 15},
		},
		{
			name: "closed maps to done",
			raw:  "closed",
			want: todoArgs{status: "done", sort: "priority", limit: 15},
		},
		{
			name: "open status",
			raw:  "open",
			want: todoArgs{status: "open", sort: "priority", limit: 15},
		},
		{
			name: "dropped status",
			raw:  "dropped",
			want: todoArgs{status: "dropped", sort: "priority", limit: 15},
		},
		{
			name: "in_progress status",
			raw:  "in_progress",
			want: todoArgs{status: "in_progress", sort: "priority", limit: 15},
		},
		{
			name: "created sort",
			raw:  "created",
			want: todoArgs{status: "active", sort: "created", limit: 15},
		},
		{
			name: "updated sort",
			raw:  "updated",
			want: todoArgs{status: "active", sort: "updated", limit: 15},
		},
		{
			name: "reverse modifier",
			raw:  "reverse",
			want: todoArgs{status: "active", sort: "priority", limit: 15, reverse: true},
		},
		{
			name: "tag filter",
			raw:  "t:shopping",
			want: todoArgs{status: "active", sort: "priority", limit: 15, tag: "shopping", setTag: true},
		},
		{
			name: "priority filter",
			raw:  "p:high",
			want: todoArgs{status: "active", sort: "priority", limit: 15, priority: "high"},
		},
		{
			name: "combined list options",
			raw:  "updated reverse all 5",
			want: todoArgs{status: "", sort: "updated", limit: 5, reverse: true},
		},
		{
			name: "combined tag and priority",
			raw:  "created all p:high t:work",
			want: todoArgs{status: "", sort: "created", limit: 15, priority: "high", tag: "work", setTag: true},
		},
		// Subcommands
		{
			name: "new basic",
			raw:  "new buy milk",
			want: todoArgs{subcommand: "new", status: "active", sort: "priority", limit: 15, text: "buy milk"},
		},
		{
			name: "new with tag and priority",
			raw:  "new t:shopping p:high bread",
			want: todoArgs{subcommand: "new", status: "active", sort: "priority", limit: 15, text: "bread", tag: "shopping", setTag: true, priority: "high"},
		},
		{
			name: "done transition single ID",
			raw:  "done 5",
			want: todoArgs{subcommand: "done", status: "active", sort: "priority", limit: 15, ids: []int64{5}},
		},
		{
			name: "done transition batch",
			raw:  "done 3 5 7",
			want: todoArgs{subcommand: "done", status: "active", sort: "priority", limit: 15, ids: []int64{3, 5, 7}},
		},
		{
			name: "done with non-integer is list filter",
			raw:  "done t:shopping",
			want: todoArgs{status: "done", sort: "priority", limit: 15, tag: "shopping", setTag: true},
		},
		{
			name: "start",
			raw:  "start 5",
			want: todoArgs{subcommand: "start", status: "active", sort: "priority", limit: 15, ids: []int64{5}},
		},
		{
			name: "drop",
			raw:  "drop 5",
			want: todoArgs{subcommand: "drop", status: "active", sort: "priority", limit: 15, ids: []int64{5}},
		},
		{
			name: "reopen",
			raw:  "reopen 5",
			want: todoArgs{subcommand: "reopen", status: "active", sort: "priority", limit: 15, ids: []int64{5}},
		},
		{
			name: "edit with priority",
			raw:  "edit 5 p:high",
			want: todoArgs{subcommand: "edit", status: "active", sort: "priority", limit: 15, ids: []int64{5}, priority: "high"},
		},
		{
			name: "edit with tag and text",
			raw:  "edit 5 t:urgent new text here",
			want: todoArgs{subcommand: "edit", status: "active", sort: "priority", limit: 15, ids: []int64{5}, tag: "urgent", setTag: true, text: "new text here"},
		},
		{
			name: "show",
			raw:  "show 5",
			want: todoArgs{subcommand: "show", status: "active", sort: "priority", limit: 15, ids: []int64{5}},
		},
		{
			name: "search",
			raw:  "search buy milk",
			want: todoArgs{subcommand: "search", status: "active", sort: "priority", limit: 15, text: "buy milk"},
		},
		{
			name: "rm",
			raw:  "rm 5",
			want: todoArgs{subcommand: "rm", status: "active", sort: "priority", limit: 15, ids: []int64{5}},
		},
		{
			name: "stats",
			raw:  "stats",
			want: todoArgs{subcommand: "stats", status: "active", sort: "priority", limit: 15},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTodoArgs(tt.raw)
			assertTodoArgs(t, tt.want, got)
		})
	}
}

func assertTodoArgs(t *testing.T, want, got todoArgs) {
	t.Helper()
	if got.subcommand != want.subcommand {
		t.Errorf("subcommand: got %q, want %q", got.subcommand, want.subcommand)
	}
	if got.status != want.status {
		t.Errorf("status: got %q, want %q", got.status, want.status)
	}
	if got.sort != want.sort {
		t.Errorf("sort: got %q, want %q", got.sort, want.sort)
	}
	if got.reverse != want.reverse {
		t.Errorf("reverse: got %v, want %v", got.reverse, want.reverse)
	}
	if got.limit != want.limit {
		t.Errorf("limit: got %d, want %d", got.limit, want.limit)
	}
	if got.tag != want.tag {
		t.Errorf("tag: got %q, want %q", got.tag, want.tag)
	}
	if got.setTag != want.setTag {
		t.Errorf("setTag: got %v, want %v", got.setTag, want.setTag)
	}
	if got.priority != want.priority {
		t.Errorf("priority: got %q, want %q", got.priority, want.priority)
	}
	if got.text != want.text {
		t.Errorf("text: got %q, want %q", got.text, want.text)
	}
	if len(got.ids) != len(want.ids) {
		t.Errorf("ids: got %v, want %v", got.ids, want.ids)
	} else {
		for i := range want.ids {
			if got.ids[i] != want.ids[i] {
				t.Errorf("ids[%d]: got %d, want %d", i, got.ids[i], want.ids[i])
			}
		}
	}
}

// --- Integration Tests ---

// TestTodoListEmpty verifies that listing with no items returns an appropriate message.
func TestTodoListEmpty(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	resp, err := cmd.Execute(context.Background(), Request{Args: ""}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "No active todos." {
		t.Errorf("got %q, want %q", resp.Text, "No active todos.")
	}
}

// TestTodoListWithItems verifies that listing returns items in the expected format.
func TestTodoListWithItems(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "buy milk", "high", "")
	store.Add(testAgent, "buy bread", "low", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: ""}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "#1") || !contains(resp.Text, "#2") {
		t.Errorf("expected both items in output: %s", resp.Text)
	}
	if !contains(resp.Text, "Todos (2)") {
		t.Errorf("expected header with count: %s", resp.Text)
	}
}

// TestTodoListWithLimit verifies the limit argument caps output.
func TestTodoListWithLimit(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	for i := 0; i < 5; i++ {
		store.Add(testAgent, "item", "medium", "")
	}

	resp, err := cmd.Execute(context.Background(), Request{Args: "2"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "Todos (2)") {
		t.Errorf("expected 2 items: %s", resp.Text)
	}
}

// TestTodoListWithTagFilter verifies the t:TAG filter narrows results.
func TestTodoListWithTagFilter(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "tagged item", "medium", "shopping")
	store.Add(testAgent, "untagged item", "medium", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "t:shopping"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "tagged item") {
		t.Errorf("expected tagged item: %s", resp.Text)
	}
	if contains(resp.Text, "untagged item") {
		t.Errorf("should not contain untagged item: %s", resp.Text)
	}
}

// TestTodoNew verifies creating a new todo item.
func TestTodoNew(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	resp, err := cmd.Execute(context.Background(), Request{Args: "new buy milk"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "Added #1") {
		t.Errorf("expected confirmation: %s", resp.Text)
	}
	if !contains(resp.Text, "medium") {
		t.Errorf("expected default priority: %s", resp.Text)
	}
}

// TestTodoNewWithTagAndPriority verifies creating with explicit tag and priority.
func TestTodoNewWithTagAndPriority(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	resp, err := cmd.Execute(context.Background(), Request{Args: "new t:shopping p:high bread"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "Added #1") || !contains(resp.Text, "high") {
		t.Errorf("expected high priority confirmation: %s", resp.Text)
	}
	item, _ := store.Get(testAgent, 1)
	if item.Tags != "shopping" {
		t.Errorf("tag: got %q, want %q", item.Tags, "shopping")
	}
}

// TestTodoNewEmpty verifies creating with no text shows usage.
func TestTodoNewEmpty(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	resp, err := cmd.Execute(context.Background(), Request{Args: "new"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "Usage") {
		t.Errorf("expected usage message: %s", resp.Text)
	}
}

// TestTodoDoneTransition verifies completing a todo by ID.
func TestTodoDoneTransition(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "buy milk", "medium", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "done 1"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "#1") && !contains(resp.Text, "done") {
		t.Errorf("expected done confirmation: %s", resp.Text)
	}
	item, _ := store.Get(testAgent, 1)
	if item.Status != "done" {
		t.Errorf("status: got %q, want %q", item.Status, "done")
	}
}

// TestTodoBatchTransition verifies completing multiple todos at once.
func TestTodoBatchTransition(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "item 1", "medium", "")
	store.Add(testAgent, "item 2", "medium", "")
	store.Add(testAgent, "item 3", "medium", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "done 1 3"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "#1") || !contains(resp.Text, "#3") {
		t.Errorf("expected both IDs in response: %s", resp.Text)
	}
	i1, _ := store.Get(testAgent, 1)
	i3, _ := store.Get(testAgent, 3)
	if i1.Status != "done" || i3.Status != "done" {
		t.Error("both items should be done")
	}
	i2, _ := store.Get(testAgent, 2)
	if i2.Status != "open" {
		t.Error("item 2 should still be open")
	}
}

// TestTodoStartTransition verifies starting a todo.
func TestTodoStartTransition(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "item", "medium", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "start 1"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "in_progress") {
		t.Errorf("expected in_progress: %s", resp.Text)
	}
	item, _ := store.Get(testAgent, 1)
	if item.Status != "in_progress" {
		t.Errorf("status: got %q, want %q", item.Status, "in_progress")
	}
}

// TestTodoDropTransition verifies dropping a todo.
func TestTodoDropTransition(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "item", "medium", "")

	_, err := cmd.Execute(context.Background(), Request{Args: "drop 1"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	item, _ := store.Get(testAgent, 1)
	if item.Status != "dropped" {
		t.Errorf("status: got %q, want %q", item.Status, "dropped")
	}
}

// TestTodoReopenTransition verifies reopening a done todo.
func TestTodoReopenTransition(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "item", "medium", "")
	store.Transition(testAgent, 1, "done", "finished")

	_, err := cmd.Execute(context.Background(), Request{Args: "reopen 1"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	item, _ := store.Get(testAgent, 1)
	if item.Status != "open" {
		t.Errorf("status: got %q, want %q", item.Status, "open")
	}
}

// TestTodoEdit verifies editing priority, tag, and text.
func TestTodoEdit(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "original text", "medium", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "edit 1 p:high t:urgent new text"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "#1 updated") {
		t.Errorf("expected update confirmation: %s", resp.Text)
	}
	item, _ := store.Get(testAgent, 1)
	if item.Priority != "high" {
		t.Errorf("priority: got %q, want %q", item.Priority, "high")
	}
	if item.Tags != "urgent" {
		t.Errorf("tags: got %q, want %q", item.Tags, "urgent")
	}
	if item.Text != "new text" {
		t.Errorf("text: got %q, want %q", item.Text, "new text")
	}
}

// TestTodoShow verifies the detail view of a single todo.
func TestTodoShow(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "detail item", "high", "work")

	resp, err := cmd.Execute(context.Background(), Request{Args: "show 1"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "#1 detail item") {
		t.Errorf("expected item detail: %s", resp.Text)
	}
	if !contains(resp.Text, "Status:") || !contains(resp.Text, "Priority:") {
		t.Errorf("expected field labels: %s", resp.Text)
	}
}

// TestTodoRm verifies hard-deleting a todo.
func TestTodoRm(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "doomed", "medium", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "rm 1"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "#1 removed") {
		t.Errorf("expected removal confirmation: %s", resp.Text)
	}
	_, getErr := store.Get(testAgent, 1)
	if getErr == nil {
		t.Error("expected item to be deleted")
	}
}

// TestTodoStats verifies the stats subcommand renders tables and filters tags
// to active items by default, with "stats all" showing all statuses.
func TestTodoStats(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "open item", "medium", "work")
	store.Add(testAgent, "done item", "high", "work")
	store.Transition(testAgent, 2, "done", "finished")
	store.Add(testAgent, "dropped item", "low", "personal")
	store.Transition(testAgent, 3, "dropped", "nah")

	// Default: tag counts are active-only.
	resp, err := cmd.Execute(context.Background(), Request{Args: "stats"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "3 total") {
		t.Errorf("expected total 3: %s", resp.Text)
	}
	if !contains(resp.Text, "open") || !contains(resp.Text, "done") || !contains(resp.Text, "dropped") {
		t.Errorf("expected all statuses: %s", resp.Text)
	}
	if !contains(resp.Text, "work") {
		t.Errorf("expected work tag: %s", resp.Text)
	}
	if contains(resp.Text, "personal") {
		t.Errorf("expected personal tag excluded (dropped): %s", resp.Text)
	}

	// "stats all" includes tags from all statuses.
	resp, err = cmd.Execute(context.Background(), Request{Args: "stats all"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "work") || !contains(resp.Text, "personal") {
		t.Errorf("expected all tags with 'all' filter: %s", resp.Text)
	}
}

// TestTodoNilStore verifies graceful handling when no store is configured.
func TestTodoNilStore(t *testing.T) {
	cc := newTestCC(nil)
	cmd := TodoCommand()

	resp, err := cmd.Execute(context.Background(), Request{Args: ""}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "Todo store not configured." {
		t.Errorf("got %q, want %q", resp.Text, "Todo store not configured.")
	}
}

// TestTodoTransitionNotFound verifies error handling for invalid IDs.
func TestTodoTransitionNotFound(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	resp, err := cmd.Execute(context.Background(), Request{Args: "done 999"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "error") {
		t.Errorf("expected error for missing ID: %s", resp.Text)
	}
}

// TestTodoTransitionNoIDs verifies usage message when no IDs given.
func TestTodoTransitionNoIDs(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	resp, err := cmd.Execute(context.Background(), Request{Args: "start"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "Usage") {
		t.Errorf("expected usage: %s", resp.Text)
	}
}

// TestTodoDoneAmbiguity verifies the done ambiguity rule: "done" alone is a
// list filter, "done 5" is a transition, "done t:shopping" is a list filter.
func TestTodoDoneAmbiguity(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "item 1", "medium", "shopping")
	store.Transition(testAgent, 1, "done", "bought")

	// "done" alone → list done items
	resp, err := cmd.Execute(context.Background(), Request{Args: "done"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "item 1") {
		t.Errorf("expected done item in list: %s", resp.Text)
	}

	// "done 1" on an open item → transition
	store.Add(testAgent, "item 2", "medium", "")
	resp, err = cmd.Execute(context.Background(), Request{Args: "done 2"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "#2") {
		t.Errorf("expected transition confirmation: %s", resp.Text)
	}
	item, _ := store.Get(testAgent, 2)
	if item.Status != "done" {
		t.Errorf("status: got %q, want %q", item.Status, "done")
	}
}

// TestTodoClosedAlias verifies "closed" works as an alias for "done" status.
func TestTodoClosedAlias(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "done item", "medium", "")
	store.Transition(testAgent, 1, "done", "finished")

	resp, err := cmd.Execute(context.Background(), Request{Args: "closed"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "done item") {
		t.Errorf("expected done item via 'closed' alias: %s", resp.Text)
	}
}

// TestTodoListAllStatuses verifies "all" shows items of every status.
func TestTodoListAllStatuses(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "open one", "medium", "")
	store.Add(testAgent, "done one", "medium", "")
	store.Transition(testAgent, 2, "done", "finished")

	resp, err := cmd.Execute(context.Background(), Request{Args: "all"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "open one") || !contains(resp.Text, "done one") {
		t.Errorf("expected both items: %s", resp.Text)
	}
}

// TestTodoKeyboardOptions verifies the keyboard options are returned.
func TestTodoKeyboardOptions(t *testing.T) {
	cmd := TodoCommand()
	opts := cmd.KeyboardOptions(context.Background(), CommandContext{})
	if len(opts) != 5 {
		t.Errorf("expected 5 keyboard options, got %d", len(opts))
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsStr(s, substr)
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
