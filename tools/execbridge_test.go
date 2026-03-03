package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
)

func testRegistry() *Registry {
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "echo_tool",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var p struct{ Text string `json:"text"` }
			json.Unmarshal(params, &p)
			return TextResult("echo: " + p.Text), nil
		},
	})
	r.Register(&Tool{
		Name:       "error_tool",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return ToolResult{}, fmt.Errorf("intentional error")
		},
	})
	r.Register(&Tool{
		Name:       "private_tool",
		ExecExport: false,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return TextResult("should not be callable"), nil
		},
	})
	return r
}

func TestExecBridgeLifecycle(t *testing.T) {
	r := testRegistry()
	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}

	// Check files exist
	if _, err := os.Stat(bridge.SockPath()); err != nil {
		t.Fatalf("socket file not found: %v", err)
	}
	if _, err := os.Stat(bridge.FuncsPath()); err != nil {
		t.Fatalf("funcs file not found: %v", err)
	}

	bridge.Close()

	// Check files cleaned up
	if _, err := os.Stat(bridge.SockPath()); !os.IsNotExist(err) {
		t.Errorf("socket file not cleaned up")
	}
	if _, err := os.Stat(bridge.FuncsPath()); !os.IsNotExist(err) {
		t.Errorf("funcs file not cleaned up")
	}
}

func TestExecBridgeCallTool(t *testing.T) {
	r := testRegistry()
	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	result, errMsg := callBridge(t, bridge.SockPath(), `{"tool":"echo_tool","params":{"text":"hello"}}`)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if result != "echo: hello" {
		t.Errorf("result = %q, want %q", result, "echo: hello")
	}
}

func TestExecBridgeToolError(t *testing.T) {
	r := testRegistry()
	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	_, errMsg := callBridge(t, bridge.SockPath(), `{"tool":"error_tool","params":{}}`)
	if errMsg == "" {
		t.Fatal("expected error from error_tool")
	}
	if !strings.Contains(errMsg, "intentional error") {
		t.Errorf("error = %q, want 'intentional error'", errMsg)
	}
}

func TestExecBridgePrivateToolRejected(t *testing.T) {
	r := testRegistry()
	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	_, errMsg := callBridge(t, bridge.SockPath(), `{"tool":"private_tool","params":{}}`)
	if errMsg == "" {
		t.Fatal("expected error for private tool")
	}
	if !strings.Contains(errMsg, "not exported") {
		t.Errorf("error = %q, want 'not exported'", errMsg)
	}
}

func TestExecBridgeUnknownTool(t *testing.T) {
	r := testRegistry()
	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	_, errMsg := callBridge(t, bridge.SockPath(), `{"tool":"nonexistent","params":{}}`)
	if errMsg == "" {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(errMsg, "unknown tool") {
		t.Errorf("error = %q, want 'unknown tool'", errMsg)
	}
}

func TestExecBridgeInvalidJSON(t *testing.T) {
	r := testRegistry()
	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	_, errMsg := callBridge(t, bridge.SockPath(), `{not valid json`)
	if errMsg == "" {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(errMsg, "invalid") {
		t.Errorf("error = %q, want 'invalid'", errMsg)
	}
}

func TestExecBridgeShellFuncsContent(t *testing.T) {
	r := testRegistry()
	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	data, err := os.ReadFile(bridge.FuncsPath())
	if err != nil {
		t.Fatalf("read funcs file: %v", err)
	}
	content := string(data)

	// Should contain function for echo_tool (ExecExport: true)
	if !strings.Contains(content, "foci_echo_tool()") {
		t.Error("funcs file should contain foci_echo_tool()")
	}
	// Should NOT contain function for private_tool (ExecExport: false)
	if strings.Contains(content, "foci_private_tool") {
		t.Error("funcs file should not contain foci_private_tool")
	}
}

func TestExecBridgeSessionKeyPropagated(t *testing.T) {
	// Verify that the session key from the bridge context reaches tool execution
	var capturedKey string
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "key_tool",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			capturedKey = SessionKeyFromContext(ctx)
			return TextResult("ok"), nil
		},
	})

	ctx := WithSessionKey(context.Background(), "agent:test:chat:123")
	bridge, err := NewExecBridge(r, ctx)
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	result, errMsg := callBridge(t, bridge.SockPath(), `{"tool":"key_tool","params":{}}`)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if result != "ok" {
		t.Errorf("result = %q", result)
	}
	if capturedKey != "agent:test:chat:123" {
		t.Errorf("session key = %q, want %q", capturedKey, "agent:test:chat:123")
	}
}

func TestExecBridgeConcurrentCalls(t *testing.T) {
	r := testRegistry()
	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	const n = 10
	results := make(chan string, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			msg := fmt.Sprintf("msg%d", i)
			req := fmt.Sprintf(`{"tool":"echo_tool","params":{"text":"%s"}}`, msg)
			result, errMsg := callBridge(t, bridge.SockPath(), req)
			if errMsg != "" {
				results <- "error: " + errMsg
				return
			}
			results <- result
		}(i)
	}

	for i := 0; i < n; i++ {
		r := <-results
		if !strings.HasPrefix(r, "echo: msg") {
			t.Errorf("unexpected result: %q", r)
		}
	}
}

func TestExecBridgeUniquePaths(t *testing.T) {
	r := testRegistry()
	b1, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge 1: %v", err)
	}
	b2, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge 2: %v", err)
	}

	if b1.SockPath() == b2.SockPath() {
		t.Error("two bridges should have unique socket paths")
	}

	b1.Close()
	b2.Close()
}

func TestStripHTTPHeaders(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "standard response",
			input: "HTTP 200 OK\nContent-Type: application/json\n\n{\"key\":\"value\"}",
			want:  "{\"key\":\"value\"}",
		},
		{
			name:  "multiple headers",
			input: "HTTP 200 OK\nContent-Type: text/html\nContent-Length: 5\nX-Request-Id: abc\n\nhello",
			want:  "hello",
		},
		{
			name:  "no headers (not HTTP prefix)",
			input: "just a plain result",
			want:  "just a plain result",
		},
		{
			name:  "empty body",
			input: "HTTP 204 No Content\n\n",
			want:  "",
		},
		{
			name:  "body with newlines",
			input: "HTTP 200 OK\nContent-Type: text/plain\n\nline1\nline2\nline3",
			want:  "line1\nline2\nline3",
		},
		{
			name:  "saved to file (no body separator)",
			input: "HTTP 200 OK\nContent-Type: image/png\n\nSaved 1234 bytes to /tmp/foo.png",
			want:  "Saved 1234 bytes to /tmp/foo.png",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripHTTPHeaders(tt.input)
			if got != tt.want {
				t.Errorf("stripHTTPHeaders() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestToolParamKeys(t *testing.T) {
	tests := []struct {
		name   string
		params json.RawMessage
		want   string
	}{
		{
			name:   "single key",
			params: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
			want:   "query",
		},
		{
			name:   "multiple keys sorted",
			params: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"},"method":{"type":"string"},"body":{"type":"string"}}}`),
			want:   "body method url",
		},
		{
			name:   "empty properties",
			params: json.RawMessage(`{"type":"object","properties":{}}`),
			want:   "",
		},
		{
			name:   "invalid JSON",
			params: json.RawMessage(`not json`),
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := &Tool{Parameters: tt.params}
			got := toolParamKeys(tool)
			if got != tt.want {
				t.Errorf("toolParamKeys() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShellFuncsContainJSONGuard(t *testing.T) {
	r := testRegistry()
	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	data, err := os.ReadFile(bridge.FuncsPath())
	if err != nil {
		t.Fatalf("read funcs file: %v", err)
	}
	content := string(data)

	// Should contain the _foci_json helper
	if !strings.Contains(content, "_foci_json()") {
		t.Error("funcs file should contain _foci_json() helper")
	}
	// Should contain export -f for the helper
	if !strings.Contains(content, "export -f _foci_json") {
		t.Error("funcs file should export _foci_json")
	}
	// echo_tool should have a guard line with its valid key "text"
	if !strings.Contains(content, `_foci_json "echo_tool" "text"`) {
		t.Error("echo_tool guard should include valid key 'text'")
	}
}

func TestExecBridgeHTTPRequestHeadersStripped(t *testing.T) {
	// Register a fake http_request tool that returns headers + body
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "http_request",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return TextResult("HTTP 200 OK\nContent-Type: application/json\nContent-Length: 27\n\n{\"origin\":\"1.2.3.4\"}"), nil
		},
	})

	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	result, errMsg := callBridge(t, bridge.SockPath(), `{"tool":"http_request","params":{"url":"https://httpbin.org/get"}}`)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	// Headers should be stripped — result should be body only
	if strings.Contains(result, "HTTP 200") {
		t.Errorf("result should not contain HTTP headers, got: %q", result)
	}
	if result != `{"origin":"1.2.3.4"}` {
		t.Errorf("result = %q, want %q", result, `{"origin":"1.2.3.4"}`)
	}
}

func TestExecBridgeHTTPRequestIncludeHeaders(t *testing.T) {
	// When include_headers is true, the full response (status + headers + body) is returned
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "http_request",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return TextResult("HTTP 200 OK\nContent-Type: application/json\n\n{\"key\":\"value\"}"), nil
		},
	})

	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	// Without include_headers — body only (existing behavior)
	result, errMsg := callBridge(t, bridge.SockPath(), `{"tool":"http_request","params":{"url":"https://example.com"}}`)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if result != `{"key":"value"}` {
		t.Errorf("default result = %q, want body only", result)
	}

	// With include_headers: true — full response
	result, errMsg = callBridge(t, bridge.SockPath(), `{"tool":"http_request","params":{"url":"https://example.com"},"include_headers":true}`)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if !strings.HasPrefix(result, "HTTP 200 OK") {
		t.Errorf("include_headers result should start with HTTP status, got: %q", result)
	}
	if !strings.Contains(result, "Content-Type: application/json") {
		t.Errorf("include_headers result should contain headers, got: %q", result)
	}
	if !strings.Contains(result, `{"key":"value"}`) {
		t.Errorf("include_headers result should contain body, got: %q", result)
	}
}

func TestExecBridgeHTTPRequestIncludeHeadersFalse(t *testing.T) {
	// Explicitly passing include_headers: false should strip headers (same as default)
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "http_request",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return TextResult("HTTP 404 Not Found\nContent-Type: text/plain\n\nnot found"), nil
		},
	})

	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	result, errMsg := callBridge(t, bridge.SockPath(), `{"tool":"http_request","params":{"url":"https://example.com"},"include_headers":false}`)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if result != "not found" {
		t.Errorf("result = %q, want %q", result, "not found")
	}
}

func TestExecBridgeShellFuncIncludeHeadersFlag(t *testing.T) {
	// Verify the generated shell function contains --include-headers handling
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "http_request",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"},"method":{"type":"string"},"headers":{"type":"object"},"body":{"type":"string"},"save_to":{"type":"string"}}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return TextResult(""), nil
		},
	})

	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	data, err := os.ReadFile(bridge.FuncsPath())
	if err != nil {
		t.Fatalf("read funcs file: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "--include-headers") {
		t.Error("http_request shell function should support --include-headers flag")
	}
	if !strings.Contains(content, "inc_headers") {
		t.Error("http_request shell function should have inc_headers variable")
	}
	if !strings.Contains(content, `"include_headers"`) {
		t.Error("http_request shell function should pass include_headers in request JSON")
	}
}

func TestExecBridgeTmuxShellFunc(t *testing.T) {
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "tmux",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"operation":{"type":"string"},"name":{"type":"string"},"command":{"type":"string"},"workdir":{"type":"string"},"watch":{"type":"boolean"},"keys":{"type":"string"},"enter":{"type":"boolean"},"lines":{"type":"integer"},"window":{"type":"integer"},"threshold_seconds":{"type":"integer"},"raw":{"type":"boolean"}}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return TextResult("ok"), nil
		},
	})

	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	data, err := os.ReadFile(bridge.FuncsPath())
	if err != nil {
		t.Fatalf("read funcs file: %v", err)
	}
	content := string(data)

	// Should contain function definition
	if !strings.Contains(content, "foci_tmux()") {
		t.Error("funcs file should contain foci_tmux()")
	}
	// Should contain export
	if !strings.Contains(content, "export -f foci_tmux") {
		t.Error("funcs file should export foci_tmux")
	}
	// Should contain subcommands
	for _, sub := range []string{"start)", "send)", "read)", "list)", "kill)", "watch)", "unwatch)"} {
		if !strings.Contains(content, sub) {
			t.Errorf("foci_tmux should handle subcommand %s", sub)
		}
	}
	// Should support stdin piping for send
	if !strings.Contains(content, "! -t 0") {
		t.Error("foci_tmux send should support stdin piping")
	}
}

func TestFinalizeExecDescription(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&Tool{
		Name:        "shell",
		Description: "Run a shell command.",
		Parameters:  json.RawMessage(`{}`),
	})
	reg.Register(&Tool{
		Name:        "web_search",
		ExecExport:  true,
		Description: "Search the web",
		Parameters:  json.RawMessage(`{}`),
	})
	reg.Register(&Tool{
		Name:        "summary",
		ExecExport:  true,
		Description: "Summarise content",
		Parameters:  json.RawMessage(`{}`),
	})
	reg.Register(&Tool{
		Name:        "read",
		ExecExport:  false,
		Description: "Read a file",
		Parameters:  json.RawMessage(`{}`),
	})

	reg.FinalizeExecDescription()

	shell := reg.Get("shell")
	if !strings.Contains(shell.Description, "foci_summary") {
		t.Errorf("shell description missing foci_summary: %s", shell.Description)
	}
	if !strings.Contains(shell.Description, "foci_web_search") {
		t.Errorf("shell description missing foci_web_search: %s", shell.Description)
	}
	if strings.Contains(shell.Description, "foci_read") {
		t.Errorf("shell description should not contain foci_read (ExecExport=false): %s", shell.Description)
	}

	// List should be alphabetically sorted
	sumIdx := strings.Index(shell.Description, "foci_summary")
	wsIdx := strings.Index(shell.Description, "foci_web_search")
	if sumIdx > wsIdx {
		t.Error("exported names should be in alphabetical order (foci_summary before foci_web_search)")
	}

	// Calling FinalizeExecDescription again should not duplicate the list
	reg.FinalizeExecDescription()
	count := strings.Count(shell.Description, "Shell functions are available")
	if count != 1 {
		t.Errorf("expected 1 occurrence of shell functions sentence, got %d", count)
	}
}

func TestExportedNamesAlphabetical(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&Tool{Name: "zebra", ExecExport: true, Parameters: json.RawMessage(`{}`)})
	reg.Register(&Tool{Name: "alpha", ExecExport: true, Parameters: json.RawMessage(`{}`)})
	reg.Register(&Tool{Name: "middle", ExecExport: true, Parameters: json.RawMessage(`{}`)})
	reg.Register(&Tool{Name: "nope", ExecExport: false, Parameters: json.RawMessage(`{}`)})

	names := reg.ExportedNames()
	if len(names) != 3 {
		t.Fatalf("expected 3 exported names, got %d", len(names))
	}
	expected := []string{"foci_alpha", "foci_middle", "foci_zebra"}
	for i, want := range expected {
		if names[i] != want {
			t.Errorf("names[%d] = %q, want %q", i, names[i], want)
		}
	}
}

// TestExecExportToolsHaveShellFunc verifies that every tool with ExecExport:true
// produces a non-empty shell function via generateShellFunc.
func TestExecExportToolsHaveShellFunc(t *testing.T) {
	// All tools that set ExecExport:true in production code.
	// When you add a new ExecExport tool, add it here.
	exportedTools := []string{
		"http_request",
		"memory_search",
		"send_telegram",
		"spawn",
		"summary",
		"tmux",
		"todo",
		"web_fetch",
		"web_search",
	}

	for _, name := range exportedTools {
		tool := &Tool{
			Name:       name,
			ExecExport: true,
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		}
		fn := generateShellFunc(tool)
		if fn == "" {
			t.Errorf("tool %q: generateShellFunc returned empty string", name)
			continue
		}
		funcName := "foci_" + name + "()"
		if !strings.Contains(fn, funcName) {
			t.Errorf("tool %q: shell function missing %q definition", name, funcName)
		}
	}
}

// TestAllToolsExportedOrSkipped ensures every known tool is either exported
// to the exec bridge (ExecExport:true) or explicitly listed in the skip
// list below with a reason. When a new tool is added, this test fails until
// the developer either exports it or adds it to the skip list.
func TestAllToolsExportedOrSkipped(t *testing.T) {
	// Tools that intentionally do NOT have ExecExport:true.
	// Each entry documents why the tool is skipped from the exec bridge.
	skippedTools := map[string]string{
		"shell":             "recursive — shell is the bridge host itself",
		"read":              "use cat/head/tail in shell",
		"write":             "use shell redirection (echo > file)",
		"edit":              "use sed/awk in shell",
		"send_to_session":   "agent-internal session routing, not useful in shell",
		"scratchpad":        "agent-internal working notes, not useful in shell",
		"bitwarden_search":  "secrets management — not exposed to subprocess",
		"bitwarden_unlock":  "secrets management — not exposed to subprocess",
		"remind":            "agent-internal reminder, not useful in shell",
	}

	// All known production tool names (from New*Tool constructors in tools/*.go
	// and main.go registration). Dynamic command wrapper tools are excluded
	// because they depend on user config.
	allTools := []string{
		"shell",
		"http_request",
		"memory_search",
		"read",
		"write",
		"edit",
		"summary",
		"send_telegram",
		"send_to_session",
		"spawn",
		"tmux",
		"todo",
		"web_fetch",
		"web_search",
		"scratchpad",
		"bitwarden_search",
		"bitwarden_unlock",
		"remind",
	}

	// ExecExport tools from the other test — keep in sync.
	exportedTools := map[string]bool{
		"http_request":  true,
		"memory_search": true,
		"send_telegram": true,
		"spawn":         true,
		"summary":       true,
		"tmux":          true,
		"todo":          true,
		"web_fetch":     true,
		"web_search":    true,
	}

	for _, name := range allTools {
		if exportedTools[name] {
			continue
		}
		if _, ok := skippedTools[name]; !ok {
			t.Errorf("tool %q is neither ExecExport:true nor in the skip list — "+
				"add it to exportedTools (and generateShellFunc) or skippedTools with a reason", name)
		}
	}

	// Verify no stale entries in skip list
	for name := range skippedTools {
		found := false
		for _, n := range allTools {
			if n == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("skip list contains %q but it's not in allTools — remove stale entry", name)
		}
		if exportedTools[name] {
			t.Errorf("tool %q is in both exportedTools and skippedTools — pick one", name)
		}
	}
}

// callBridge connects to a bridge socket and sends a request, returning the result and error.
func callBridge(t *testing.T, sockPath, request string) (result, errMsg string) {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "%s\n", request)

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !scanner.Scan() {
		t.Fatalf("read response: %v", scanner.Err())
	}

	var resp struct {
		Result string `json:"result"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	return resp.Result, resp.Error
}
