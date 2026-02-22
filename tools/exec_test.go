package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clod/secrets"
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

func TestExecBackgroundMode(t *testing.T) {
	tool := NewExecTool(nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command":    "echo bg",
		"background": true,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "bg") {
		t.Errorf("result = %q, want 'bg'", result)
	}
}

func TestExecSecretResolution(t *testing.T) {
	dir := t.TempDir()
	secretsPath := filepath.Join(dir, "secrets.toml")
	os.WriteFile(secretsPath, []byte(`[custom]
token = "secret-value-12345"
`), 0644)

	store, err := secrets.Load(secretsPath)
	if err != nil {
		t.Fatalf("Load secrets: %v", err)
	}

	tool := NewExecTool(store)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "echo {{secret:custom.token}}",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The secret value should have been resolved and then the output should contain it
	// But it should be redacted in output
	if strings.Contains(result, "secret-value-12345") {
		t.Errorf("secret value should be redacted in output: %q", result)
	}
	if !strings.Contains(result, "[REDACTED]") {
		t.Errorf("result should contain [REDACTED]: %q", result)
	}
}

func TestExecBlockedPath(t *testing.T) {
	dir := t.TempDir()
	secretsPath := filepath.Join(dir, "secrets.toml")
	os.WriteFile(secretsPath, []byte(`[test]
key = "value"
`), 0644)

	store, err := secrets.Load(secretsPath)
	if err != nil {
		t.Fatalf("Load secrets: %v", err)
	}

	tool := NewExecTool(store)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "cat secrets.toml",
	})

	_, err = tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for blocked path")
	}
	if !strings.Contains(err.Error(), "blocked path") {
		t.Errorf("error = %q, want 'blocked path'", err.Error())
	}
}

func TestExecOutputTruncation(t *testing.T) {
	tool := NewExecTool(nil)

	// Generate output >100k chars
	params, _ := json.Marshal(map[string]interface{}{
		"command": "python3 -c 'print(\"x\" * 110000)'",
		"timeout": 10,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "truncated") {
		t.Errorf("expected truncation notice in long output")
	}
	if len(result) > 110_000 {
		t.Errorf("result length = %d, expected truncated", len(result))
	}
}

func TestExecNilStoreWithTemplate(t *testing.T) {
	tool := NewExecTool(nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "echo '{{secret:test.key}}'",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// With nil store, template syntax should pass through literally
	if !strings.Contains(result, "{{secret:test.key}}") {
		t.Errorf("result = %q, want template passed through", result)
	}
}
