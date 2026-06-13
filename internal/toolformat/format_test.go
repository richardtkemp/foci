package toolformat

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCompactSummary verifies that CompactSummary extracts the most meaningful
// param values for each known tool type, and falls back to the first string
// param for unknown tools.
func TestCompactSummary(t *testing.T) {
	tests := []struct {
		name string
		tool string
		m    map[string]json.RawMessage
		want string
	}{
		{"shell", "shell", raw(map[string]string{"command": "ls -la /tmp"}), "ls -la /tmp"},
		{"web_fetch", "web_fetch", raw(map[string]string{"url": "https://example.com/page"}), "https://example.com/page"},
		{"web_search", "web_search", raw(map[string]string{"query": "golang generics"}), "golang generics"},
		{"memory_search", "memory_search", raw(map[string]string{"query": "project setup"}), "project setup"},
		{"http_request default GET", "http_request", raw(map[string]string{"url": "https://api.example.com/v1"}), "GET https://api.example.com/v1"},
		{"http_request POST", "http_request", raw(map[string]string{"method": "POST", "url": "https://api.example.com/v1"}), "POST https://api.example.com/v1"},
		{"read", "read", raw(map[string]string{"path": "/home/user/file.txt"}), "/home/user/file.txt"},
		{"write", "write", raw(map[string]string{"path": "/tmp/out.txt"}), "/tmp/out.txt"},
		{"edit", "edit", raw(map[string]string{"path": "/tmp/file.go"}), "/tmp/file.go"},
		{"tmux op+name", "tmux", raw(map[string]string{"operation": "watch", "name": "cc-bash"}), "watch cc-bash"},
		{"tmux op only", "tmux", raw(map[string]string{"operation": "list"}), "list"},
		{"tmux name only", "tmux", raw(map[string]string{"name": "cc-main"}), "cc-main"},
		{"todo", "todo", raw(map[string]string{"action": "add"}), "add"},
		{"scratchpad", "scratchpad", raw(map[string]string{"action": "read"}), "read"},
		{"remind", "remind", raw(map[string]string{"text": "remember this"}), "remember this"},
		{"send_to_chat", "send_to_chat", raw(map[string]string{"text": "hello world"}), "hello world"},
		{"spawn", "spawn", raw(map[string]string{"prompt": "summarize this"}), "summarize this"},
		{"unknown fallback", "custom_tool", raw(map[string]string{"foo": "bar value"}), "bar value"},
		{"empty params", "unknown", map[string]json.RawMessage{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CompactSummary(tt.tool, tt.m)
			if got != tt.want {
				t.Errorf("CompactSummary(%s) = %q, want %q", tt.tool, got, tt.want)
			}
		})
	}
}

// TestCompactSummaryTruncation verifies that long values are truncated with
// ellipsis at the appropriate length for each tool type.
func TestCompactSummaryTruncation(t *testing.T) {
	long := strings.Repeat("x", 200)
	got := CompactSummary("shell", raw(map[string]string{"command": long}))
	if len(got) > 63 { // 60 + "..."
		t.Errorf("shell command not truncated: len=%d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected ellipsis, got %q", got)
	}
}

// TestCompactResultHint verifies that CompactResultHint dispatches to the
// correct per-tool hint extractor and returns meaningful hints.
func TestCompactResultHint(t *testing.T) {
	tests := []struct {
		name   string
		tool   string
		params string
		result string
		want   string
	}{
		{"todo add", "todo", `{"action":"add","text":"buy milk"}`, "Added #542 (medium)", "#542"},
		{"todo add high", "todo", `{"action":"add","text":"urgent"}`, "Added #1 (high)", "#1"},
		{"todo list items", "todo", `{"action":"list"}`, "**#1** `med` — *2h ago*\nbuy milk\n---\n**#2** `high` — *1h ago*\nfix bug", "2 items"},
		{"todo list single", "todo", `{"action":"list"}`, "**#1** `med` — *2h ago*\nbuy milk", "1 item"},
		{"todo list empty", "todo", `{"action":"list"}`, "No active todos.", "0 items"},
		{"todo search empty", "todo", `{"action":"search","query":"x"}`, "No todos matching \"x\".", "0 items"},
		{"todo transition", "todo", `{"action":"transition","id":5,"state":"done"}`, "#5: done", "done"},
		{"todo edit", "todo", `{"action":"edit","id":3,"text":"new"}`, "#3: text: old → new", "#3"},
		{"todo remove", "todo", `{"action":"remove","id":7}`, "#7: removed", ""},
		{"shell multiline", "shell", `{"command":"ls"}`, "a\nb\nc\nd\ne", "5 lines"},
		{"shell short", "shell", `{"command":"pwd"}`, "/home/user", ""},
		{"shell empty", "shell", `{"command":"true"}`, "", "(empty)"},
		{"write", "write", `{"path":"f.go","content":"x"}`, "Wrote 42 bytes to f.go", "42 bytes"},
		{"edit applied", "edit", `{"path":"f.go"}`, "Applied 1 edit to f.go", "Applied 1 edit to f.go"},
		{"spawn", "spawn", `{"prompt":"do stuff"}`, "Spawned agent: helper", "Spawned agent: helper"},
		{"spawn no prefix", "spawn", `{"prompt":"x"}`, "other result", ""},
		{"tmux read lines", "tmux", `{"operation":"read","name":"cc-main"}`, "line1\nline2\nline3", "3 lines"},
		{"tmux read single", "tmux", `{"operation":"read","name":"cc-main"}`, "single line", "1 line"},
		{"tmux read empty", "tmux", `{"operation":"read","name":"cc-main"}`, "", "(empty)"},
		{"tmux start", "tmux", `{"operation":"start","name":"cc-main"}`, "Session started: cc-main", "Session started: cc-main"},
		{"tmux kill", "tmux", `{"operation":"kill","name":"cc-main"}`, "Session killed: cc-main", "Session killed: cc-main"},
		{"unknown tool", "web_fetch", `{"url":"x"}`, "some content", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CompactResultHint(tt.tool, json.RawMessage(tt.params), tt.result)
			if got != tt.want {
				t.Errorf("CompactResultHint(%s) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// TestTruncate verifies the Truncate helper for short, exact, and long strings.
func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"short", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"long", "hello world", 5, "hello..."},
		{"empty", "", 5, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Truncate(tt.s, tt.max)
			if got != tt.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
			}
		})
	}
}

// TestShellResultHint verifies edge cases for shell result hints.
func TestShellResultHint(t *testing.T) {
	if got := ShellResultHint(""); got != "(empty)" {
		t.Errorf("empty result: got %q, want \"(empty)\"", got)
	}
	if got := ShellResultHint("one line"); got != "" {
		t.Errorf("single line: got %q, want \"\"", got)
	}
	if got := ShellResultHint("a\nb\nc"); got != "" {
		t.Errorf("3 lines: got %q, want \"\"", got)
	}
	if got := ShellResultHint("a\nb\nc\nd"); got != "4 lines" {
		t.Errorf("4 lines: got %q, want \"4 lines\"", got)
	}
}

// TestWriteResultHint verifies byte count extraction from write results.
func TestWriteResultHint(t *testing.T) {
	if got := WriteResultHint("Wrote 42 bytes to f.go"); got != "42 bytes" {
		t.Errorf("got %q, want \"42 bytes\"", got)
	}
	if got := WriteResultHint("Error: file not found"); got != "" {
		t.Errorf("got %q, want \"\"", got)
	}
}

// TestEditResultHint verifies edit confirmation extraction.
func TestEditResultHint(t *testing.T) {
	if got := EditResultHint("Applied 1 edit to f.go"); got != "Applied 1 edit to f.go" {
		t.Errorf("got %q, want \"Applied 1 edit to f.go\"", got)
	}
	if got := EditResultHint("Edited file.go"); got != "Edited file.go" {
		t.Errorf("got %q, want \"Edited file.go\"", got)
	}
	if got := EditResultHint("Error: no match"); got != "" {
		t.Errorf("got %q, want \"\"", got)
	}
}

// TestSpawnResultHint verifies spawn result extraction.
func TestSpawnResultHint(t *testing.T) {
	if got := SpawnResultHint("Spawned agent: helper"); got != "Spawned agent: helper" {
		t.Errorf("got %q, want \"Spawned agent: helper\"", got)
	}
	if got := SpawnResultHint("other result"); got != "" {
		t.Errorf("got %q, want \"\"", got)
	}
}

// TestTmuxResultHint verifies tmux result hint extraction for all operations.
func TestTmuxResultHint(t *testing.T) {
	tests := []struct {
		name   string
		params string
		result string
		want   string
	}{
		{"read lines", `{"operation":"read","name":"cc"}`, "a\nb\nc", "3 lines"},
		{"read single", `{"operation":"read","name":"cc"}`, "single", "1 line"},
		{"read empty", `{"operation":"read","name":"cc"}`, "", "(empty)"},
		{"read whitespace", `{"operation":"read","name":"cc"}`, "  \n  ", "(empty)"},
		{"start", `{"operation":"start","name":"cc"}`, "Session started: cc", "Session started: cc"},
		{"kill", `{"operation":"kill","name":"cc"}`, "Session killed: cc\nmore", "Session killed: cc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TmuxResultHint(json.RawMessage(tt.params), tt.result)
			if got != tt.want {
				t.Errorf("TmuxResultHint(%s) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// TestTodoResultHint verifies all todo action branches.
func TestTodoResultHint(t *testing.T) {
	tests := []struct {
		name   string
		params string
		result string
		want   string
	}{
		{"add with space", `{"action":"add"}`, "Added #10 (med)", "#10"},
		{"add no space", `{"action":"add"}`, "Added #10", "#10"},
		{"list 0", `{"action":"list"}`, "No items", "0 items"},
		{"list 2", `{"action":"list"}`, "a\n---\nb", "2 items"},
		{"search 0", `{"action":"search"}`, "No results", "0 items"},
		{"transition", `{"action":"transition"}`, "#5: done", "done"},
		{"remove", `{"action":"remove"}`, "#7: removed", ""},
		{"edit", `{"action":"edit"}`, "#3: text: old", "#3"},
		{"unknown action", `{"action":"unknown"}`, "whatever", ""},
		{"bad json", `invalid`, "whatever", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TodoResultHint(json.RawMessage(tt.params), tt.result)
			if got != tt.want {
				t.Errorf("TodoResultHint(%s) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// TestStrHelper verifies the internal str helper for various JSON value types.
func TestStrHelper(t *testing.T) {
	m := map[string]json.RawMessage{
		"string":  json.RawMessage(`"hello"`),
		"number":  json.RawMessage(`42`),
		"bool":    json.RawMessage(`true`),
		"missing": json.RawMessage(``), // won't be accessed
	}

	if got := str(m, "string"); got != "hello" {
		t.Errorf("string: got %q, want \"hello\"", got)
	}
	if got := str(m, "number"); got != "42" {
		t.Errorf("number: got %q, want \"42\"", got)
	}
	if got := str(m, "bool"); got != "true" {
		t.Errorf("bool: got %q, want \"true\"", got)
	}
	if got := str(m, "absent"); got != "" {
		t.Errorf("absent: got %q, want \"\"", got)
	}
}

// TestCompactSummaryFallback verifies that unknown tools with no matching case
// fall back to the first string-valued param in sorted key order.
func TestCompactSummaryFallback(t *testing.T) {
	// Keys sorted: "alpha" < "beta" — should use alpha's value
	m := map[string]json.RawMessage{
		"beta":  json.RawMessage(`"second"`),
		"alpha": json.RawMessage(`"first"`),
	}
	got := CompactSummary("unknown_tool", m)
	if got != "first" {
		t.Errorf("fallback sort: got %q, want \"first\"", got)
	}
}

// TestCompactSummaryFallbackSkipsEmpty verifies that the fallback skips empty
// string values and uses the next non-empty one.
func TestCompactSummaryFallbackSkipsEmpty(t *testing.T) {
	m := map[string]json.RawMessage{
		"alpha": json.RawMessage(`""`),
		"beta":  json.RawMessage(`"value"`),
	}
	got := CompactSummary("unknown_tool", m)
	if got != "value" {
		t.Errorf("fallback skip empty: got %q, want \"value\"", got)
	}
}

// raw is a test helper that builds a map[string]json.RawMessage from string pairs.
func raw(m map[string]string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		b, _ := json.Marshal(v)
		out[k] = b
	}
	return out
}
