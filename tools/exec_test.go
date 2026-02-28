package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/secrets"
)

func TestExecEcho(t *testing.T) {
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

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

func TestExecWorkDir(t *testing.T) {
	dir := t.TempDir()
	tool := NewExecTool(nil, nil, 0, nil, dir, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "pwd",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Resolve symlinks (macOS /tmp -> /private/tmp, etc.)
	want, _ := filepath.EvalSymlinks(dir)
	got := strings.TrimSpace(result)
	if got != want {
		t.Errorf("workdir: got %q, want %q", got, want)
	}
}

func TestExecWithTimeout(t *testing.T) {
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

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
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "read -t 60 < /dev/null",
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
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

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
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

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
	tool := NewExecTool(nil, nil, 0, nil, "", nil)
	_, err := tool.Execute(context.Background(), json.RawMessage(`{invalid`))
	if err == nil {
		t.Fatal("expected error for invalid params")
	}
}

func TestExecMultilineOutput(t *testing.T) {
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

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
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

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

func TestExecSecretTemplatesBlocked(t *testing.T) {
	dir := t.TempDir()
	secretsPath := filepath.Join(dir, "secrets.toml")
	os.WriteFile(secretsPath, []byte(`[custom]
token = "secret-value-12345"
`), 0644)

	store, err := secrets.Load(secretsPath)
	if err != nil {
		t.Fatalf("Load secrets: %v", err)
	}

	tool := NewExecTool(store, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "echo {{secret:custom.token}}",
	})

	_, err = tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for secret template in exec")
	}
	if !strings.Contains(err.Error(), "not allowed in exec") {
		t.Errorf("error = %q, want 'not allowed in exec'", err.Error())
	}
}

func TestExecSecretTemplatesBlockedNoStore(t *testing.T) {
	// Even without a store, regular secret templates should be rejected
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "curl -H 'Authorization: {{secret:api.key}}' https://example.com",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for secret template in exec without store")
	}
	if !strings.Contains(err.Error(), "not allowed in exec") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestExecBitwardenSecretsAllowed(t *testing.T) {
	// Bitwarden refs (bw.*) should NOT be blocked — they're approval-gated
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "echo '{{secret:bw.aaaa-1111}}'",
	})

	// Without a bwStore, the template passes through unresolved (not blocked)
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("bitwarden refs should not be blocked in exec: %v", err)
	}
	// With nil bwStore, template passes through literally
	if !strings.Contains(result, "{{secret:bw.aaaa-1111}}") {
		t.Errorf("result = %q, want template passed through", result)
	}
}

func TestExecMixedSecretsBlocked(t *testing.T) {
	// A mix of regular and bitwarden refs should still be blocked
	// (because regular refs are present)
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "curl -H '{{secret:api.key}}' -H '{{secret:bw.aaaa}}' https://example.com",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error when regular secret refs are mixed with bitwarden refs")
	}
	if !strings.Contains(err.Error(), "not allowed in exec") {
		t.Errorf("error = %q", err.Error())
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

	tool := NewExecTool(store, nil, 0, nil, "", nil)

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
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

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
	// Even with nil store, secret templates should be blocked
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "echo '{{secret:test.key}}'",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for secret template in exec with nil store")
	}
	if !strings.Contains(err.Error(), "not allowed in exec") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestExecAutoBackgroundFastCommand(t *testing.T) {
	// A fast command should complete before the threshold
	var called bool
	tool := NewExecTool(nil, nil, 5, NewAsyncNotifier(func(sk, msg string) {
		called = true
	}), "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "echo fast",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "fast") {
		t.Errorf("result = %q, want 'fast'", result)
	}
	if called {
		t.Error("notifier should not be called for fast commands")
	}
}

func TestExecAutoBackgroundSlowCommand(t *testing.T) {
	// A slow command should auto-background after 1 second
	completeCh := make(chan string, 1)
	tool := NewExecTool(nil, nil, 1, NewAsyncNotifier(func(sk, msg string) {
		completeCh <- msg
	}), "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "timeout 3 tail -f /dev/null",
		"timeout": 10,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should get the auto-background message
	if !strings.Contains(result, "still running") {
		t.Errorf("expected auto-background message, got %q", result)
	}

	// Wait for the command to complete
	select {
	case completed := <-completeCh:
		if strings.Contains(completed, "still running") {
			t.Errorf("should have completed, got auto-background message: %q", completed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for auto-backgrounded command")
	}
}

func TestExecAutoBackgroundSessionKeyPropagated(t *testing.T) {
	// Verify the session key from context reaches the notifier callback
	type result struct {
		sk, msg string
	}
	ch := make(chan result, 1)
	tool := NewExecTool(nil, nil, 1, NewAsyncNotifier(func(sk, msg string) {
		ch <- result{sk, msg}
	}), "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "timeout 3 tail -f /dev/null",
		"timeout": 10,
	})

	ctx := WithSessionKey(context.Background(), "agent:test:branch-42")
	out, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "still running") {
		t.Fatalf("expected auto-background message, got %q", out)
	}

	select {
	case r := <-ch:
		if r.sk != "agent:test:branch-42" {
			t.Errorf("session key = %q, want %q", r.sk, "agent:test:branch-42")
		}
		if r.msg == "" {
			t.Error("message should not be empty")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for notifier callback")
	}
}

func TestExecSleepBlocked(t *testing.T) {
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "sleep 5",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for sleep command")
	}
	if !strings.Contains(err.Error(), "memory_remind") {
		t.Errorf("error should mention memory_remind, got: %v", err)
	}
}

func TestExecSleepWithTimeUnitBlocked(t *testing.T) {
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "sleep 30s",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for sleep 30s command")
	}
	if !strings.Contains(err.Error(), "memory_remind") {
		t.Errorf("error should mention memory_remind, got: %v", err)
	}
}

func TestExecSleepCaseInsensitive(t *testing.T) {
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "SLEEP 10",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for SLEEP command")
	}
}

func TestExecSleepWithLeadingWhitespaceBlocked(t *testing.T) {
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "  sleep 5",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for '  sleep 5' command")
	}
}

func TestExecSleepWithChainedCommandBlocked(t *testing.T) {
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "sleep 5 && do_thing",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for 'sleep 5 && do_thing' command")
	}
}

func TestExecSleepNotBlockedInMiddle(t *testing.T) {
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "echo 'going to sleep'",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "going to sleep") {
		t.Errorf("expected 'going to sleep' in output, got: %q", result)
	}
}
