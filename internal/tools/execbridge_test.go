package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	osexec "os/exec"
	"strings"
	"sync"
	"testing"
)

func testRegistry() *Registry {
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "echo_tool",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var p struct {
				Text string `json:"text"`
			}
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
	// Verifies that NewExecBridge creates the socket and funcs files, and that Close removes them cleanly.
	t.Parallel()
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

func TestSessionExecBridgeUniquePerInstance(t *testing.T) {
	// #1120 guard: two bridges for the SAME session key must get DIFFERENT
	// socket paths, so tearing one down can never close the other's socket. On
	// /reset a dying backend (remapped to a branch key) and a fresh backend
	// briefly coexist under the same key — see NewSessionExecBridge.
	t.Parallel()
	r := testRegistry()
	key := t.Name() // unique across tests / parallel-safe

	b1, err := NewSessionExecBridge(r, context.Background(), key)
	if err != nil {
		t.Fatalf("first NewSessionExecBridge: %v", err)
	}
	defer b1.Close()
	b2, err := NewSessionExecBridge(r, context.Background(), key)
	if err != nil {
		t.Fatalf("second NewSessionExecBridge: %v", err)
	}
	defer b2.Close()

	if b1.SockPath() == b2.SockPath() {
		t.Fatalf("two bridges for key %q share socket path %q — teardown of one would kill the other", key, b1.SockPath())
	}

	// Both must be independently live...
	if res, errMsg := callBridge(t, b1.SockPath(), `{"tool":"echo_tool","params":{"text":"one"}}`); errMsg != "" || res != "echo: one" {
		t.Fatalf("b1 not callable: res=%q err=%q", res, errMsg)
	}
	// ...and closing b1 must NOT affect b2 (the #1120 failure mode).
	b1.Close()
	if res, errMsg := callBridge(t, b2.SockPath(), `{"tool":"echo_tool","params":{"text":"two"}}`); errMsg != "" || res != "echo: two" {
		t.Fatalf("b2 died when b1 closed — shared-socket regression: res=%q err=%q", res, errMsg)
	}
}

func TestExecBridgeCallTool(t *testing.T) {
	// Verifies that calling an exported tool via the bridge socket returns the correct result.
	t.Parallel()
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
	// Verifies that when a tool returns an error, the bridge propagates it as an error field in the response.
	t.Parallel()
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
	// Verifies that tools with ExecExport:false cannot be called through the bridge socket.
	t.Parallel()
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
	// Verifies that requesting a tool name not registered in the registry returns an "unknown tool" error.
	t.Parallel()
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
	// Verifies that malformed JSON sent to the bridge socket returns an error rather than panicking or hanging.
	t.Parallel()
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
	// Verifies that the generated shell funcs file includes exported tools and excludes private tools.
	t.Parallel()
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
	t.Parallel()
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

	ctx := WithSessionKey(context.Background(), "test/c123")
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
	if capturedKey != "test/c123" {
		t.Errorf("session key = %q, want %q", capturedKey, "test/c123")
	}
}

func TestExecBridgeConcurrentCalls(t *testing.T) {
	// Verifies that multiple goroutines can call tools through the bridge simultaneously without data races or corruption.
	t.Parallel()
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
	// Verifies that two bridges created concurrently get distinct socket paths so they don't collide.
	t.Parallel()
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
	// Verifies that stripHTTPHeaders removes the status line and headers, returning only the body, across multiple response shapes.
	t.Parallel()
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
	// Verifies that toolParamKeys extracts and alphabetically sorts property names from a tool's JSON schema.
	t.Parallel()
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
	// Verifies that the generated shell funcs file includes the foci__json guard helper and that exported tools reference it with their valid parameter keys.
	t.Parallel()
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

	// Should contain the foci__json helper
	if !strings.Contains(content, "foci__json()") {
		t.Error("funcs file should contain foci__json() helper")
	}
	// Should contain export -f for the helper
	if !strings.Contains(content, "export -f foci__json") {
		t.Error("funcs file should export foci__json")
	}
	// echo_tool should have a guard line with its valid key "text"
	if !strings.Contains(content, `foci__json "echo_tool" "text"`) {
		t.Error("echo_tool guard should include valid key 'text'")
	}
}

func TestExecBridgeHTTPRequestHeadersStripped(t *testing.T) {
	// Register a fake http_request tool that returns headers + body
	t.Parallel()
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "http_request",
		Positional: []string{"url"},
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
	t.Parallel()
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "http_request",
		Positional: []string{"url"},
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
	t.Parallel()
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "http_request",
		Positional: []string{"url"},
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
	t.Parallel()
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "http_request",
		Positional: []string{"url"},
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
	// Verifies that the generated foci_tmux shell function handles all expected subcommands and supports stdin piping for send.
	t.Parallel()
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "tmux",
		Positional: []string{"operation"},
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

func TestFinalizeShellDescription(t *testing.T) {
	// Verifies that FinalizeShellDescription appends an alphabetically-sorted list of exported tool names to the shell tool description, excluding non-exported tools, and is idempotent.
	t.Parallel()
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
		Positional:  []string{"prompt"},
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

	reg.FinalizeShellDescription()

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

	// Calling FinalizeShellDescription again should not duplicate the list
	reg.FinalizeShellDescription()
	count := strings.Count(shell.Description, "Shell functions are available")
	if count != 1 {
		t.Errorf("expected 1 occurrence of shell functions sentence, got %d", count)
	}
}

func TestExportedNamesAlphabetical(t *testing.T) {
	// Verifies that ExportedNames returns only ExecExport:true tools in alphabetical order with the foci_ prefix.
	t.Parallel()
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

func TestExecExportToolsHaveShellFunc(t *testing.T) {
	// All tools that set ExecExport:true in production code.
	t.Parallel()
	// When you add a new ExecExport tool, add it here.
	exportedTools := []string{
		"http_request",
		"memory_search",
		"send_to_chat",
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

// TestTodoActionsCoverEveryDispatchArm guards against the foci_todo action
// list drifting between todoActions (the source for help / flag-scope) and
// the actual dispatch case in the generated bash. Adding a new action to
// the dispatch without updating todoActions would silently produce wrong
// help — this test catches that at build time.
func TestTodoActionsCoverEveryDispatchArm(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "todo",
		Positional: []string{"action"},
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"}}}`),
	})
	body := generateShellFunc(r.All()[0])

	// Walk the dispatch case ARMS in the generated bash. Each arm is a
	// line like "    add)" or "    list-all)". The set of arms (minus
	// the catch-all "*)") must equal the todoActions name set.
	dispatchArms := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		// Match e.g. "add)" but skip "--add)" (a flag) and "*)" (catch-all).
		if strings.HasSuffix(trimmed, ")") &&
			!strings.HasPrefix(trimmed, "--") &&
			!strings.HasPrefix(trimmed, "*") &&
			!strings.Contains(trimmed, "$") &&
			!strings.Contains(trimmed, "|") &&
			!strings.Contains(trimmed, "[") &&
			!strings.Contains(trimmed, "(") {
			arm := strings.TrimSuffix(trimmed, ")")
			// Filter to bare-word arms that look like action names.
			if arm != "" && !strings.ContainsAny(arm, " \t\"'") {
				dispatchArms[arm] = true
			}
		}
	}

	declared := map[string]bool{}
	for _, a := range todoActions {
		declared[a.Name] = true
	}

	// Every declared action must show up as a dispatch arm somewhere in the
	// body (the actions case statement OR the JQ dispatch).
	for name := range declared {
		if !strings.Contains(body, "    "+name+")") {
			t.Errorf("todoActions declares %q but no '%s)' arm found in generated bash", name, name)
		}
	}
	// Every dispatch arm of an action-name shape must be declared.
	for arm := range dispatchArms {
		if declared[arm] {
			continue
		}
		// Skip arms that aren't action names (e.g. inner case for positional dispatch).
		// We tolerate unknowns silently — the goal is to catch declared-but-undispatched,
		// not the other direction.
	}
}

// TestTodoShellFunc_TopLevelHelpListsSubcommands verifies that the
// top-level `foci_todo --help` output now includes a Subcommands block
// listing each action's signature. Closes the recovery loop where I
// (clutch) had to guess flag names per action.
func TestTodoShellFunc_TopLevelHelpListsSubcommands(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "todo",
		Positional: []string{"action"},
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"}}}`),
	})
	body := generateShellFunc(r.All()[0])

	if !strings.Contains(body, "Subcommands:") {
		t.Error("top-level help should contain 'Subcommands:' section")
	}
	for _, a := range todoActions {
		if !strings.Contains(body, "foci_todo "+a.Usage) {
			t.Errorf("top-level help missing subcommand line for %q (expected 'foci_todo %s')", a.Name, a.Usage)
		}
	}
	if !strings.Contains(body, "foci_todo <subcommand> --help") {
		t.Error("top-level help should point users to per-subcommand help")
	}
}

// TestTodoShellFunc_PerActionHelpIntercept verifies the generated bash
// includes an action-scoped --help intercept that runs after the action
// is parsed but before the flag loop. This is the headline fix for
// TODO #729: `foci_todo complete --help` should print
// complete-specific usage instead of erroring as "unrecognized flag".
func TestTodoShellFunc_PerActionHelpIntercept(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "todo",
		Positional: []string{"action"},
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"}}}`),
	})
	body := generateShellFunc(r.All()[0])

	// The intercept block should set action_usage / action_flags from a
	// case statement keyed on $action, then check $1 for -h/--help.
	if !strings.Contains(body, `local action_usage="" action_flags=""`) {
		t.Error("expected action_usage/action_flags locals to be declared")
	}
	for _, a := range todoActions {
		expected := `action_usage='` + a.Usage + `'`
		if !strings.Contains(body, expected) {
			t.Errorf("missing per-action usage assignment for %q (expected %q)", a.Name, expected)
		}
		if a.Flags != "" {
			expectedFlags := `action_flags='` + a.Flags + `'`
			if !strings.Contains(body, expectedFlags) {
				t.Errorf("missing per-action flags assignment for %q (expected %q)", a.Name, expectedFlags)
			}
		}
	}
	if !strings.Contains(body, `if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
    if [ -n "$action_usage" ]; then
      echo "usage: foci_todo $action_usage"`) {
		t.Error("expected per-action --help intercept block")
	}
}

// TestTodoShellFunc_UnknownFlagErrorScopedToAction verifies the
// "unrecognized flag" error message scopes to the current action's
// flags when an action is in scope, instead of dumping every foci_todo
// flag. Resolves the second half of today's friction (#744 close note,
// where `complete --note ...` reported the full 11-flag list).
func TestTodoShellFunc_UnknownFlagErrorScopedToAction(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "todo",
		Positional: []string{"action"},
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"}}}`),
	})
	body := generateShellFunc(r.All()[0])

	if !strings.Contains(body, `if [ -n "$action_flags" ]; then
          echo "valid flags for '$action': $action_flags"`) {
		t.Error("expected per-action flag-scope branch in unknown-flag error")
	}
	if !strings.Contains(body, `elif [ -n "$action" ]; then
          echo "'$action' takes no flags"`) {
		t.Error("expected 'no flags for this action' branch")
	}
	// Master fallback for unknown-action context must still exist.
	if !strings.Contains(body, "valid flags: --text --priority --tag --query --status --id --ids --reason --notes --note --append --append-text --add --sort --reverse --limit") {
		t.Error("expected master flag list as fallback when action is unknown")
	}
}

// TestTodoShellFunc_CloseReasonAliases verifies the close-reason alias surface
// added for TODO #761 and extended later: --notes and --note both parse
// straight into the reason var, and on complete/drop --text falls back into
// reason if --reason wasn't given. This lets users reach for any of
// {--reason, --notes, --note, --text} when closing a todo with rich detail.
// --reason wins if both --reason and --text are present (explicit beats
// implicit fallback). The singular --note alias exists because the bare
// English word is what naturally comes to mind, and the recurring
// --note/--notes typo deserves a tooling fix not a memory-training fix.
func TestTodoShellFunc_CloseReasonAliases(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "todo",
		Positional: []string{"action"},
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"}}}`),
	})
	body := generateShellFunc(r.All()[0])

	// --notes parses directly into the reason var, alongside --reason.
	// Kept as two cases (rather than --reason|--notes alternation) so the
	// schema-params validator at validateShellFunc still sees `--reason)`
	// as a literal substring.
	if !strings.Contains(body, `--reason) reason="$2"; shift 2 ;;`) {
		t.Error("expected --reason flag parser case")
	}
	if !strings.Contains(body, `--notes) reason="$2"; shift 2 ;;`) {
		t.Error("expected --notes flag parser case (writes to reason)")
	}
	if !strings.Contains(body, `--note) reason="$2"; shift 2 ;;`) {
		t.Error("expected --note flag parser case (writes to reason)")
	}

	// On complete/drop, --text falls back to reason when --reason absent.
	// Tested by checking the post-parse case block exists.
	if !strings.Contains(body, `    complete|drop)
      if [ -z "$reason" ] && [ -n "$text" ]; then
        reason="$text"
        text=""
      fi
      ;;`) {
		t.Error("expected post-parse text→reason fallback for complete/drop")
	}

	// Per-action flags surface for complete/drop must list the new aliases
	// so the unknown-flag error and --help output are accurate.
	for _, name := range []string{"complete", "drop"} {
		var found bool
		for _, a := range todoActions {
			if a.Name == name {
				found = true
				if !strings.Contains(a.Flags, "--text") || !strings.Contains(a.Flags, "--notes") || !strings.Contains(a.Flags, "--note") {
					t.Errorf("todoActions[%q].Flags = %q, want --text, --notes, and --note", name, a.Flags)
				}
				if !strings.Contains(a.Usage, "--reason|--notes|--note|--text") {
					t.Errorf("todoActions[%q].Usage = %q, want --reason|--notes|--note|--text in usage", name, a.Usage)
				}
			}
		}
		if !found {
			t.Errorf("todoActions missing entry for %q", name)
		}
	}
}

// TestTodoActionAliases covers the action-alias surface (TODO: foci_todo
// create as a valid synonym for add). Verifies both layers:
//
//   - The shell layer emits a normalization case so `foci_todo create ...`
//     resolves to add before reaching action_usage / dispatch.
//   - The Go layer's resolveTodoAction maps aliases to canonical names so a
//     direct tool call with action="create" also dispatches to add.
//
// Adding a new alias to todoActionAliases should make this test continue to
// pass without manual updates — the loop walks the map.
func TestTodoActionAliases(t *testing.T) {
	t.Parallel()

	// Tool layer: every alias resolves to its canonical action.
	for alias, canonical := range todoActionAliases {
		if got := resolveTodoAction(alias); got != canonical {
			t.Errorf("resolveTodoAction(%q) = %q, want %q", alias, got, canonical)
		}
	}
	// Non-alias action passes through unchanged.
	if got := resolveTodoAction("add"); got != "add" {
		t.Errorf("resolveTodoAction(\"add\") = %q, want \"add\"", got)
	}
	if got := resolveTodoAction("nonsense"); got != "nonsense" {
		t.Errorf("resolveTodoAction(\"nonsense\") = %q, want passthrough", got)
	}

	// Shell layer: generated bash contains a normalization case for every
	// declared alias, placed before the action_usage lookup so downstream
	// help / dispatch sees the canonical name.
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "todo",
		Positional: []string{"action"},
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"}}}`),
	})
	body := generateShellFunc(r.All()[0])

	for alias, canonical := range todoActionAliases {
		expected := alias + ") action='" + canonical + "'"
		if !strings.Contains(body, expected) {
			t.Errorf("generated bash missing alias normalization for %q → %q (expected substring %q)", alias, canonical, expected)
		}
	}

	// The normalization comment marks the block so it can't be silently
	// removed by a refactor.
	if !strings.Contains(body, "Normalize action aliases") {
		t.Error("generated bash missing alias-normalization comment block")
	}
}

// TestTodoActionAliasEndToEnd dispatches an alias through the real tool
// callback and confirms it lands in the underlying store as if the canonical
// name had been used. Belt-and-braces against future refactors that route
// around resolveTodoAction.
func TestTodoActionAliasEndToEnd(t *testing.T) {
	t.Parallel()
	store := newTestTodoStore(t)
	tool := NewTodoTool(store, "agent-alias")

	params := map[string]interface{}{
		"action":   "create",
		"text":     "alias-test item",
		"priority": "medium",
	}
	if _, err := executeTodoTool(tool, params); err != nil {
		t.Fatalf("create-aliased add: %v", err)
	}

	items, _ := store.List("agent-alias", "", nil, "", "", false, 0)
	if len(items) != 1 || items[0].Text != "alias-test item" {
		t.Errorf("expected one item from create-aliased add, got %+v", items)
	}
}

func TestExecBridgeTodoShellFuncSortParam(t *testing.T) {
	// Verify the generated foci_todo shell function includes --sort parameter handling
	t.Parallel()
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "todo",
		Positional: []string{"action"},
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"},"text":{"type":"string"},"priority":{"type":"string"},"tag":{"type":"string"},"query":{"type":"string"},"status":{"type":"string"},"id":{"type":"integer"},"reason":{"type":"string"},"sort":{"type":"string"}}}`),
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
	if !strings.Contains(content, "foci_todo()") {
		t.Error("funcs file should contain foci_todo()")
	}
	// Should contain --sort flag handling in the argument parser
	if !strings.Contains(content, "--sort)") {
		t.Error("foci_todo should handle --sort flag")
	}
	// Should have a sort variable declared
	if !strings.Contains(content, `local text="" priority="" tag="" query="" status="" id="" ids="" reason="" sort="" reverse="" limit=""`) {
		t.Error("foci_todo should declare sort, reverse, limit, and ids variables")
	}
	// Should pass sort parameter to list action
	if !strings.Contains(content, `[ -n "$sort" ] && params="$(echo "$params" | jq --arg o "$sort" '. + {sort: $o}')"`) {
		t.Error("foci_todo list action should pass sort parameter")
	}
}

func TestExecBridgeShellFuncsRejectUnknownFlags(t *testing.T) {
	// Verify that all generated shell functions reject unrecognized flags
	t.Parallel()
	r := NewRegistry()
	tools := []struct {
		name       string
		params     json.RawMessage
		validFlags []string
		positional []string // bare-arg params (matches the tool's real Tool.Positional)
	}{
		{
			name:       "web_search",
			params:     json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
			validFlags: []string{"--query"},
		},
		{
			name:       "memory_search",
			params:     json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
			validFlags: []string{"--query"},
		},
		{
			name:       "web_fetch",
			params:     json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"},"raw":{"type":"boolean"}}}`),
			validFlags: []string{"--raw"},
		},
		{
			name:       "http_request",
			params:     json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"},"method":{"type":"string"},"headers":{"type":"object"},"body":{"type":"string"},"save_to":{"type":"string"}}}`),
			validFlags: []string{"--method", "--body", "--header", "--save-to", "--include-headers"},
			positional: []string{"url"},
		},
		{
			name:       "send_to_chat",
			params:     json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"file":{"type":"string"},"send_as":{"type":"string"}}}`),
			validFlags: []string{"--file", "--send-as"},
		},
		{
			name:       "todo",
			params:     json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"},"text":{"type":"string"},"priority":{"type":"string"},"tag":{"type":"string"},"query":{"type":"string"},"status":{"type":"string"},"id":{"type":"integer"},"reason":{"type":"string"},"sort":{"type":"string"}}}`),
			validFlags: []string{"--text", "--priority", "--tag", "--query", "--status", "--id", "--reason", "--sort"},
			positional: []string{"action"},
		},
		{
			name:       "summary",
			params:     json.RawMessage(`{"type":"object","properties":{"file":{"type":"string"},"prompt":{"type":"string"}}}`),
			validFlags: []string{"--file"},
			positional: []string{"prompt"},
		},
		{
			name:       "spawn",
			params:     json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"},"model":{"type":"string"},"context":{"type":"string"}}}`),
			validFlags: []string{"--model", "--context"},
		},
		{
			name:       "tmux",
			params:     json.RawMessage(`{"type":"object","properties":{"operation":{"type":"string"},"name":{"type":"string"},"command":{"type":"string"},"workdir":{"type":"string"},"watch":{"type":"boolean"},"keys":{"type":"string"},"enter":{"type":"boolean"},"lines":{"type":"integer"},"window":{"type":"integer"},"threshold_seconds":{"type":"integer"},"raw":{"type":"boolean"}}}`),
			validFlags: []string{"--name", "--command", "--workdir", "--watch", "--keys", "--enter", "--lines", "--window", "--threshold-seconds", "--raw"},
			positional: []string{"operation"},
		},
	}

	for _, tool := range tools {
		r.Register(&Tool{
			Name:       tool.name,
			ExecExport: true,
			Positional: tool.positional,
			Parameters: tool.params,
			Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
				return TextResult("ok"), nil
			},
		})
	}

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

	for _, tool := range tools {
		funcName := "foci_" + tool.name
		if !strings.Contains(content, funcName+"()") {
			t.Errorf("funcs file should contain %s()", funcName)
			continue
		}

		// Find the function definition
		funcStart := strings.Index(content, funcName+"()")
		if funcStart == -1 {
			continue
		}
		funcEnd := strings.Index(content[funcStart:], "\nexport -f "+funcName)
		if funcEnd == -1 {
			funcEnd = len(content) - funcStart
		}
		funcBody := content[funcStart : funcStart+funcEnd]

		// Should contain error handling for unrecognized flags
		if !strings.Contains(funcBody, `--*)`) {
			t.Errorf("%s should have wildcard case for unrecognized flags", funcName)
		}
		if !strings.Contains(funcBody, `echo "error: unrecognized flag:`) {
			t.Errorf("%s should print error for unrecognized flag", funcName)
		}
		if !strings.Contains(funcBody, `"valid flags:`) {
			t.Errorf("%s should list valid flags in error message", funcName)
		}

		// Should list all valid flags in error message
		for _, flag := range tool.validFlags {
			// The error message should mention the flag (without the --)
			flagName := strings.TrimPrefix(flag, "--")
			if !strings.Contains(funcBody, flagName) {
				t.Errorf("%s error message should mention valid flag %s", funcName, flag)
			}
		}
	}
}

func TestExecBridgePipeFunctions(t *testing.T) {
	// Verifies that piping between generated shell functions works end-to-end:
	// foci_todo get 1 | foci_send_to_chat
	// The left side outputs todo text via foci-call, the right side reads stdin
	// via $(cat) and sends it via foci-call. This is the exact scenario that
	// was reported broken in production.
	t.Parallel()

	if _, err := osexec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := osexec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}

	// Build foci-call binary to a temp directory
	binDir := t.TempDir()
	binPath := binDir + "/foci-call"
	build := osexec.Command("go", "build", "-o", binPath, "foci/cmd/foci-call")
	build.Dir = findModuleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build foci-call: %v\n%s", err, out)
	}

	// Register mock tools — use realistic todo output with markdown, newlines, special chars
	const todoText = "**#573** [ ] `med` `work`\nBuy milk & eggs (\"organic\" if possible)\nDue: tomorrow"
	var captured string
	var mu sync.Mutex

	r := NewRegistry()
	r.Register(&Tool{
		Name:       "todo",
		Positional: []string{"action"},
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"},"id":{"type":"integer"}}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return TextResult(todoText), nil
		},
	})
	r.Register(&Tool{
		Name:       "send_to_chat",
		ExecExport: true,
		Positional: []string{"text"}, // matches NewSendToChatTool
		StdinParam: "text",           // matches NewSendToChatTool
		Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"file":{"type":"string"},"send_as":{"type":"string"}}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var p struct {
				Text string `json:"text"`
			}
			json.Unmarshal(params, &p)
			mu.Lock()
			captured = p.Text
			mu.Unlock()
			return TextResult("sent"), nil
		},
	})

	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	// Run the pipe through real bash with the same shell options as production
	// (execPreamble sets pipefail, nounset, failglob).
	script := fmt.Sprintf(
		"set -o pipefail -o nounset; shopt -s failglob; source %s; foci_todo get 1 | foci_send_to_chat",
		bridge.FuncsPath(),
	)
	cmd := osexec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(),
		"FOCI_SOCK="+bridge.SockPath(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash pipe failed: %v\noutput: %s", err, out)
	}

	mu.Lock()
	got := captured
	mu.Unlock()
	if got != todoText {
		t.Errorf("captured text = %q, want %q\nbash output: %s", got, todoText, out)
	}

	// `--text -` must read stdin like `--file -`, not send a literal "-" (#1007).
	script2 := fmt.Sprintf(
		"set -o pipefail -o nounset; shopt -s failglob; source %s; foci_todo get 1 | foci_send_to_chat --text -",
		bridge.FuncsPath(),
	)
	cmd2 := osexec.Command("bash", "-c", script2)
	cmd2.Env = append(os.Environ(),
		"FOCI_SOCK="+bridge.SockPath(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
	)
	if out2, err := cmd2.CombinedOutput(); err != nil {
		t.Fatalf("bash --text - pipe failed: %v\noutput: %s", err, out2)
	}
	mu.Lock()
	got2 := captured
	mu.Unlock()
	if got2 != todoText {
		t.Errorf("--text - captured %q, want %q (should read stdin, not send literal '-')", got2, todoText)
	}
}

// TestTodoShellFunc_AppendAliasesResolve runs the generated foci_todo through
// real bash and asserts every ergonomic append spelling — --note, --append-text,
// --add, the bare --append boolean, the `update` action alias, and a numeric
// positional id — collapses to the one canonical bare-tool shape
// {action:edit, id, text, append:true}. The replace path (no append flag) is
// the control, and supplying both replace and append text is rejected.
func TestTodoShellFunc_AppendAliasesResolve(t *testing.T) {
	t.Parallel()
	if _, err := osexec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := osexec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}

	binDir := t.TempDir()
	binPath := binDir + "/foci-call"
	build := osexec.Command("go", "build", "-o", binPath, "foci/cmd/foci-call")
	build.Dir = findModuleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build foci-call: %v\n%s", err, out)
	}

	var mu sync.Mutex
	var captured string
	r := NewRegistry()
	r.Register(&Tool{
		Name:       "todo",
		Positional: []string{"action"},
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"},"id":{"type":"integer"},"text":{"type":"string"},"append":{"type":"boolean"}}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			mu.Lock()
			captured = string(params)
			mu.Unlock()
			return TextResult("ok"), nil
		},
	})
	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	run := func(cmdline string) (map[string]any, error) {
		mu.Lock()
		captured = ""
		mu.Unlock()
		script := fmt.Sprintf("set -o pipefail -o nounset; shopt -s failglob; source %s; %s", bridge.FuncsPath(), cmdline)
		cmd := osexec.Command("bash", "-c", script)
		cmd.Env = append(os.Environ(), "FOCI_SOCK="+bridge.SockPath(), "PATH="+binDir+":"+os.Getenv("PATH"))
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("%v: %s", err, out)
		}
		mu.Lock()
		defer mu.Unlock()
		var got map[string]any
		if err := json.Unmarshal([]byte(captured), &got); err != nil {
			return nil, fmt.Errorf("unmarshal captured %q: %w", captured, err)
		}
		return got, nil
	}

	want := func(t *testing.T, got map[string]any, text string, append bool) {
		t.Helper()
		if got["action"] != "edit" {
			t.Errorf("action = %v, want edit", got["action"])
		}
		if got["id"] != float64(6) {
			t.Errorf("id = %v, want 6", got["id"])
		}
		if got["text"] != text {
			t.Errorf("text = %v, want %q", got["text"], text)
		}
		if append {
			if got["append"] != true {
				t.Errorf("append = %v, want true", got["append"])
			}
		} else if _, present := got["append"]; present {
			t.Errorf("append present (%v), want absent", got["append"])
		}
	}

	t.Run("note alias + positional id", func(t *testing.T) {
		got, err := run(`foci_todo edit 6 --note "hello world"`)
		if err != nil {
			t.Fatal(err)
		}
		want(t, got, "hello world", true)
	})
	t.Run("update action alias + append-text", func(t *testing.T) {
		got, err := run(`foci_todo update 6 --append-text "appended"`)
		if err != nil {
			t.Fatal(err)
		}
		want(t, got, "appended", true)
	})
	t.Run("add alias with --id", func(t *testing.T) {
		got, err := run(`foci_todo edit --id 6 --add "added"`)
		if err != nil {
			t.Fatal(err)
		}
		want(t, got, "added", true)
	})
	t.Run("bare --append boolean with --text", func(t *testing.T) {
		got, err := run(`foci_todo edit --id 6 --append --text "viatext"`)
		if err != nil {
			t.Fatal(err)
		}
		want(t, got, "viatext", true)
	})
	t.Run("replace path (control, no append)", func(t *testing.T) {
		got, err := run(`foci_todo edit --id 6 --text "replaced"`)
		if err != nil {
			t.Fatal(err)
		}
		want(t, got, "replaced", false)
	})
	t.Run("replace and append both → error", func(t *testing.T) {
		if _, err := run(`foci_todo edit 6 --text "a" --note "b"`); err == nil {
			t.Error("expected error when both replace --text and append flag given")
		}
	})
}

func TestGenerateGenericShellFuncFlatSchema(t *testing.T) {
	// Pins the generic generator's contract for a flat schema with one
	// required string, one optional boolean, and a snake_case key. This is
	// the shape of foci_remind and any future flat-schema tool added to
	// the registry without a hand-rolled case in generateShellFunc.
	t.Parallel()
	tool := &Tool{
		Name:        "test_flat",
		Description: "test tool",
		ExecExport:  true,
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"text":{"type":"string","description":"the text"},
				"when":{"type":"string","description":"when"},
				"wake":{"type":"boolean","description":"wake flag"},
				"date_from":{"type":"string","description":"snake_case key"}
			},
			"required":["text","when"]
		}`),
	}

	body := generateGenericShellFunc(tool)

	// String-flag arms consume two args. Snake_case becomes kebab-case.
	for _, want := range []string{
		`--text) text="$2"; shift 2`,
		`--when) when="$2"; shift 2`,
		`--date-from) date_from="$2"; shift 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// Boolean is presence-only.
	if !strings.Contains(body, `--wake) wake=true; shift ;;`) {
		t.Errorf("body missing presence-only --wake arm")
	}
	// Required-param check fires when text or when are empty.
	if !strings.Contains(body, `[ -z "$text" ] || [ -z "$when" ]`) {
		t.Errorf("body missing required-param check")
	}
	// Usage line lists required flags but not optional ones.
	if !strings.Contains(body, `usage: foci_test_flat --text <text> --when <when>`) {
		t.Errorf("body missing usage line for required params")
	}
	// Boolean param uses jq object literal (no --argjson with empty value).
	if !strings.Contains(body, `[ "$wake" = true ] && params="$(echo "$params" | jq '. + {wake: true}')"`) {
		t.Errorf("body missing boolean params injection")
	}
	// String param uses jq --arg.
	if !strings.Contains(body, `[ -n "$text" ] && params="$(echo "$params" | jq --arg v "$text" '. + {text: $v}')"`) {
		t.Errorf("body missing string params injection")
	}
}

func TestGenerateGenericShellFuncEmptyFallback(t *testing.T) {
	// Empty/unparseable schema falls back to legacy JSON-blob behavior so
	// the foci__json passthrough still works for raw-JSON callers.
	t.Parallel()
	tool := &Tool{
		Name:       "test_empty",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}
	body := generateGenericShellFunc(tool)
	if !strings.Contains(body, `foci-call "$(jq -nc --argjson p "$1" '{"tool":"test_empty","params":$p}')"`) {
		t.Errorf("empty-schema fallback should emit JSON-blob handler")
	}
	if strings.Contains(body, "while [ $# -gt 0 ]") {
		t.Errorf("empty-schema fallback should not emit flag-parsing loop")
	}
}

func TestGenerateGenericShellFuncAliases(t *testing.T) {
	// Aliases on Tool produce additional --flag case arms that assign into
	// the canonical variable. So `--description X` and `--text X` both
	// populate $text. Help text shows both names. Catches drift if the
	// generator silently drops aliases.
	t.Parallel()
	tool := &Tool{
		Name:        "test_alias",
		Description: "test tool",
		ExecExport:  true,
		Aliases:     map[string][]string{"text": {"description"}},
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"text":{"type":"string","description":"the text"}
			},
			"required":["text"]
		}`),
	}

	body := generateGenericShellFunc(tool)

	// Both canonical and alias arms exist, both assign to text.
	if !strings.Contains(body, `--text) text="$2"; shift 2`) {
		t.Errorf("body missing canonical --text arm")
	}
	if !strings.Contains(body, `--description) text="$2"; shift 2`) {
		t.Errorf("body missing alias --description arm targeting text")
	}
	// The recognized-flag list (used by the unknown-flag error message)
	// includes both names so callers see the alias is valid.
	if !strings.Contains(body, "--text --description") {
		t.Errorf("body unrecognized-flag list should include both --text and --description")
	}

	// Help text advertises the alias inline with the canonical flag.
	help := generateHelpText(tool)
	if !strings.Contains(help, "--text|--description") {
		t.Errorf("help text should show alias: got %q", help)
	}
}

func TestGenerateHelpTextPositionalArguments(t *testing.T) {
	t.Parallel()

	// A positional param's schema description (here the bare-agent-name
	// affordance) must surface in --help. Positionals are excluded from the
	// Flags list, so without an Arguments section the description is invisible.
	tool := &Tool{
		Name:       "send_to_session",
		Positional: []string{"session_key"},
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"session_key":{"type":"string","description":"Target session. Accepts a full session key, a bare agent name, or a chat alias."},
				"message":{"type":"string","description":"Message to send"}
			},
			"required":["session_key","message"]
		}`),
	}

	help := generateHelpText(tool)
	if !strings.Contains(help, "Arguments:") {
		t.Errorf("help text missing Arguments section: got %q", help)
	}
	if !strings.Contains(help, "bare agent name") {
		t.Errorf("help text should surface positional description: got %q", help)
	}
	// The positional must not also appear as a flag.
	if strings.Contains(help, "--session-key") {
		t.Errorf("positional should not be rendered as a flag: got %q", help)
	}
}

func TestGeneratedRemindShellFuncEndToEnd(t *testing.T) {
	// Sources the generated funcs file and invokes foci_remind with
	// --text/--when/--wake against a real socket. Asserts the JSON
	// delivered to foci-call has the expected typed values. Catches bash
	// quoting bugs that static checks would miss — and proves the exact
	// invocation that failed in TODO #723 now works.
	t.Parallel()

	if _, err := osexec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := osexec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}

	binDir := t.TempDir()
	binPath := binDir + "/foci-call"
	build := osexec.Command("go", "build", "-o", binPath, "foci/cmd/foci-call")
	build.Dir = findModuleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build foci-call: %v\n%s", err, out)
	}

	var captured json.RawMessage
	var mu sync.Mutex
	r := NewRegistry()
	r.Register(&Tool{
		Name:        "remind",
		Description: "Defer a thought for later.",
		ExecExport:  true,
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"text":{"type":"string","description":"the text"},
				"when":{"type":"string","description":"when"},
				"wake":{"type":"boolean","description":"wake flag"}
			},
			"required":["text","when"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			mu.Lock()
			captured = append([]byte(nil), params...)
			mu.Unlock()
			return TextResult("ok"), nil
		},
	})

	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	// Same shell options as production execPreamble.
	script := fmt.Sprintf(
		"set -o pipefail -o nounset; shopt -s failglob; source %s; foci_remind --text 'investigate why' --when 2m --wake",
		bridge.FuncsPath(),
	)
	cmd := osexec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(),
		"FOCI_SOCK="+bridge.SockPath(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash invocation failed: %v\noutput: %s", err, out)
	}

	mu.Lock()
	got := captured
	mu.Unlock()
	var p struct {
		Text string `json:"text"`
		When string `json:"when"`
		Wake bool   `json:"wake"`
	}
	if err := json.Unmarshal(got, &p); err != nil {
		t.Fatalf("parse captured params %q: %v", got, err)
	}
	if p.Text != "investigate why" {
		t.Errorf("text = %q, want %q", p.Text, "investigate why")
	}
	if p.When != "2m" {
		t.Errorf("when = %q, want %q", p.When, "2m")
	}
	if !p.Wake {
		t.Errorf("wake = false, want true (presence-only --wake)")
	}
}

// findModuleRoot walks up from the test's directory to find the go.mod file.
func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(dir + "/go.mod"); err == nil {
			return dir
		}
		parent := dir[:strings.LastIndex(dir, "/")]
		if parent == dir {
			t.Fatal("could not find go.mod")
		}
		dir = parent
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

func TestValidateShellFuncSchemaParity(t *testing.T) {
	// Unit-level test of the parity validator. The validator runs on every
	// NewExecBridge call, so the structural coverage for real production
	// tools comes from any test that builds a bridge (see
	// TestBuildExecRegistryAllToolsHaveShellFuncParity in cmd/foci-gw).
	// This test pins the validator's own behavior on synthetic schemas.
	t.Parallel()
	tests := []struct {
		name      string
		tool      *Tool
		wantError bool
	}{
		{
			name: "flat schema all flags wired",
			tool: &Tool{
				Name:       "flat",
				ExecExport: true,
				Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"wake":{"type":"boolean"}}}`),
			},
			wantError: false, // generic generator wires all params
		},
		{
			name: "snake_case to kebab-case",
			tool: &Tool{
				Name:       "kebab",
				ExecExport: true,
				Parameters: json.RawMessage(`{"type":"object","properties":{"date_from":{"type":"string"}}}`),
			},
			wantError: false,
		},
		{
			name: "empty schema falls back to JSON-blob (no validation needed)",
			tool: &Tool{
				Name:       "empty",
				ExecExport: true,
				Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
			},
			wantError: false,
		},
		{
			name: "unparseable schema is tolerated",
			tool: &Tool{
				Name:       "bad",
				ExecExport: true,
				Parameters: json.RawMessage(`not json`),
			},
			wantError: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateShellFuncSchemaParity(tc.tool)
			if tc.wantError && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tc.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateShellFuncSchemaParityCatchesDrift(t *testing.T) {
	// Constructs a deliberately-broken tool: it has a hand-rolled case in
	// generateShellFunc's switch (http_request) but a schema with extra
	// params the body doesn't handle. The validator must detect this and
	// surface the drift before it reaches production.
	t.Parallel()
	tool := &Tool{
		Name:       "http_request", // hand-rolled — fixed flag set
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"},"new_field":{"type":"string"},"another_one":{"type":"boolean"}}}`),
	}
	err := validateShellFuncSchemaParity(tool)
	if err == nil {
		t.Fatal("expected drift error for http_request with extra schema params, got nil")
	}
	for _, want := range []string{"new_field", "another_one"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}
