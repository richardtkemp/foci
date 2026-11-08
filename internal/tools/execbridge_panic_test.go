package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func panicTool() *Tool {
	return &Tool{
		Name:       "panic_tool",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var s []string
			_ = s[1] // the #2961 shape: index out of range
			return TextResult("unreachable"), nil
		},
	}
}

// TestToolCall_RecoversPanic: Tool.Call turns a panic in Execute into an
// error, so no caller (exec bridge, API tool loop, spawn tool loop) can be
// killed by a tool bug (#2961).
func TestToolCall_RecoversPanic(t *testing.T) {
	t.Parallel()
	_, err := panicTool().Call(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected an error from a panicking tool")
	}
	if !strings.Contains(err.Error(), "panic_tool") || !strings.Contains(err.Error(), "index out of range") {
		t.Errorf("error should name the tool and the panic: %v", err)
	}
}

// TestExecBridge_ToolPanicIsAnErrorAndBridgeSurvives: a tool that panics
// behind the exec bridge used to panic the bridge's connection goroutine and
// exit foci-gw (#2961). The caller must get an error response, and the same
// bridge must keep serving the next call. On the unfixed code this test does
// not fail politely: the panic kills the test binary, as it killed the gateway.
func TestExecBridge_ToolPanicIsAnErrorAndBridgeSurvives(t *testing.T) {
	t.Parallel()
	r := testRegistry()
	r.Register(panicTool())
	bridge, err := NewExecBridge(r, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	result, errMsg := callBridge(t, bridge.SockPath(), `{"tool":"panic_tool","params":{}}`)
	if errMsg == "" {
		t.Fatalf("expected an error response, got result %q", result)
	}
	if !strings.Contains(errMsg, "panic") {
		t.Errorf("error = %q, want it to report the panic", errMsg)
	}

	result, errMsg = callBridge(t, bridge.SockPath(), `{"tool":"echo_tool","params":{"text":"still here"}}`)
	if errMsg != "" {
		t.Fatalf("bridge stopped serving after a tool panic: %s", errMsg)
	}
	if result != "echo: still here" {
		t.Errorf("result = %q, want %q", result, "echo: still here")
	}
}
