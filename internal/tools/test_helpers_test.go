package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// newTestExecTool creates an ExecTool with all-zero/nil defaults.
// Use this instead of NewExecTool(nil, nil, 0, nil, "", nil, 0, "", nil).
func newTestExecTool() *Tool {
	return NewExecTool(nil, nil, 0, nil, "", nil, 0, "", nil)
}

// execCommand marshals a command string into exec params and executes it.
func runExec(t *testing.T, tool *Tool, command string) (ToolResult, error) {
	t.Helper()
	params, _ := json.Marshal(map[string]interface{}{"command": command})
	return tool.Execute(context.Background(), params)
}

// requireError asserts err is non-nil and contains substr.
func requireError(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Errorf("error = %q, want substring %q", err.Error(), substr)
	}
}

// requireNoError fails the test if err is non-nil.
func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// requireContains asserts that s contains substr.
func requireContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected %q to contain %q", s, substr)
	}
}
