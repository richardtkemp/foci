package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/memory"
)

// newSearchableTodoStore is newTestTodoStore with a bleve index, so search works.
func newSearchableTodoStore(t *testing.T) *memory.TodoStore {
	t.Helper()
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	idx, err := memory.NewBleveIndex(filepath.Join(dir, "search.bleve"), map[string]memory.SourceConfig{
		"memory": {Dir: memDir, Weight: 1.0},
	}, 0, 0.1)
	if err != nil {
		t.Fatalf("NewBleveIndex: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	store := newTestTodoStore(t)
	store.SetSearchIndex(idx)
	return store
}

// --truncate (list/search): cap each item's text at N characters. The default
// is todoTruncateDefault when stdout is not piped, and no truncation when it
// is; an explicit value wins either way, and 0 means no truncation.

func TestTodoTruncate_UnpipedMarkdownDefault(t *testing.T) {
	t.Parallel()
	store := newSearchableTodoStore(t)
	tool := NewTodoTool(store, "agent1")
	long := "*Long one*\n\n" + strings.Repeat("abcdefghi ", 100) // 1012 chars
	id, _ := store.Add("agent1", long, "medium", "")
	shortID, _ := store.Add("agent1", "short item", "medium", "")

	for _, action := range []map[string]interface{}{
		{"action": "list"},
		{"action": "search", "query": "abcdefghi short"},
	} {
		out, err := execTodoWithHints(t, tool, OutputHints{}, action)
		if err != nil {
			t.Fatalf("%v: %v", action["action"], err)
		}
		if strings.Contains(out, long) {
			t.Errorf("%v: unpiped output carries the full 1012-char text; want it cut at %d", action["action"], todoTruncateDefault)
		}
		kept := []rune(long)[:todoTruncateDefault]
		if !strings.Contains(out, string(kept)) {
			t.Errorf("%v: output lacks the first %d chars of the item", action["action"], todoTruncateDefault)
		}
		marker := fmt.Sprintf("[+%d chars: get %d]", len([]rune(long))-todoTruncateDefault, id)
		if !strings.Contains(out, marker) {
			t.Errorf("%v: missing truncation marker %q in:\n%s", action["action"], marker, out)
		}
		if strings.Contains(out, fmt.Sprintf("get %d]", shortID)) {
			t.Errorf("%v: a short item got a truncation marker", action["action"])
		}
	}
}

func TestTodoTruncate_ZeroAndExplicit(t *testing.T) {
	t.Parallel()
	store := newSearchableTodoStore(t)
	tool := NewTodoTool(store, "agent1")
	long := strings.Repeat("xyz ", 200) // 800 chars
	store.Add("agent1", long, "medium", "")

	out, _ := execTodoWithHints(t, tool, OutputHints{}, map[string]interface{}{"action": "list", "truncate": 0})
	if !strings.Contains(out, long) {
		t.Errorf("truncate=0 unpiped: want the full text, got:\n%s", out)
	}
	out, _ = execTodoWithHints(t, tool, OutputHints{}, map[string]interface{}{"action": "list", "truncate": 50})
	if strings.Contains(out, string([]rune(long)[:51])) || !strings.Contains(out, "[+750 chars") {
		t.Errorf("truncate=50 unpiped: want 50 chars and a +750 marker, got:\n%s", out)
	}
}

func TestTodoTruncate_PipedDefaultIsFull(t *testing.T) {
	t.Parallel()
	store := newSearchableTodoStore(t)
	tool := NewTodoTool(store, "agent1")
	longTitle := strings.Repeat("T", 150)
	body := strings.Repeat("word ", 200) + "\nsecond paragraph"
	store.Add("agent1", "*"+longTitle+"*\n\n"+body, "medium", "")

	for _, action := range []map[string]interface{}{
		{"action": "list"},
		{"action": "search", "query": "word"},
	} {
		out, err := execTodoWithHints(t, tool, pipedHints, action)
		if err != nil {
			t.Fatalf("%v: %v", action["action"], err)
		}
		objs := jsonlLines(t, out)
		if len(objs) == 0 {
			t.Fatalf("%v: no records: %s", action["action"], out)
		}
		if objs[0]["title"] != longTitle {
			t.Errorf("%v piped: title cut to %d runes; want it whole", action["action"], len([]rune(objs[0]["title"].(string))))
		}
		if objs[0]["body"] != body {
			t.Errorf("%v piped: body is not the full verbatim body (%d runes)", action["action"], len([]rune(objs[0]["body"].(string))))
		}

		action["truncate"] = 40
		out, _ = execTodoWithHints(t, tool, pipedHints, action)
		objs = jsonlLines(t, out)
		if got := []rune(objs[0]["body"].(string)); len(got) > 41 || got[len(got)-1] != '…' {
			t.Errorf("%v piped truncate=40: body = %d runes %q, want ≤40 + …", action["action"], len(got), string(got))
		}
	}
}
