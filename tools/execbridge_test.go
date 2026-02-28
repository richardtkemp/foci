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
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct{ Text string `json:"text"` }
			json.Unmarshal(params, &p)
			return "echo: " + p.Text, nil
		},
	})
	r.Register(&Tool{
		Name:       "error_tool",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return "", fmt.Errorf("intentional error")
		},
	})
	r.Register(&Tool{
		Name:       "private_tool",
		ExecExport: false,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return "should not be callable", nil
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
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			capturedKey = SessionKeyFromContext(ctx)
			return "ok", nil
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
