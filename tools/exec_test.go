package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestExecEcho(t *testing.T) {
	tool := NewExecTool(nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "echo hello world",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if strings.TrimSpace(result) != "hello world" {
		t.Errorf("result = %q", result)
	}
}

func TestExecWithTimeout(t *testing.T) {
	tool := NewExecTool(nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "echo fast",
		"timeout": 5,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "fast") {
		t.Errorf("result = %q", result)
	}
}

func TestExecTimeout(t *testing.T) {
	tool := NewExecTool(nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "sleep 60",
		"timeout": 1,
	})

	result, err := tool.Execute(context.Background(), params)
	// Should not return Go error — error is in result text
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if !strings.Contains(result, "Error:") {
		t.Errorf("expected error in result, got %q", result)
	}
}

func TestExecFailedCommand(t *testing.T) {
	tool := NewExecTool(nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "false",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if !strings.Contains(result, "Error:") {
		t.Errorf("expected error in result, got %q", result)
	}
}

func TestExecStderr(t *testing.T) {
	tool := NewExecTool(nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "echo stderr_msg >&2",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "stderr_msg") {
		t.Errorf("expected stderr in result, got %q", result)
	}
}

func TestExecInvalidParams(t *testing.T) {
	tool := NewExecTool(nil)
	_, err := tool.Execute(context.Background(), json.RawMessage(`{invalid`))
	if err == nil {
		t.Fatal("expected error for invalid params")
	}
}

func TestExecMultilineOutput(t *testing.T) {
	tool := NewExecTool(nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "printf 'line1\nline2\nline3'",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 3 {
		t.Errorf("got %d lines, want 3", len(lines))
	}
}
