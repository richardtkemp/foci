package command

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// TestParseTodoArgs_DoneReason verifies the done-with-reason parse
// (Richard, 2026-10-30): when text follows, ONLY THE FIRST word is an id and
// the raw remainder is the close reason, kept byte for byte (spacing, quotes,
// $, backticks, a leading dash). A digits-only word after the first one is
// reason text, not another id.
func TestParseTodoArgs_DoneReason(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		ids    []int64
		reason string
	}{
		{name: "simple", raw: "done 7 landed abc", ids: []int64{7}, reason: "landed abc"},
		{name: "second number is reason, not an id", raw: "done 3 5 shipped it", ids: []int64{3}, reason: "5 shipped it"},
		{name: "ruling example", raw: "done 2457 2 commits landed", ids: []int64{2457}, reason: "2 commits landed"},
		{name: "complete alias", raw: "complete 5 dup of 3", ids: []int64{5}, reason: "dup of 3"},
		{name: "finish alias", raw: "finish 5 x y", ids: []int64{5}, reason: "x y"},
		{name: "close alias", raw: "close 5 why", ids: []int64{5}, reason: "why"},
		{name: "uppercase verb", raw: "DONE 5 Upper Case", ids: []int64{5}, reason: "Upper Case"},
		{
			name:   "spec example",
			raw:    "done 7 landed 3fc608953 (factory run 01M4SWKGRFGSMXBHKPKH3E6VEK)",
			ids:    []int64{7},
			reason: "landed 3fc608953 (factory run 01M4SWKGRFGSMXBHKPKH3E6VEK)",
		},
		{
			name:   "quotes dollars backticks",
			raw:    "done 7 landed abc (run \"x\" $(id) `y`)",
			ids:    []int64{7},
			reason: "landed abc (run \"x\" $(id) `y`)",
		},
		{name: "leading dash and double spaces", raw: "done 7 -x  a  b", ids: []int64{7}, reason: "-x  a  b"},
		{name: "newline and inner spaces", raw: "done 7 a  b\n  c", ids: []int64{7}, reason: "a  b\n  c"},
		{name: "integer after first word stays in reason", raw: "done 7 landed 8", ids: []int64{7}, reason: "landed 8"},
		{name: "tag-looking word is reason", raw: "done 7 t:shopping", ids: []int64{7}, reason: "t:shopping"},
		// The id grammar is digits only (memory.ParseTodoID): a signed word
		// can never be read as another id, so numeric-looking reason text
		// closes the todos it names and no others.
		{name: "plus-signed number is reason, not another id", raw: "done 13 +14 for the fix", ids: []int64{13}, reason: "+14 for the fix"},
		{name: "negative number is reason, not another id", raw: "done 7 -1 penalty", ids: []int64{7}, reason: "-1 penalty"},
		{name: "leading zeros are still an id", raw: "done 007 x", ids: []int64{7}, reason: "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTodoArgs(tt.raw)
			assertTodoArgs(t, todoArgs{
				subcommand: "done",
				status:     "active",
				sort:       "priority",
				limit:      15,
				ids:        tt.ids,
				reason:     tt.reason,
			}, got)
		})
	}
}

// TestParseTodoArgs_DoneRequiresLeadingID pins the done grammar after
// Richard's 2026-10-17 ruling: /todo done means CLOSE and nothing else,
// so the words after the verb must start with a digits-only id. A first
// word that is not one — a mistyped id (14x), a filter word (t:x,
// reverse), a signed number (+14) — is badID and the verb refuses; no
// words at all leaves the ids empty for the usage line. Ids-only forms
// keep the empty reason, as ever.
func TestParseTodoArgs_DoneRequiresLeadingID(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want todoArgs
	}{
		{
			name: "done single id keeps empty reason",
			raw:  "done 5",
			want: todoArgs{subcommand: "done", status: "active", sort: "priority", limit: 15, ids: []int64{5}},
		},
		{
			name: "done batch keeps empty reason",
			raw:  "done 3 5 7",
			want: todoArgs{subcommand: "done", status: "active", sort: "priority", limit: 15, ids: []int64{3, 5, 7}},
		},
		{
			name: "mistyped id is refused, not a reason",
			raw:  "done 14x landed abc",
			want: todoArgs{subcommand: "done", status: "active", sort: "priority", limit: 15, badID: "14x"},
		},
		{
			name: "tag filter word is refused",
			raw:  "done t:shopping",
			want: todoArgs{subcommand: "done", status: "active", sort: "priority", limit: 15, badID: "t:shopping"},
		},
		{
			name: "reverse filter word is refused",
			raw:  "done reverse 5",
			want: todoArgs{subcommand: "done", status: "active", sort: "priority", limit: 15, badID: "reverse"},
		},
		{
			name: "reason word first is refused",
			raw:  "done landed 7",
			want: todoArgs{subcommand: "done", status: "active", sort: "priority", limit: 15, badID: "landed"},
		},
		{
			// The id grammar is digits only, so a signed number can
			// never lead: /todo done +14 for the fix is an error naming
			// "+14" (the base parse read it as id 14 and closed it).
			name: "signed lead is refused, never id 14",
			raw:  "done +14 for the fix",
			want: todoArgs{subcommand: "done", status: "active", sort: "priority", limit: 15, badID: "+14"},
		},
		{
			name: "bare done is the verb with no ids",
			raw:  "done",
			want: todoArgs{subcommand: "done", status: "active", sort: "priority", limit: 15},
		},
		{
			name: "bare complete alias is the verb with no ids",
			raw:  "complete",
			want: todoArgs{subcommand: "done", status: "active", sort: "priority", limit: 15},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertTodoArgs(t, tt.want, parseTodoArgs(tt.raw))
		})
	}
}

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
			name: "bare done is the close verb with no ids",
			raw:  "done",
			want: todoArgs{subcommand: "done", status: "active", sort: "priority", limit: 15},
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
			name: "started status",
			raw:  "started",
			want: todoArgs{status: "started", sort: "priority", limit: 15},
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
			want: todoArgs{status: "active", sort: "priority", limit: 15, tags: []string{"shopping"}, setTag: true},
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
			want: todoArgs{status: "", sort: "created", limit: 15, priority: "high", tags: []string{"work"}, setTag: true},
		},
		// Negated filters
		{
			name: "negated tag with dash prefix",
			raw:  "-t:background",
			want: todoArgs{status: "active", sort: "priority", limit: 15, tags: []string{"!background"}, setTag: true},
		},
		{
			name: "negated tag with bang prefix",
			raw:  "!t:background",
			want: todoArgs{status: "active", sort: "priority", limit: 15, tags: []string{"!background"}, setTag: true},
		},
		{
			name: "negated tag with bang inside value",
			raw:  "t:!background",
			want: todoArgs{status: "active", sort: "priority", limit: 15, tags: []string{"!background"}, setTag: true},
		},
		{
			name: "negated priority with dash prefix",
			raw:  "-p:low",
			want: todoArgs{status: "active", sort: "priority", limit: 15, priority: "!low"},
		},
		{
			name: "negated priority with bang prefix",
			raw:  "!p:low",
			want: todoArgs{status: "active", sort: "priority", limit: 15, priority: "!low"},
		},
		{
			name: "negated priority with bang inside value",
			raw:  "p:!low",
			want: todoArgs{status: "active", sort: "priority", limit: 15, priority: "!low"},
		},
		// Multi-tag filters
		{
			name: "multiple positive tags",
			raw:  "t:foci t:bug",
			want: todoArgs{status: "active", sort: "priority", limit: 15, tags: []string{"foci", "bug"}, setTag: true},
		},
		{
			name: "positive and negated tags",
			raw:  "t:work -t:background",
			want: todoArgs{status: "active", sort: "priority", limit: 15, tags: []string{"work", "!background"}, setTag: true},
		},
		{
			name: "multiple negated tags",
			raw:  "-t:background -t:daily",
			want: todoArgs{status: "active", sort: "priority", limit: 15, tags: []string{"!background", "!daily"}, setTag: true},
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
			want: todoArgs{subcommand: "new", status: "active", sort: "priority", limit: 15, text: "bread", tags: []string{"shopping"}, setTag: true, priority: "high"},
		},
		{
			name: "new with multiple tags",
			raw:  "new t:foci t:background do the thing",
			want: todoArgs{subcommand: "new", status: "active", sort: "priority", limit: 15, text: "do the thing", tags: []string{"foci", "background"}, setTag: true},
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
			name: "done with non-integer lead is refused",
			raw:  "done t:shopping",
			want: todoArgs{subcommand: "done", status: "active", sort: "priority", limit: 15, badID: "t:shopping"},
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
			want: todoArgs{subcommand: "edit", status: "active", sort: "priority", limit: 15, ids: []int64{5}, tags: []string{"urgent"}, setTag: true, text: "new text here"},
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
		// Subcommand aliases
		{
			name: "add alias for new",
			raw:  "add buy milk",
			want: todoArgs{subcommand: "new", status: "active", sort: "priority", limit: 15, text: "buy milk"},
		},
		{
			name: "create alias for new",
			raw:  "create buy milk",
			want: todoArgs{subcommand: "new", status: "active", sort: "priority", limit: 15, text: "buy milk"},
		},
		{
			name: "complete alias for done transition",
			raw:  "complete 5",
			want: todoArgs{subcommand: "done", status: "active", sort: "priority", limit: 15, ids: []int64{5}},
		},
		{
			name: "finish alias for done transition",
			raw:  "finish 3 7",
			want: todoArgs{subcommand: "done", status: "active", sort: "priority", limit: 15, ids: []int64{3, 7}},
		},
		{
			name: "close alias for done transition",
			raw:  "close 5",
			want: todoArgs{subcommand: "done", status: "active", sort: "priority", limit: 15, ids: []int64{5}},
		},
		{
			name: "complete bare is the close verb with no ids",
			raw:  "complete",
			want: todoArgs{subcommand: "done", status: "active", sort: "priority", limit: 15},
		},
		{
			name: "begin alias for start",
			raw:  "begin 5",
			want: todoArgs{subcommand: "start", status: "active", sort: "priority", limit: 15, ids: []int64{5}},
		},
		{
			name: "cancel alias for drop",
			raw:  "cancel 5",
			want: todoArgs{subcommand: "drop", status: "active", sort: "priority", limit: 15, ids: []int64{5}},
		},
		{
			name: "abandon alias for drop",
			raw:  "abandon 5",
			want: todoArgs{subcommand: "drop", status: "active", sort: "priority", limit: 15, ids: []int64{5}},
		},
		{
			name: "update alias for edit",
			raw:  "update 5 p:high",
			want: todoArgs{subcommand: "edit", status: "active", sort: "priority", limit: 15, ids: []int64{5}, priority: "high"},
		},
		{
			name: "modify alias for edit",
			raw:  "modify 5 new text",
			want: todoArgs{subcommand: "edit", status: "active", sort: "priority", limit: 15, ids: []int64{5}, text: "new text"},
		},
		{
			name: "info alias for show",
			raw:  "info 5",
			want: todoArgs{subcommand: "show", status: "active", sort: "priority", limit: 15, ids: []int64{5}},
		},
		{
			name: "detail alias for show",
			raw:  "detail 5",
			want: todoArgs{subcommand: "show", status: "active", sort: "priority", limit: 15, ids: []int64{5}},
		},
		{
			name: "find alias for search",
			raw:  "find buy milk",
			want: todoArgs{subcommand: "search", status: "active", sort: "priority", limit: 15, text: "buy milk"},
		},
		{
			name: "remove alias for rm",
			raw:  "remove 5",
			want: todoArgs{subcommand: "rm", status: "active", sort: "priority", limit: 15, ids: []int64{5}},
		},
		{
			name: "delete alias for rm",
			raw:  "delete 5",
			want: todoArgs{subcommand: "rm", status: "active", sort: "priority", limit: 15, ids: []int64{5}},
		},
		{
			name: "del alias for rm",
			raw:  "del 5",
			want: todoArgs{subcommand: "rm", status: "active", sort: "priority", limit: 15, ids: []int64{5}},
		},
		{
			name: "summary alias for stats",
			raw:  "summary",
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
	if len(got.tags) != len(want.tags) {
		t.Errorf("tags: got %v, want %v", got.tags, want.tags)
	} else {
		for i := range want.tags {
			if got.tags[i] != want.tags[i] {
				t.Errorf("tags[%d]: got %q, want %q", i, got.tags[i], want.tags[i])
			}
		}
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
	if got.reason != want.reason {
		t.Errorf("reason: got %q, want %q", got.reason, want.reason)
	}
	if got.badID != want.badID {
		t.Errorf("badID: got %q, want %q", got.badID, want.badID)
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

// TestTodoBareShowsUsage verifies that bare /todo (no args) returns usage text,
// not a list of items.
func TestTodoBareShowsUsage(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "buy milk", "high", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: ""}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "Usage: /todo") {
		t.Errorf("bare /todo should show usage, got: %s", resp.Text)
	}
	if contains(resp.Text, "buy milk") {
		t.Errorf("bare /todo should not list items, got: %s", resp.Text)
	}
}

// TestTodoListEmpty verifies that listing with no items returns an appropriate message.
func TestTodoListEmpty(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	resp, err := cmd.Execute(context.Background(), Request{Args: "active"}, cc)
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

	resp, err := cmd.Execute(context.Background(), Request{Args: "active"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "#1") || !contains(resp.Text, "#2") {
		t.Errorf("expected both items in output: %s", resp.Text)
	}
	if !contains(resp.Text, "buy milk") || !contains(resp.Text, "buy bread") {
		t.Errorf("expected item text in output: %s", resp.Text)
	}
}

// TestTodoListTableFormat verifies that setting todo_format=table produces a markdown table.
func TestTodoListTableFormat(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cc.ResolvedLive = config.NewLiveValue(&config.ResolvedAgentConfig{
		Tools: config.ResolvedTool{TodoFormat: "table"},
	})
	cmd := TodoCommand()

	store.Add(testAgent, "buy milk", "high", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "active"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "| 1 |") {
		t.Errorf("expected table format with pipe-delimited columns: %s", resp.Text)
	}
}

// TestTodoListDefaultFormat verifies that the default format (no config) uses lines.
func TestTodoListDefaultFormat(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "buy milk", "high", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "active"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "**#1** `high`") || !contains(resp.Text, "buy milk") {
		t.Errorf("expected lines format with metadata and text: %s", resp.Text)
	}
}

// TestTodoListGlobalFormat verifies that defaults.todo_format is used when per-agent is unset.
func TestTodoListGlobalFormat(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cc.ResolvedLive = config.NewLiveValue(&config.ResolvedAgentConfig{
		Tools: config.ResolvedTool{TodoFormat: "table"},
	})
	cmd := TodoCommand()

	store.Add(testAgent, "buy milk", "high", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "active"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "| 1 |") {
		t.Errorf("expected table format from global default: %s", resp.Text)
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

// TestTodoListWithNegatedTagFilter verifies that -t:TAG excludes items with
// that tag, returning only items without it.
func TestTodoListWithNegatedTagFilter(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "background task", "medium", "background")
	store.Add(testAgent, "foreground task", "medium", "work")
	store.Add(testAgent, "untagged task", "medium", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "-t:background"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if contains(resp.Text, "background task") {
		t.Errorf("should exclude background-tagged item: %s", resp.Text)
	}
	if !contains(resp.Text, "foreground task") {
		t.Errorf("should include non-background items: %s", resp.Text)
	}
	if !contains(resp.Text, "untagged task") {
		t.Errorf("should include untagged items: %s", resp.Text)
	}
}

// TestTodoListWithMultipleTagFilters verifies that multiple t:TAG filters use
// AND logic: only items matching all tags are returned.
func TestTodoListWithMultipleTagFilters(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "foci bug", "medium", "foci,bug")
	store.Add(testAgent, "foci feature", "medium", "foci,feature")
	store.Add(testAgent, "other bug", "medium", "other,bug")
	store.Add(testAgent, "untagged", "medium", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "t:foci t:bug"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "foci bug") {
		t.Errorf("expected item with both tags: %s", resp.Text)
	}
	if contains(resp.Text, "foci feature") {
		t.Errorf("should exclude item missing 'bug' tag: %s", resp.Text)
	}
	if contains(resp.Text, "other bug") {
		t.Errorf("should exclude item missing 'foci' tag: %s", resp.Text)
	}
}

// TestTodoListWithMixedTagFilters verifies that positive and negated tag
// filters can be combined (AND logic).
func TestTodoListWithMixedTagFilters(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "work task", "medium", "work")
	store.Add(testAgent, "work bg task", "medium", "work,background")
	store.Add(testAgent, "personal task", "medium", "personal")

	resp, err := cmd.Execute(context.Background(), Request{Args: "t:work -t:background"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "work task") {
		t.Errorf("expected work item without background: %s", resp.Text)
	}
	if contains(resp.Text, "work bg task") {
		t.Errorf("should exclude work+background item: %s", resp.Text)
	}
	if contains(resp.Text, "personal task") {
		t.Errorf("should exclude non-work item: %s", resp.Text)
	}
}

// TestTodoListWithNegatedPriorityFilter verifies that -p:PRIO excludes items
// with that priority.
func TestTodoListWithNegatedPriorityFilter(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "high item", "high", "")
	store.Add(testAgent, "medium item", "medium", "")
	store.Add(testAgent, "low item", "low", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "-p:low"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if contains(resp.Text, "low item") {
		t.Errorf("should exclude low-priority item: %s", resp.Text)
	}
	if !contains(resp.Text, "high item") || !contains(resp.Text, "medium item") {
		t.Errorf("should include non-low items: %s", resp.Text)
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

// TestTodoNewWithMultipleTags verifies creating with multiple t: prefixes
// stores all tags as comma-separated.
func TestTodoNewWithMultipleTags(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	resp, err := cmd.Execute(context.Background(), Request{Args: "new t:foci t:background do the thing"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "Added #1") {
		t.Errorf("expected confirmation: %s", resp.Text)
	}
	item, _ := store.Get(testAgent, 1)
	if item.Tags != "foci,background" {
		t.Errorf("tags: got %q, want %q", item.Tags, "foci,background")
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

// TestTodoBatchTransition verifies completing multiple todos at once,
// each closed with an empty close reason — the ids-only form carries no
// reason words.
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
	if i1.CloseReason != "" || i3.CloseReason != "" {
		t.Errorf("ids-only batch close reasons = %q, %q, want both empty", i1.CloseReason, i3.CloseReason)
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
	if !contains(resp.Text, "started") {
		t.Errorf("expected started: %s", resp.Text)
	}
	item, _ := store.Get(testAgent, 1)
	if item.Status != "started" {
		t.Errorf("status: got %q, want %q", item.Status, "started")
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

func TestTodoShowMultipleIDs(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "first item", "high", "work")
	store.Add(testAgent, "second item", "low", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "show 1 2"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "#1 first item") {
		t.Errorf("missing #1: %s", resp.Text)
	}
	if !contains(resp.Text, "#2 second item") {
		t.Errorf("missing #2: %s", resp.Text)
	}

	// Non-existent ID shows error but doesn't abort the rest.
	resp, err = cmd.Execute(context.Background(), Request{Args: "show 1 999 2"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "#1 first item") {
		t.Errorf("missing #1 before error: %s", resp.Text)
	}
	if !contains(resp.Text, "#999:") {
		t.Errorf("missing error for #999: %s", resp.Text)
	}
	if !contains(resp.Text, "#2 second item") {
		t.Errorf("missing #2 after error: %s", resp.Text)
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

// TestTodoDoneNonIDFormsAreErrors pins Richard's 2026-10-17 ruling through
// the public entry point: /todo done means CLOSE and nothing else. Bare
// `done` (and its aliases) answers with the usage line, and a first word
// after the verb that is not a digits-only id — a mistyped id, a filter
// word, a signed number — is refused naming the word. Neither lists, and
// neither changes any todo; listing done todos stays available through
// /todo list done.
func TestTodoDoneNonIDFormsAreErrors(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	closedID, _ := store.Add(testAgent, "closed item", "medium", "")
	if err := store.Transition(testAgent, closedID, "done", "bought"); err != nil {
		t.Fatalf("seed closed item: %v", err)
	}
	openID, _ := store.Add(testAgent, "open item", "medium", "")

	for _, tt := range []struct {
		args string
		want string
	}{
		{args: "done", want: "Usage: /todo done <ids> [reason]"},
		{args: "complete", want: "Usage: /todo done <ids> [reason]"},
		{args: "done 14x landed abc", want: `/todo done: "14x" is not a todo id (ids are digits only); nothing was changed`},
		{args: "done t:shopping", want: `/todo done: "t:shopping" is not a todo id (ids are digits only); nothing was changed`},
		{args: "done reverse 5", want: `/todo done: "reverse" is not a todo id (ids are digits only); nothing was changed`},
		{args: "done +14 for the fix", want: `/todo done: "+14" is not a todo id (ids are digits only); nothing was changed`},
	} {
		t.Run(tt.args, func(t *testing.T) {
			resp, err := cmd.Execute(context.Background(), Request{Args: tt.args}, cc)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Text != tt.want {
				t.Errorf("response = %q, want %q", resp.Text, tt.want)
			}
			if contains(resp.Text, "Todos") || contains(resp.Text, "item") {
				t.Errorf("response = %q, must not be a listing", resp.Text)
			}
		})
	}

	for _, id := range []int64{closedID, openID} {
		item, err := store.Get(testAgent, id)
		if err != nil {
			t.Fatalf("Get #%d: %v", id, err)
		}
		if id == openID && item.Status != "open" {
			t.Errorf("open item = %s, want still open — a refused done must change nothing", item.Status)
		}
	}

	// Listing done todos stays available through the list form.
	resp, err := cmd.Execute(context.Background(), Request{Args: "list done"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "closed item") {
		t.Errorf("/todo list done = %q, want the done item listed", resp.Text)
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

// TestTodoKeyboardOptions verifies the keyboard options are returned, and
// that the done button keeps listing done todos through the list form —
// its data is "list done" because the bare done verb is the usage error
// since /todo done means close and nothing else (Richard, 2026-10-17).
func TestTodoKeyboardOptions(t *testing.T) {
	cmd := TodoCommand()
	opts := cmd.KeyboardOptions(context.Background(), CommandContext{})
	if len(opts) != 5 {
		t.Errorf("expected 5 keyboard options, got %d", len(opts))
	}
	for _, opt := range opts {
		if opt.Label == "done" && opt.Data != "list done" {
			t.Errorf("done button data = %q, want %q — the bare done verb no longer lists", opt.Data, "list done")
		}
	}
}

// --- parseGetArgs Tests ---

// TestParseGetArgs verifies the get subcommand parser handles filter-only,
// search-only, combined, and explicit "/" delimiter cases.
func TestParseGetArgs(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want todoArgs
	}{
		{
			name: "search only",
			raw:  "get deploy server",
			want: todoArgs{subcommand: "get", status: "active", sort: "priority", limit: 15, text: "deploy server"},
		},
		{
			name: "filters only (tag)",
			raw:  "get t:daily",
			want: todoArgs{subcommand: "get", status: "active", sort: "priority", limit: 15, tags: []string{"daily"}, setTag: true},
		},
		{
			name: "filters only (priority and sort)",
			raw:  "get p:high created",
			want: todoArgs{subcommand: "get", status: "active", sort: "created", limit: 15, priority: "high"},
		},
		{
			name: "tag with search",
			raw:  "get t:work deploy",
			want: todoArgs{subcommand: "get", status: "active", sort: "priority", limit: 15, tags: []string{"work"}, setTag: true, text: "deploy"},
		},
		{
			name: "priority sort and search",
			raw:  "get p:high created server database",
			want: todoArgs{subcommand: "get", status: "active", sort: "created", limit: 15, priority: "high", text: "server database"},
		},
		{
			name: "explicit delimiter",
			raw:  "get t:work / deploy server",
			want: todoArgs{subcommand: "get", status: "active", sort: "priority", limit: 15, tags: []string{"work"}, setTag: true, text: "deploy server"},
		},
		{
			name: "delimiter with status filter",
			raw:  "get all t:work / deploy -old",
			want: todoArgs{subcommand: "get", status: "", sort: "priority", limit: 15, tags: []string{"work"}, setTag: true, text: "deploy -old"},
		},
		{
			name: "status and reverse with search",
			raw:  "get done reverse deploy",
			want: todoArgs{subcommand: "get", status: "done", sort: "priority", limit: 15, reverse: true, text: "deploy"},
		},
		{
			// #880: a bare integer selects a todo by ID, not a count limit.
			name: "bare id with search",
			raw:  "get 5 deploy",
			want: todoArgs{subcommand: "get", status: "active", sort: "priority", limit: 15, ids: []int64{5}, text: "deploy"},
		},
		{
			// #880: nN is the count-limit form.
			name: "count limit with search",
			raw:  "get n5 deploy",
			want: todoArgs{subcommand: "get", status: "active", sort: "priority", limit: 5, text: "deploy"},
		},
		{
			name: "empty get (no args)",
			raw:  "get",
			want: todoArgs{subcommand: "get", status: "active", sort: "priority", limit: 15},
		},
		// Negated filters in get
		{
			name: "get negated tag with dash",
			raw:  "get -t:background deploy",
			want: todoArgs{subcommand: "get", status: "active", sort: "priority", limit: 15, tags: []string{"!background"}, setTag: true, text: "deploy"},
		},
		{
			name: "get negated tag with bang",
			raw:  "get !t:background deploy",
			want: todoArgs{subcommand: "get", status: "active", sort: "priority", limit: 15, tags: []string{"!background"}, setTag: true, text: "deploy"},
		},
		{
			name: "get negated priority with dash",
			raw:  "get -p:low server",
			want: todoArgs{subcommand: "get", status: "active", sort: "priority", limit: 15, priority: "!low", text: "server"},
		},
		{
			name: "get negated priority with bang",
			raw:  "get !p:low server",
			want: todoArgs{subcommand: "get", status: "active", sort: "priority", limit: 15, priority: "!low", text: "server"},
		},
		{
			name: "get multiple tags with search",
			raw:  "get t:foci t:bug deploy",
			want: todoArgs{subcommand: "get", status: "active", sort: "priority", limit: 15, tags: []string{"foci", "bug"}, setTag: true, text: "deploy"},
		},
		{
			name: "get multiple tags via delimiter",
			raw:  "get t:foci t:bug / deploy server",
			want: todoArgs{subcommand: "get", status: "active", sort: "priority", limit: 15, tags: []string{"foci", "bug"}, setTag: true, text: "deploy server"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTodoArgs(tt.raw)
			assertTodoArgs(t, tt.want, got)
		})
	}
}

// TestTodoGetFilterOnly verifies that get with only filters (no search text)
// falls back to List behavior.
func TestTodoGetFilterOnly(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "daily standup", "medium", "daily")
	store.Add(testAgent, "deploy app", "high", "work")

	resp, err := cmd.Execute(context.Background(), Request{Args: "get t:daily"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "daily standup") {
		t.Errorf("expected daily standup: %s", resp.Text)
	}
	if contains(resp.Text, "deploy app") {
		t.Errorf("should not contain non-daily items: %s", resp.Text)
	}
}

// TestTodoGetWithSearch verifies that get with both filters and search text
// uses full-text search with post-filtering. Requires bleve index.
func TestTodoGetWithSearch(t *testing.T) {
	store, idx := newTestTodoStoreWithSearch(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "deploy backend server", "high", "work")
	store.Add(testAgent, "deploy frontend app", "medium", "work")
	store.Add(testAgent, "deploy staging server", "low", "personal")

	resp, err := cmd.Execute(context.Background(), Request{Args: "get t:work server"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "deploy backend server") {
		t.Errorf("expected backend server: %s", resp.Text)
	}
	if contains(resp.Text, "frontend") {
		t.Errorf("should not contain non-matching search term: %s", resp.Text)
	}
	if contains(resp.Text, "staging") {
		t.Errorf("should not contain non-work items: %s", resp.Text)
	}

	_ = idx // keep reference
}

// TestTodoGetPriorityFilter verifies that get filters by priority in search mode.
func TestTodoGetPriorityFilter(t *testing.T) {
	store, idx := newTestTodoStoreWithSearch(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "deploy production server", "high", "")
	store.Add(testAgent, "deploy staging server", "low", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "get p:high server"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "production") {
		t.Errorf("expected high-priority item: %s", resp.Text)
	}
	if contains(resp.Text, "staging") {
		t.Errorf("should not contain low-priority item: %s", resp.Text)
	}

	_ = idx
}

// TestTodoGetNegatedSearch verifies that an all-negated search query like
// "-android" excludes matching items instead of returning nothing.
func TestTodoGetNegatedSearch(t *testing.T) {
	store, idx := newTestTodoStoreWithSearch(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "fix login bug in auth", "high", "foci,bug")
	store.Add(testAgent, "android app crash on start", "medium", "foci,bug")
	store.Add(testAgent, "update server config", "low", "foci,bug")

	// "-android" should exclude the android item
	resp, err := cmd.Execute(context.Background(), Request{Args: "get t:foci t:bug / -android"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if contains(resp.Text, "android app") {
		t.Errorf("should exclude android item: %s", resp.Text)
	}
	if !contains(resp.Text, "login bug") {
		t.Errorf("expected login bug item: %s", resp.Text)
	}
	if !contains(resp.Text, "server config") {
		t.Errorf("expected server config item: %s", resp.Text)
	}

	_ = idx
}

// TestTodoGetNoSearchIndex verifies get returns an error when there's no
// search index and a search query is provided.
func TestTodoGetNoSearchIndex(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "some item", "medium", "")

	_, err := cmd.Execute(context.Background(), Request{Args: "get deploy"}, cc)
	if err == nil {
		t.Error("expected error when no search index is configured")
	}
}

// newTestTodoStoreWithSearch creates a TodoStore with a bleve search index.
func newTestTodoStoreWithSearch(t *testing.T) (*memory.TodoStore, *memory.BleveIndex) {
	t.Helper()
	dir := t.TempDir()
	store, err := memory.NewTodoStore(filepath.Join(dir, "todo.db"))
	if err != nil {
		t.Fatalf("NewTodoStore: %v", err)
	}

	memDir := filepath.Join(dir, "memory")
	if err := os.MkdirAll(memDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	sources := map[string]memory.SourceConfig{"memory": {Dir: memDir, Weight: 1.0}}
	idx, err := memory.NewBleveIndex(filepath.Join(dir, "search.bleve"), sources, 0, 0.1)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}

	store.SetSearchIndex(idx)
	t.Cleanup(func() {
		_ = store.Close()
		_ = idx.Close()
	})
	return store, idx
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

// TestTodoPriorityAliases: /todo new and edit coerce an alias and refuse an
// unmappable priority with the valid values (#2071).
func TestTodoPriorityAliases(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	resp, err := cmd.Execute(context.Background(), Request{Args: "new p:med a thing"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "(medium)") {
		t.Errorf("new echo = %q, want (medium)", resp.Text)
	}
	if item, _ := store.Get(testAgent, 1); item.Priority != "medium" {
		t.Errorf("stored priority = %q, want medium", item.Priority)
	}

	resp, err = cmd.Execute(context.Background(), Request{Args: "edit 1 p:urgent"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "high, medium, low") {
		t.Errorf("edit p:urgent = %q, want the valid values listed", resp.Text)
	}
	if item, _ := store.Get(testAgent, 1); item.Priority != "medium" {
		t.Errorf("rejected edit changed priority to %q", item.Priority)
	}
}

// TestTodoDoneWithReason proves /todo done <id> <words> closes the id with
// the words as the close reason — status done, completed_at set, reason
// stored — instead of silently listing todos.
func TestTodoDoneWithReason(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "ship it", "medium", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "done 1 landed abc"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "#1 → done" {
		t.Errorf("response = %q, want %q (not a listing)", resp.Text, "#1 → done")
	}
	item, err := store.Get(testAgent, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if item.Status != "done" || item.CompletedAt == nil {
		t.Errorf("item = {%s, completed_at=%v}, want done with completed_at set", item.Status, item.CompletedAt)
	}
	if item.CloseReason != "landed abc" {
		t.Errorf("close_reason = %q, want %q", item.CloseReason, "landed abc")
	}
}

// TestTodoDoneOnlyFirstIDWhenTextFollows pins Richard's 2026-10-30 ruling:
// with a reason, only the first word is an id. "/todo done 1 2 commits
// landed" closes #1 alone, with reason "2 commits landed", and leaves #2
// open — a number at the start of a reason never closes a second ticket.
func TestTodoDoneOnlyFirstIDWhenTextFollows(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "first", "medium", "")
	store.Add(testAgent, "second", "medium", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "done 1 2 commits landed"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "#1 → done" {
		t.Errorf("response = %q, want one confirmation line for #1", resp.Text)
	}
	first, _ := store.Get(testAgent, 1)
	if first.Status != "done" || first.CloseReason != "2 commits landed" {
		t.Errorf("#1 = {%s, %q}, want done with reason %q", first.Status, first.CloseReason, "2 commits landed")
	}
	second, _ := store.Get(testAgent, 2)
	if second.Status == "done" {
		t.Errorf("#2 status = %q, want it left open — only the first word is an id", second.Status)
	}
}

// TestTodoDoneReasonKeepsSpacing proves the reason's inner spacing survives
// the parse byte for byte — double spaces, newlines, trailing spaces.
func TestTodoDoneReasonKeepsSpacing(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "spacing", "medium", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "done 1 a  b\n  c"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(resp.Text, "#1") || contains(resp.Text, "error") {
		t.Errorf("response = %q, want a clean transition", resp.Text)
	}
	item, _ := store.Get(testAgent, 1)
	if item.CloseReason != "a  b\n  c" {
		t.Errorf("close_reason = %q, want %q", item.CloseReason, "a  b\n  c")
	}
}

// TestTodoDoneUnknownIDWithReason proves a missing id with a reason reports
// the error per id instead of falling back to listing todos, and that
// nothing else is closed.
func TestTodoDoneUnknownIDWithReason(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "survivor", "medium", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "done 99 landed abc"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "#99: error: todo #99 not found" {
		t.Errorf("response = %q, want the per-id error", resp.Text)
	}
	if contains(resp.Text, "Todos") || contains(resp.Text, "survivor") {
		t.Errorf("response = %q, must not be a listing", resp.Text)
	}
	item, _ := store.Get(testAgent, 1)
	if item.Status != "open" {
		t.Errorf("unrelated #1 = %q, want still open", item.Status)
	}
}

// TestTodoDoneIDsOnlyKeepsEmptyReason is a characterisation test: /todo done
// with ids only keeps today's behaviour — closed with an empty close_reason.
func TestTodoDoneIDsOnlyKeepsEmptyReason(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "plain", "medium", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "done 1"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "#1 → done" {
		t.Errorf("response = %q, want %q", resp.Text, "#1 → done")
	}
	item, _ := store.Get(testAgent, 1)
	if item.Status != "done" || item.CloseReason != "" {
		t.Errorf("item = {%s, %q}, want done with empty reason", item.Status, item.CloseReason)
	}
}

// TestTodoDoneSignedWordIsReason proves a plus-signed number after the ids
// is reason text, never another id: `/todo done 13 +14 for the fix` closes
// only #13 with that whole reason — the base parse read "+14" as id 14 and
// closed it too, silently.
func TestTodoDoneSignedWordIsReason(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "thirteen", "medium", "")
	store.Add(testAgent, "fourteen", "medium", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "done 1 +2 for the fix"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "#1 → done" {
		t.Errorf("response = %q, want only #1 closed (no #2 line, no listing)", resp.Text)
	}
	if item, _ := store.Get(testAgent, 1); item.Status != "done" || item.CloseReason != "+2 for the fix" {
		t.Errorf("#1 = {%s, %q}, want done with the plus-signed reason", item.Status, item.CloseReason)
	}
	if item, _ := store.Get(testAgent, 2); item.Status != "open" {
		t.Errorf("#2 = %s, want still open — a signed word is reason, not an id", item.Status)
	}
}

// TestTodoTransitionSignedTokenIsNotID proves the plain transition verbs
// share the digits-only id grammar: `/todo start +3` names "+3" in a
// refusal that changes nothing, instead of transitioning todo #3 (which
// the old ParseInt-based id list once did).
func TestTodoTransitionSignedTokenIsNotID(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	store.Add(testAgent, "survivor", "medium", "")

	resp, err := cmd.Execute(context.Background(), Request{Args: "start +1"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `/todo start: "+1" is not a todo id (ids are digits only); nothing was changed` {
		t.Errorf("response = %q, want the refusal naming the word", resp.Text)
	}
	if item, _ := store.Get(testAgent, 1); item.Status != "open" {
		t.Errorf("#1 = %s, want still open", item.Status)
	}
}

// TestTodoGetEditSignedWordIsNotID proves get and edit read their ids
// with the one digits-only grammar (memory.ParseTodoID) like every other
// verb: a decorated word like "+7" never selects ticket 7 — for get it
// is a search term, for edit it leaves no id and the verb answers its
// usage line — where the old ParseInt read it as 7 and acted on the
// ticket. The digits-only form still selects.
func TestTodoGetEditSignedWordIsNotID(t *testing.T) {
	store := newTestTodoStore(t)
	cc := newTestCC(store)
	cmd := TodoCommand()

	id, err := store.Add(testAgent, "probe item", "medium", "")
	if err != nil {
		t.Fatal(err)
	}
	signed := "+" + strconv.FormatInt(id, 10)

	// get's grammar is filters + search terms: a word that is not an id
	// IS search text, so the signed word must reach the search query,
	// never the id list (parsed here; the search itself needs an index
	// the execute-level tests do not carry).
	if a := parseTodoArgs("get " + signed); len(a.ids) != 0 || a.text != signed {
		t.Errorf("parseTodoArgs(get %s) = {ids %v, text %q}, want the signed word as search text only", signed, a.ids, a.text)
	}

	resp, err := cmd.Execute(context.Background(), Request{Args: "edit " + signed + " probed"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Text, "Usage: /todo edit") {
		t.Errorf("edit %s response = %q, want the usage line — a signed word is not an id", signed, resp.Text)
	}
	if item, _ := store.Get(testAgent, id); item.Text != "probe item" {
		t.Errorf("text = %q, want unchanged (the edit must not act on the signed word as an id)", item.Text)
	}

	resp, err = cmd.Execute(context.Background(), Request{Args: "get " + strconv.FormatInt(id, 10)}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Text, "probe item") {
		t.Errorf("get %d response = %q, want the digits-only form to still select the todo", id, resp.Text)
	}
}

// TestTodoIDListRefusesNonIDWord proves every id-list verb (start, drop,
// reopen, show, rm) refuses an argument list containing a word that is not
// a digits-only id, naming the verb and the word — the whole command
// changes nothing. Skipping the word (this change's earlier behaviour for
// signed tokens) and honouring it as an id (the base's ParseInt behaviour,
// which read "+4" as 4) both act on a list the typist did not spell.
func TestTodoIDListRefusesNonIDWord(t *testing.T) {
	for _, tt := range []struct {
		verb string
		args string
		word string
	}{
		{verb: "start", args: "start 3 +4", word: "+4"},
		{verb: "drop", args: "drop 3 +4", word: "+4"},
		{verb: "reopen", args: "reopen 3 +4", word: "+4"},
		{verb: "show", args: "show 2 +3", word: "+3"},
		{verb: "rm", args: "rm 2 +3", word: "+3"},
		{verb: "drop", args: "drop 3 abc", word: "abc"},
		{verb: "drop", args: "drop +4 3", word: "+4"},
	} {
		t.Run(tt.args, func(t *testing.T) {
			store := newTestTodoStore(t)
			cc := newTestCC(store)
			cmd := TodoCommand()

			store.Add(testAgent, "first", "medium", "")
			store.Add(testAgent, "second", "medium", "")

			resp, err := cmd.Execute(context.Background(), Request{Args: tt.args}, cc)
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("/todo %s: %q is not a todo id (ids are digits only); nothing was changed", tt.verb, tt.word)
			if resp.Text != want {
				t.Errorf("response = %q, want %q", resp.Text, want)
			}
			for _, id := range []int64{1, 2} {
				item, err := store.Get(testAgent, id)
				if err != nil {
					t.Fatalf("#%d gone — rm must not delete anything on a refused list: %v", id, err)
				}
				if item.Status != "open" {
					t.Errorf("#%d = %s, want still open — a refused list must change nothing", id, item.Status)
				}
			}
		})
	}
}

// TestTodoBatchTransitionUsageNamesVerb proves the no-ids usage line names
// the VERB the command accepts (/todo start), not the target state
// (/todo started). Bare "done" reaches its own line, which names the
// reason form (Richard, 2026-10-17: done means close and nothing else).
func TestTodoBatchTransitionUsageNamesVerb(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "start", want: "Usage: /todo start <id> [id...]"},
		{raw: "begin", want: "Usage: /todo start <id> [id...]"},
		{raw: "drop", want: "Usage: /todo drop <id> [id...]"},
		{raw: "cancel", want: "Usage: /todo drop <id> [id...]"},
		{raw: "reopen", want: "Usage: /todo reopen <id> [id...]"},
		{raw: "done", want: "Usage: /todo done <ids> [reason]"},
		{raw: "finish", want: "Usage: /todo done <ids> [reason]"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			store := newTestTodoStore(t)
			cc := newTestCC(store)
			cmd := TodoCommand()
			resp, err := cmd.Execute(context.Background(), Request{Args: tt.raw}, cc)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Text != tt.want {
				t.Errorf("/todo %s with no ids = %q, want %q", tt.raw, resp.Text, tt.want)
			}
		})
	}
}

// TestTodoUsageDocumentsDoneReason proves the /todo help documents the
// reason form of done and the ruling's boundary, word for word: the words
// after the verb must start with an id (done never lists; done todos
// list through /todo list done), and a reason cannot start with a
// digits-only word (those are read as more ids; a signed word like "+1"
// is reason). The whole done entry is pinned verbatim — a wording change
// anywhere in it fails here (a previous round's looser substrings let
// one through).
func TestTodoUsageDocumentsDoneReason(t *testing.T) {
	usage := todoUsage()
	const wantDoneEntry = "  done <id> [reason]           Mark done, recording the reason (aliases: complete, finish, close).\n" +
		"  done <id> <id>...            With a reason, only the first word is an id and the rest is the\n" +
		"                               reason, kept as typed (done 7 2 commits landed closes #7 only).\n" +
		"                               Ids only (done 7 8) closes them all. Done never lists (use\n" +
		"                               /todo list done)\n"
	if !contains(usage, wantDoneEntry) {
		t.Errorf("todoUsage() = %q, want the done entry to read exactly:\n%s", usage, wantDoneEntry)
	}
	for _, sub := range TodoCommand().Subcommands {
		if sub.Name != "done" {
			continue
		}
		if !contains(sub.Description, "reason") {
			t.Errorf("done subcommand description = %q, want it to mention the reason", sub.Description)
		}
	}
}

// TestTodoUsageDocumentsIDGrammar proves the /todo help states the id
// grammar every verb follows, word for word: an <id> is a digits-only
// ticket number (get and edit included — a signed word is never another
// ticket), and a word that is not one in an id list (start, drop, reopen,
// show, rm, and done's first word) is an error naming it that changes
// nothing — the behaviour TestTodoIDListRefusesNonIDWord and
// TestTodoDoneNonIDFormsAreErrors pin. The refusals must be documented
// up front, not discovered on first hit.
func TestTodoUsageDocumentsIDGrammar(t *testing.T) {
	usage := todoUsage()
	const wantGrammar = "An <id> is a digits-only ticket number; get and edit read theirs\n" +
		"with the same grammar (a signed word like \"+7\" is search text or\n" +
		"the usage line, never ticket 7). In an id list (start, drop,\n" +
		"reopen, show, rm, and done's first word) a word that is not one is an error\n" +
		"naming it, and nothing is changed.\n"
	if !contains(usage, wantGrammar) {
		t.Errorf("todoUsage() = %q, want the id-grammar paragraph to read exactly:\n%s", usage, wantGrammar)
	}
}
