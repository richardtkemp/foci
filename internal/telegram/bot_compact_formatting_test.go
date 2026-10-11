package telegram

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestFormatToolCallCompact(t *testing.T) {
	// Verifies that formatToolCallCompact produces
	// compact, emoji-enhanced tool call summaries.
	tests := []struct {
		name     string
		tool     string
		params   string
		contains string // expected substring in output
		emoji    string // expected per-tool emoji
	}{
		{"shell", "shell", `{"command":"ls -la /tmp"}`, "ls -la /tmp", "▶️"},
		{"web_search", "web_search", `{"query":"golang generics"}`, "golang generics", "🔍"},
		{"web_fetch", "web_fetch", `{"url":"https://example.com/page"}`, "https://example.com/page", "🔗"},
		{"http_request GET", "http_request", `{"url":"https://api.example.com/v1"}`, "GET https://api.example.com/v1", "🌍"},
		{"http_request POST", "http_request", `{"method":"POST","url":"https://api.example.com/v1"}`, "POST https://api.example.com/v1", "🌍"},
		{"read", "read", `{"path":"/home/user/file.txt"}`, "/home/user/file.txt", "📖"},
		{"tmux watch", "tmux", `{"operation":"watch","name":"cc-bash","threshold_seconds":30}`, "watch cc-bash", "🪟"},
		{"todo add", "todo", `{"action":"add","text":"buy milk"}`, "add", "☑️"},
		{"send_to_chat", "send_to_chat", `{"text":"hello world, how are you doing today?"}`, "hello world", "📨"},
		{"spawn", "spawn", `{"prompt":"summarize this document please"}`, "summarize this document", "🐣"},
		{"memory_search", "memory_search", `{"query":"project setup"}`, "project setup", "🧠"},
		{"unknown tool", "custom_tool", `{"foo":"bar value"}`, "bar value", "🔧"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatToolCallCompact(tt.tool, json.RawMessage(tt.params))
			if !strings.Contains(result, tt.emoji) {
				t.Errorf("expected emoji %s in %q", tt.emoji, result)
			}
			if !strings.Contains(result, tt.tool) {
				t.Errorf("missing tool name in %q", result)
			}
			if !strings.Contains(result, tt.contains) {
				t.Errorf("expected %q in %q", tt.contains, result)
			}
			// Should NOT contain <pre> block (that's the full format)
			if strings.Contains(result, "<pre>") {
				t.Errorf("compact format should not contain <pre>, got: %s", result)
			}
		})
	}
}

func TestFormatToolCallCompact_LoadedSkill(t *testing.T) {
	// Proves a Read of a SKILL.md renders the whole compact line as the
	// "📖 Loaded skill: <dir>" chip (#1299) for both read forms that reach
	// the formatter — ccstream's Read/file_path and the API transport's
	// read/path — while a read of a file inside a skill directory keeps the
	// generic summary, and a directory name with HTML specials is escaped.
	tests := []struct {
		name   string
		tool   string
		params string
		want   string
	}{
		{"ccstream Read form", "Read", `{"file_path":"/home/foci/shared/skills/writing-code/SKILL.md"}`, "📖 Loaded skill: writing-code"},
		{"API read form", "read", `{"path":"/home/foci/shared/skills/writing-code/SKILL.md"}`, "📖 Loaded skill: writing-code"},
		{"relative path", "read", `{"path":"./shared/skills/writing-code/SKILL.md"}`, "📖 Loaded skill: writing-code"},
		{"HTML-special dir name", "Read", `{"file_path":"/home/foci/shared/skills/a&b/SKILL.md"}`, "📖 Loaded skill: a&amp;b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatToolCallCompact(tt.tool, json.RawMessage(tt.params)); got != tt.want {
				t.Errorf("formatToolCallCompact(%s) = %q, want %q", tt.tool, got, tt.want)
			}
		})
	}

	got := formatToolCallCompact("read", json.RawMessage(`{"path":"/home/foci/shared/skills/writing-code/go.md"}`))
	if want := "📖 <b>read</b>: /home/foci/shared/skills/writing-code/go.md"; got != want {
		t.Errorf("sub-file read = %q, want generic summary %q", got, want)
	}
}

func TestFormatToolCallCompact_OrdinaryReadSummary(t *testing.T) {
	// CHARACTERISATION: pins the pre-#1299 compact line for reads of
	// ordinary files, in both read forms, so the chip substitution cannot
	// leak beyond SKILL.md reads.
	if got := formatToolCallCompact("read", json.RawMessage(`{"path":"/home/foci/notes.txt"}`)); got != "📖 <b>read</b>: /home/foci/notes.txt" {
		t.Errorf("ordinary read = %q, want %q", got, "📖 <b>read</b>: /home/foci/notes.txt")
	}
	if got := formatToolCallCompact("Read", json.RawMessage(`{"file_path":"/home/foci/notes.txt"}`)); got != "🔧 <b>Read</b>: /home/foci/notes.txt" {
		t.Errorf("ordinary CC Read = %q, want %q", got, "🔧 <b>Read</b>: /home/foci/notes.txt")
	}
}

func TestFormatToolCallCompact_HTMLEscape(t *testing.T) {
	// Verifies that HTML is properly escaped
	// in compact tool call messages.
	result := formatToolCallCompact("shell", json.RawMessage(`{"command":"echo <script>"}`))
	if strings.Contains(result, "<script>") {
		t.Errorf("HTML not escaped in %q", result)
	}
	if !strings.Contains(result, "&lt;script&gt;") {
		t.Errorf("expected escaped HTML in %q", result)
	}
}

func TestFormatToolCallCompact_Truncation(t *testing.T) {
	// Verifies that long compact messages
	// are truncated with ellipsis.
	longCmd := strings.Repeat("x", 200)
	result := formatToolCallCompact("shell", json.RawMessage(fmt.Sprintf(`{"command":"%s"}`, longCmd)))
	// Should be truncated to ~60 chars + "..."
	if !strings.Contains(result, "...") {
		t.Errorf("long command should be truncated: %s", result)
	}
}

func TestFormatToolCallCompact_EmptyParams(t *testing.T) {
	// Verifies that compact format handles
	// empty parameters correctly.
	result := formatToolCallCompact("unknown", json.RawMessage(`{}`))
	// Should just be the tool name with no summary; unknown tool gets 🔧
	if !strings.Contains(result, "🔧") {
		t.Error("missing fallback tool emoji")
	}
	if strings.Contains(result, ":") {
		t.Errorf("empty params should not have colon separator, got: %s", result)
	}
}

func TestCompactResultHint(t *testing.T) {
	// Verifies that compactResultHint extracts meaningful key info
	// from tool results to display inline in compact notifications.
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
		{"unknown tool", "web_fetch", `{"url":"x"}`, "some content", ""},
		{"tmux read lines", "tmux", `{"operation":"read","name":"cc-main"}`, "line1\nline2\nline3", "3 lines"},
		{"tmux read single", "tmux", `{"operation":"read","name":"cc-main"}`, "single line", "1 line"},
		{"tmux read empty", "tmux", `{"operation":"read","name":"cc-main"}`, "", "(empty)"},
		{"tmux start", "tmux", `{"operation":"start","name":"cc-main"}`, "Session started: cc-main", "Session started: cc-main"},
		{"tmux kill", "tmux", `{"operation":"kill","name":"cc-main"}`, "Session killed: cc-main", "Session killed: cc-main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compactResultHint(tt.tool, json.RawMessage(tt.params), tt.result)
			if got != tt.want {
				t.Errorf("compactResultHint(%s) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}
