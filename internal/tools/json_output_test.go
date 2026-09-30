package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"foci/internal/memory"
)

// jsonCtx is the context an exec-bridge call made with --json runs under.
func jsonCtx() context.Context {
	return WithOutputHints(context.Background(), OutputHints{Format: OutputFormatJSON})
}

// decodeJSONResult asserts r is a --json result and decodes it into v.
func decodeJSONResult(t *testing.T, r ToolResult, v any) {
	t.Helper()
	if !r.JSON {
		t.Fatalf("result not flagged JSON: %q", r.Text)
	}
	if err := json.Unmarshal([]byte(r.Text), v); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, r.Text)
	}
}

func TestWebFetchJSONOutput(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><p>Hello <b>World</b> &amp; more</p></body></html>"))
	}))
	defer srv.Close()
	params, _ := json.Marshal(map[string]any{"url": srv.URL + "/old"})

	text, err := NewWebFetchTool().Execute(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if text.JSON || !strings.Contains(text.Text, "Hello") {
		t.Fatalf("text form changed: %+v", text)
	}

	r, err := NewWebFetchTool().Execute(jsonCtx(), params)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	decodeJSONResult(t, r, &got)
	if got["url"] != srv.URL+"/old" || got["final_url"] != srv.URL+"/new" || got["status"] != float64(200) ||
		got["content_type"] != "text/html" || got["content"] != text.Text {
		t.Errorf("got %v, want the text output as content, and the redirect target as final_url", got)
	}
}

func TestHTTPRequestJSONOutput(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Add("X-Multi", "a")
		w.Header().Add("X-Multi", "b")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(strings.Repeat("z", 64)))
	}))
	defer srv.Close()
	newTool := func(ceiling int64) *Tool {
		return NewHTTPRequestTool(nil, nil, t.TempDir(), func() int { return 0 }, func() int64 { return 1 << 20 }, func() int64 { return ceiling }, func() int64 { return 0 }, nil, 0640)
	}

	t.Run("inline", func(t *testing.T) {
		params, _ := json.Marshal(map[string]any{"url": srv.URL})
		r, err := newTool(0).Execute(jsonCtx(), params)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		decodeJSONResult(t, r, &got)
		if got["status"] != float64(418) || got["status_text"] != "418 I'm a teapot" || got["body"] != strings.Repeat("z", 64) {
			t.Errorf("got %v", got)
		}
		multi, _ := got["headers"].(map[string]any)["X-Multi"].([]any)
		if len(multi) != 2 || multi[0] != "a" || multi[1] != "b" {
			t.Errorf("headers.X-Multi = %v, want [a b]", multi)
		}
		if _, ok := got["body_file"]; ok || r.ResultFile != "" {
			t.Errorf("small body must not name a spill file: %v / %q", got, r.ResultFile)
		}
	})

	t.Run("spilled", func(t *testing.T) {
		// Over the 1MB inline preview: the full body is on disk and named in
		// the JSON, never streamed raw in the JSON's place.
		const size = 1<<20 + 100
		big := bigBodyServer(t, size)
		defer big.Close()
		params, _ := json.Marshal(map[string]any{"url": big.URL})
		r, err := newTool(4<<20).Execute(jsonCtx(), params)
		if err != nil {
			t.Fatal(err)
		}
		if r.ResultFile != "" {
			t.Errorf("--json result must not set ResultFile (foci-call would stream the raw body): %q", r.ResultFile)
		}
		var got map[string]any
		decodeJSONResult(t, r, &got)
		file, _ := got["body_file"].(string)
		data, err := os.ReadFile(file)
		if err != nil || len(data) != size || got["body_size"] != float64(size) || got["truncated"] != nil {
			t.Errorf("body_file=%q (%d bytes, err %v) body_size=%v truncated=%v, want the full %d-byte body on disk",
				file, len(data), err, got["body_size"], got["truncated"], size)
		}
		if body, _ := got["body"].(string); len(body) != 1<<20 {
			t.Errorf("inline body = %d bytes, want the 1MB preview", len(body))
		}
	})

	t.Run("save_to", func(t *testing.T) {
		dst := t.TempDir() + "/out.txt"
		params, _ := json.Marshal(map[string]any{"url": srv.URL, "save_to": dst})
		r, err := newTool(0).Execute(jsonCtx(), params)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		decodeJSONResult(t, r, &got)
		if got["saved_to"] != dst || got["saved_bytes"] != float64(64) || got["status"] != float64(418) {
			t.Errorf("got %v", got)
		}
	})
}

type fakeSearcher []memory.Result

func (f fakeSearcher) Search(string, string, *memory.SearchOptions) ([]memory.Result, error) {
	return f, nil
}

func TestMemorySearchJSONOutput(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tool := func(res fakeSearcher) *Tool {
		return NewMemorySearchTool(map[string]memory.Searcher{"fts5": res}, func() string { return "fts5" }, nil)
	}
	params := json.RawMessage(`{"query":"milk"}`)

	r, err := tool(fakeSearcher{
		{Path: "notes.md", Snippet: "buy <milk>", Source: "memory", Time: when},
		{Path: "agent/c1", Snippet: "said milk", Source: "conversation", RowID: 42},
	}).Execute(jsonCtx(), params)
	if err != nil {
		t.Fatal(err)
	}
	var hits []map[string]any
	decodeJSONResult(t, r, &hits)
	if len(hits) != 2 ||
		hits[0]["path"] != "notes.md" || hits[0]["snippet"] != "buy <milk>" || hits[0]["time"] != "2026-09-01T12:00:00Z" || hits[0]["ref"] != nil ||
		hits[1]["ref"] != "agent/c1#42" || hits[1]["row_id"] != float64(42) || hits[1]["source"] != "conversation" {
		t.Errorf("hits = %v", hits)
	}

	empty, err := tool(nil).Execute(jsonCtx(), params)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(empty.Text) != "[]" || !empty.JSON {
		t.Errorf("no matches under --json = %q, want []", empty.Text)
	}
	if plain, _ := tool(nil).Execute(context.Background(), params); plain.Text != "No matches found." {
		t.Errorf("text form changed: %q", plain.Text)
	}
}

func TestRemindJSONOutput(t *testing.T) {
	t.Parallel()
	tool, _ := testRemindToolWithCancel(t, func(int64) bool { return true })

	set, err := tool.Execute(jsonCtx(), json.RawMessage(`{"text":"stretch","when":"2h"}`))
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	decodeJSONResult(t, set, &s)
	if s["set"] != "reminder" || s["when"] != "2h" || s["text"] != "stretch" {
		t.Errorf("passive set = %v", s)
	}

	long := strings.Repeat("x", 100) // text output truncates at 70; JSON must not
	wake, err := tool.Execute(jsonCtx(), json.RawMessage(`{"text":"`+long+`","when":"1h","wake":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var w map[string]any
	decodeJSONResult(t, wake, &w)
	if w["set"] != "wake" || w["text"] != long || w["id"] == nil || w["due_at"] == nil {
		t.Errorf("wake set = %v", w)
	}

	list, err := tool.Execute(jsonCtx(), json.RawMessage(`{"list":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var l []map[string]any
	decodeJSONResult(t, list, &l)
	if len(l) != 1 || l[0]["text"] != long || l[0]["id"] != w["id"] {
		t.Errorf("list = %v", l)
	}

	id, _ := json.Marshal(w["id"])
	cancel, err := tool.Execute(jsonCtx(), json.RawMessage(`{"cancel":`+string(id)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	decodeJSONResult(t, cancel, &c)
	if c["cancelled"] != w["id"] || c["text"] != long {
		t.Errorf("cancel = %v", c)
	}
}
