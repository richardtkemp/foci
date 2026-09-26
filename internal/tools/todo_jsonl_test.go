package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/memory"
)

// execTodoWithHints runs the todo tool with output hints on the context, the
// way the exec bridge does for a foci_todo call whose stdout is piped (#2048).
func execTodoWithHints(t *testing.T, tool *Tool, h OutputHints, params map[string]interface{}) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(params)
	res, err := tool.Execute(WithOutputHints(context.Background(), h), raw)
	return res.Text, err
}

// jsonlLines splits a JSONL result into decoded objects, failing on any line
// that is not a JSON object — the whole point of the format is that every line
// stands alone for head/grep/jq.
func jsonlLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	if out == "" {
		return nil
	}
	// Every line newline-terminated, the last included: without the final
	// newline `wc -l` undercounts by one and `while read` drops the last item.
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("JSONL output must end with a newline: %q", out)
	}
	var objs []map[string]any
	for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d is not a JSON object: %v\n  line: %q\n  full: %s", i, err, line, out)
		}
		objs = append(objs, m)
	}
	return objs
}

var pipedHints = OutputHints{StdoutPiped: true}

// TestTodoJSONL_ListShape pins the per-item JSONL record: one object per line,
// the ticket's field set, tags as an array, and the bold "*Title*" headline
// split out from the body excerpt.
func TestTodoJSONL_ListShape(t *testing.T) {
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	longBody := strings.Repeat("word ", 200) // 1000 chars: must be excerpted
	id1, _ := store.Add("agent1", "*Ship the thing*\n\n"+longBody, "high", "foci,tools")
	id2, _ := store.Add("agent1", "plain item\nsecond line <a> & b", "medium", "")

	out, err := execTodoWithHints(t, tool, pipedHints, map[string]interface{}{"action": "list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	objs := jsonlLines(t, out)
	if len(objs) != 2 {
		t.Fatalf("want 2 lines (one per item), got %d:\n%s", len(objs), out)
	}
	byID := map[int64]map[string]any{}
	for _, o := range objs {
		for _, k := range []string{"id", "status", "priority", "tags", "title", "created_at", "updated_at", "body"} {
			if _, ok := o[k]; !ok {
				t.Errorf("record missing %q: %v", k, o)
			}
		}
		byID[int64(o["id"].(float64))] = o
	}

	a := byID[id1]
	if a["title"] != "Ship the thing" {
		t.Errorf("title = %q, want the bold headline without its asterisks", a["title"])
	}
	if a["status"] != "open" || a["priority"] != "high" {
		t.Errorf("status/priority = %v/%v", a["status"], a["priority"])
	}
	tags, _ := a["tags"].([]any)
	if len(tags) != 2 || tags[0] != "foci" || tags[1] != "tools" {
		t.Errorf("tags = %#v, want [foci tools]", a["tags"])
	}
	body, _ := a["body"].(string)
	if body == "" || len([]rune(body)) > todoJSONLBodyExcerpt+1 || !strings.HasSuffix(body, "…") {
		t.Errorf("body should be a short excerpt ending in …, got %d runes: %q", len([]rune(body)), body)
	}
	if strings.Contains(body, "Ship the thing") {
		t.Errorf("body excerpt repeats the title: %q", body)
	}

	b := byID[id2]
	// Literal angle brackets/ampersand in the raw line (not < escapes), so grep finds them.
	if !strings.Contains(out, "second line <a> & b") {
		t.Errorf("JSONL escapes HTML characters, breaking grep: %s", out)
	}
	if b["title"] != "plain item" || b["body"] != "second line <a> & b" {
		t.Errorf("untitled item: title=%q body=%q, want first line / rest", b["title"], b["body"])
	}
	if tags, _ := b["tags"].([]any); tags == nil || len(tags) != 0 {
		t.Errorf("untagged item: tags = %#v, want [] (not null)", b["tags"])
	}
}

// TestTodoJSONL_UnpipedOutputUnchanged: the markdown a plain (unpiped) call
// returns must be byte-identical with the hint absent, explicitly false, or an
// explicit --format md that overrides a pipe.
func TestTodoJSONL_UnpipedOutputUnchanged(t *testing.T) {
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")
	id, _ := store.Add("agent1", "*T*\n\nbody", "high", "x")

	for _, params := range []map[string]interface{}{
		{"action": "list"},
		{"action": "list", "status": "all"},
		{"action": "get", "id": id},
	} {
		want, err := executeTodoTool(tool, params)
		if err != nil {
			t.Fatalf("%v: %v", params, err)
		}
		for _, h := range []OutputHints{{}, {StdoutPiped: false}, {StdoutPiped: true, Format: "md"}, {Format: "md"}} {
			got, err := execTodoWithHints(t, tool, h, params)
			if err != nil {
				t.Fatalf("%v %+v: %v", params, h, err)
			}
			if got != want {
				t.Errorf("%v with hints %+v changed the markdown:\n got: %q\nwant: %q", params, h, got, want)
			}
		}
	}
}

// TestTodoJSONL_FormatOverride: an explicit --format jsonl wins on an unpiped call.
func TestTodoJSONL_FormatOverride(t *testing.T) {
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")
	store.Add("agent1", "one", "medium", "")

	out, err := execTodoWithHints(t, tool, OutputHints{Format: "jsonl"}, map[string]interface{}{"action": "list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if objs := jsonlLines(t, out); len(objs) != 1 {
		t.Fatalf("want 1 JSONL line, got: %s", out)
	}

	if _, err := execTodoWithHints(t, tool, OutputHints{Format: "yaml"}, map[string]interface{}{"action": "list"}); err == nil ||
		!strings.Contains(err.Error(), "yaml") {
		t.Errorf("unknown format must be rejected naming the value, got err=%v", err)
	}
}

// TestTodoJSONL_GetIsOneObject: get emits exactly one object carrying the FULL
// body (no excerpting — get is the full-detail view) plus close metadata.
func TestTodoJSONL_GetIsOneObject(t *testing.T) {
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")
	longBody := strings.Repeat("abc ", 300)
	id, _ := store.Add("agent1", "*Title here*\n\n"+longBody, "low", "a")
	if err := store.Transition("agent1", id, "done", "landed abc123"); err != nil {
		t.Fatal(err)
	}

	out, err := execTodoWithHints(t, tool, pipedHints, map[string]interface{}{"action": "get", "id": id})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("get must be a single line, got:\n%s", out)
	}
	o := jsonlLines(t, out)[0]
	if o["title"] != "Title here" || o["body"] != longBody {
		t.Errorf("get: title=%q, body len %d (want the full %d-char body)", o["title"], len(o["body"].(string)), len(longBody))
	}
	if o["status"] != "done" || o["close_reason"] != "landed abc123" || o["closed_at"] == nil {
		t.Errorf("get: close metadata missing: %v", o)
	}
}

// TestTodoJSONL_ListAllAndEmpty: list-all (status=all) includes closed items
// with their reason; an empty result is zero lines, not a prose sentence that
// would break `| jq` or inflate `| wc -l`.
func TestTodoJSONL_ListAllAndEmpty(t *testing.T) {
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")

	out, err := execTodoWithHints(t, tool, pipedHints, map[string]interface{}{"action": "list"})
	if err != nil {
		t.Fatalf("empty list: %v", err)
	}
	if out != "" {
		t.Errorf("empty JSONL list = %q, want empty output", out)
	}

	id, _ := store.Add("agent1", "gone", "medium", "")
	store.Add("agent1", "still here", "medium", "")
	if err := store.Transition("agent1", id, "dropped", "obsolete"); err != nil {
		t.Fatal(err)
	}
	out, err = execTodoWithHints(t, tool, pipedHints, map[string]interface{}{"action": "list", "status": "all"})
	if err != nil {
		t.Fatalf("list-all: %v", err)
	}
	objs := jsonlLines(t, out)
	if len(objs) != 2 {
		t.Fatalf("list-all: want 2 lines, got:\n%s", out)
	}
	var sawDropped bool
	for _, o := range objs {
		if o["status"] == "dropped" {
			sawDropped = true
			if o["close_reason"] != "obsolete" {
				t.Errorf("dropped item lacks close_reason: %v", o)
			}
		}
	}
	if !sawDropped {
		t.Errorf("list-all did not include the dropped item:\n%s", out)
	}
}

// TestTodoJSONL_TruncationIsItsOwnObject: #1957's capped-result notice must
// survive the format change, but as a JSON line of its own so every line still
// parses.
func TestTodoJSONL_TruncationIsItsOwnObject(t *testing.T) {
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")
	for i := 0; i < 5; i++ {
		store.Add("agent1", "Task", "medium", "")
	}
	out, err := execTodoWithHints(t, tool, pipedHints, map[string]interface{}{"action": "list", "limit": 2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	objs := jsonlLines(t, out)
	if len(objs) != 3 {
		t.Fatalf("want 2 items + 1 truncation line, got:\n%s", out)
	}
	last := objs[2]
	if last["truncated"] != true || last["shown"] != float64(2) || last["total"] != float64(5) {
		t.Errorf("truncation line = %v, want truncated/shown=2/total=5", last)
	}
	if _, isItem := last["id"]; isItem {
		t.Errorf("truncation line must not look like an item: %v", last)
	}
}

// TestTodoJSONL_Search covers the search action, which has its own result path.
func TestTodoJSONL_Search(t *testing.T) {
	t.Parallel()
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
	defer idx.Close()
	store := newTestTodoStore(t)
	store.SetSearchIndex(idx)
	tool := NewTodoTool(store, "agent1")
	store.Add("agent1", "Deploy the new release", "medium", "ops")
	store.Add("agent1", "Deploy docs", "low", "")

	out, err := execTodoWithHints(t, tool, pipedHints, map[string]interface{}{"action": "search", "query": "deploy"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if objs := jsonlLines(t, out); len(objs) != 2 {
		t.Fatalf("search: want 2 JSONL lines, got:\n%s", out)
	}

	none, err := execTodoWithHints(t, tool, pipedHints, map[string]interface{}{"action": "search", "query": "zzznomatch"})
	if err != nil {
		t.Fatalf("search none: %v", err)
	}
	if none != "" {
		t.Errorf("no-match JSONL search = %q, want empty output", none)
	}
}

// TestTodoJSONL_MutationsIgnoreHint: only the read actions have a JSONL form;
// add/complete keep their one-line confirmations when piped.
func TestTodoJSONL_MutationsIgnoreHint(t *testing.T) {
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent1")
	out, err := execTodoWithHints(t, tool, pipedHints, map[string]interface{}{"action": "add", "text": "x"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.HasPrefix(out, "Added #") {
		t.Errorf("piped add changed its output: %q", out)
	}
}
