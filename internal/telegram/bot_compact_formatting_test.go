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
		{"send_message_to_user", "send_message_to_user", `{"text":"hello world, how are you doing today?"}`, "hello world", "📨"},
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
		{"todo list items", "todo", `{"action":"list"}`, "- **#1** [ ] buy milk `med`\n- **#2** [ ] fix bug `high`", "2 items"},
		{"todo list single", "todo", `{"action":"list"}`, "- **#1** [ ] buy milk `med`", "1 item"},
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
