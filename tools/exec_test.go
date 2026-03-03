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

func TestExecOutputNoTruncation(t *testing.T) {
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	// Generate output >100k chars — exec no longer truncates (guardToolResult handles it)
	params, _ := json.Marshal(map[string]interface{}{
		"command": "python3 -c 'print(\"x\" * 110000)'",
		"timeout": 10,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(result, "truncated") {
		t.Errorf("exec should no longer truncate output (guardToolResult handles it)")
	}
	if len(result) < 110_000 {
		t.Errorf("result length = %d, expected full output", len(result))
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

func TestExecSecretInHTTPRequestAllowed(t *testing.T) {
	// Secret refs inside foci_http_request should be allowed (passed as literals)
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": `foci_http_request --header "Authorization: Bearer {{secret:coolify.api_token}}" "https://example.com/api" | jq '.name'`,
	})

	// Should not error — the secret ref is in foci_http_request scope
	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("secret ref in foci_http_request should be allowed: %v", err)
	}
}

func TestExecSecretInHTTPRequestMultipleArgs(t *testing.T) {
	// Multiple secret refs, all inside foci_http_request
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": `foci_http_request --header "Authorization: {{secret:api.token}}" --header "X-Key: {{secret:api.key}}" "https://example.com"`,
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("multiple secret refs in foci_http_request should be allowed: %v", err)
	}
}

func TestExecSecretOutsideHTTPRequestBlocked(t *testing.T) {
	// Secret ref after a pipe (outside foci_http_request scope) should be blocked
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": `foci_http_request --header "Authorization: {{secret:api.token}}" "https://example.com" | echo {{secret:api.key}}`,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error when secret ref is outside foci_http_request scope")
	}
	if !strings.Contains(err.Error(), "not allowed in exec") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestExecSecretInHTTPRequestAndBareBlocked(t *testing.T) {
	// One secret in foci_http_request, another in a separate command — should block
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": `foci_http_request --header "{{secret:api.token}}" url && curl -H "{{secret:api.key}}" url2`,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error when secret ref appears in non-http_request segment")
	}
}

func TestExecSecretInHTTPRequestWithSemicolon(t *testing.T) {
	// Secret ref after semicolon is a new command — should block
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": `foci_http_request url; echo {{secret:leak.me}}`,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error when secret ref is after semicolon")
	}
}

func TestExecBareSecretStillBlocked(t *testing.T) {
	// No foci_http_request at all — secret refs should be blocked as before
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": `curl -H "Authorization: {{secret:api.token}}" https://example.com`,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for bare secret ref without foci_http_request")
	}
}

func TestAllSecretRefsInHTTPRequestScope(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want bool
	}{
		{
			name: "single ref in http_request",
			cmd:  `foci_http_request --header "{{secret:api.token}}" url`,
			want: true,
		},
		{
			name: "ref after pipe",
			cmd:  `foci_http_request url | echo {{secret:x}}`,
			want: false,
		},
		{
			name: "ref in http_request before pipe, no ref after",
			cmd:  `foci_http_request --header "{{secret:x}}" url | jq '.'`,
			want: true,
		},
		{
			name: "ref in both segments",
			cmd:  `foci_http_request "{{secret:x}}" url | grep {{secret:y}}`,
			want: false,
		},
		{
			name: "ref after &&",
			cmd:  `foci_http_request url && echo {{secret:x}}`,
			want: false,
		},
		{
			name: "ref after ||",
			cmd:  `foci_http_request url || echo {{secret:x}}`,
			want: false,
		},
		{
			name: "ref after semicolon",
			cmd:  `foci_http_request url; echo {{secret:x}}`,
			want: false,
		},
		{
			name: "no http_request at all",
			cmd:  `echo {{secret:x}}`,
			want: false,
		},
		{
			name: "no secret refs",
			cmd:  `foci_http_request url | jq '.'`,
			want: true,
		},
		{
			name: "multiple refs all in http_request",
			cmd:  `foci_http_request --header "{{secret:a}}" --header "{{secret:b}}" url`,
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := allSecretRefsInHTTPRequestScope(tt.cmd)
			if got != tt.want {
				t.Errorf("allSecretRefsInHTTPRequestScope(%q) = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}
}

func TestExecOutputModeSeparated(t *testing.T) {
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command":     "echo out && echo err >&2",
		"output_mode": "separated",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var out separatedOutput
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal: %v (raw: %q)", err, result)
	}
	if strings.TrimSpace(out.Stdout) != "out" {
		t.Errorf("stdout = %q, want %q", out.Stdout, "out\n")
	}
	if strings.TrimSpace(out.Stderr) != "err" {
		t.Errorf("stderr = %q, want %q", out.Stderr, "err\n")
	}
	if out.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", out.ExitCode)
	}
}

func TestExecOutputModeSeparatedStdoutOnly(t *testing.T) {
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command":     "echo hello",
		"output_mode": "separated",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var out separatedOutput
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if strings.TrimSpace(out.Stdout) != "hello" {
		t.Errorf("stdout = %q", out.Stdout)
	}
	if out.Stderr != "" {
		t.Errorf("stderr = %q, want empty", out.Stderr)
	}
	if out.ExitCode != 0 {
		t.Errorf("exit_code = %d", out.ExitCode)
	}
}

func TestExecOutputModeSeparatedStderrOnly(t *testing.T) {
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command":     "echo err >&2",
		"output_mode": "separated",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var out separatedOutput
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Stdout != "" {
		t.Errorf("stdout = %q, want empty", out.Stdout)
	}
	if strings.TrimSpace(out.Stderr) != "err" {
		t.Errorf("stderr = %q", out.Stderr)
	}
}

func TestExecOutputModeSeparatedFailure(t *testing.T) {
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command":     "echo before-fail; exit 42",
		"output_mode": "separated",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var out separatedOutput
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if strings.TrimSpace(out.Stdout) != "before-fail" {
		t.Errorf("stdout = %q", out.Stdout)
	}
	if out.ExitCode != 42 {
		t.Errorf("exit_code = %d, want 42", out.ExitCode)
	}
}

func TestExecOutputModeCombinedDefault(t *testing.T) {
	// Omitting output_mode should behave exactly like the original combined mode
	tool := NewExecTool(nil, nil, 0, nil, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "echo out && echo err >&2",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Combined mode returns raw text, not JSON
	if !strings.Contains(result, "out") {
		t.Errorf("result should contain stdout, got %q", result)
	}
	if !strings.Contains(result, "err") {
		t.Errorf("result should contain stderr, got %q", result)
	}
	// Should NOT be valid separatedOutput JSON
	var out separatedOutput
	if json.Unmarshal([]byte(result), &out) == nil && out.Stdout != "" {
		t.Error("combined mode should not return separated JSON")
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
	if !strings.Contains(err.Error(), "remind") {
		t.Errorf("error should mention remind, got: %v", err)
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
	if !strings.Contains(err.Error(), "remind") {
		t.Errorf("error should mention remind, got: %v", err)
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

func TestExecAutoBackgroundCtxCancelled(t *testing.T) {
	// When the parent context is cancelled mid-execution (turn cancelled),
	// results should still be delivered via the notifier — not silently lost.
	completeCh := make(chan string, 1)
	notifier := NewAsyncNotifier(func(sk, msg string) {
		completeCh <- msg
	})
	// Use a 10s threshold so the ctx.Done() path fires before the threshold.
	tool := NewExecTool(nil, nil, 10, notifier, "", nil)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "echo ctx-cancel-result; timeout 3 tail -f /dev/null",
		"timeout": 10,
	})

	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithSessionKey(ctx, "agent:test:cancel-42")

	// Cancel the context shortly after starting
	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()

	_, err := tool.Execute(ctx, params)
	if err == nil {
		t.Fatal("expected context cancelled error")
	}

	// The notifier should deliver the result when the command finishes
	select {
	case msg := <-completeCh:
		if !strings.Contains(msg, "ctx-cancel-result") {
			t.Errorf("expected command output in notification, got %q", msg)
		}
		if !strings.Contains(msg, "EXEC RESULT") {
			t.Errorf("expected [EXEC RESULT] header, got %q", msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for notifier — result was silently lost")
	}

	// Pending count should be back to zero
	if notifier.HasPending("agent:test:cancel-42") {
		t.Error("pending count should be zero after delivery")
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
