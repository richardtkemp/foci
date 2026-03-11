package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/secrets"
)

func TestExecEcho(t *testing.T) {
	tool := newTestExecTool()
	result, err := runExec(t, tool, "echo hello world")
	requireNoError(t, err)

	if strings.TrimSpace(result.Text) != "hello world" {
		t.Errorf("result = %q", result.Text)
	}
}

func TestExecWorkDir(t *testing.T) {
	dir := t.TempDir()
	tool := NewExecTool(nil, nil, 0, nil, dir, nil, 0, "")

	params, _ := json.Marshal(map[string]interface{}{
		"command": "pwd",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Resolve symlinks (macOS /tmp -> /private/tmp, etc.)
	want, _ := filepath.EvalSymlinks(dir)
	got := strings.TrimSpace(result.Text)
	if got != want {
		t.Errorf("workdir: got %q, want %q", got, want)
	}
}

func TestExecWithTimeout(t *testing.T) {
	tool := newTestExecTool()

	params, _ := json.Marshal(map[string]interface{}{
		"command": "echo fast",
		"timeout": 5,
	})

	result, err := tool.Execute(context.Background(), params)
	requireNoError(t, err)
	requireContains(t, result.Text, "fast")
}

func TestExecTimeout(t *testing.T) {
	tool := newTestExecTool()

	params, _ := json.Marshal(map[string]interface{}{
		"command": "read -t 60 < /dev/null",
		"timeout": 1,
	})

	result, err := tool.Execute(context.Background(), params)
	requireNoError(t, err)
	requireContains(t, result.Text, "Error:")
}

func TestExecFailedCommand(t *testing.T) {
	tool := newTestExecTool()
	result, err := runExec(t, tool, "false")
	requireNoError(t, err)
	requireContains(t, result.Text, "Error:")
}

func TestExecStderr(t *testing.T) {
	tool := newTestExecTool()
	result, err := runExec(t, tool, "echo stderr_msg >&2")
	requireNoError(t, err)
	requireContains(t, result.Text, "stderr_msg")
}

func TestExecInvalidParams(t *testing.T) {
	tool := newTestExecTool()
	_, err := tool.Execute(context.Background(), json.RawMessage(`{invalid`))
	requireError(t, err, "")
}

func TestExecMultilineOutput(t *testing.T) {
	tool := newTestExecTool()
	result, err := runExec(t, tool, "printf 'line1\nline2\nline3'")
	requireNoError(t, err)

	lines := strings.Split(strings.TrimSpace(result.Text), "\n")
	if len(lines) != 3 {
		t.Errorf("got %d lines, want 3", len(lines))
	}
}

func TestExecBackgroundMode(t *testing.T) {
	tool := newTestExecTool()

	params, _ := json.Marshal(map[string]interface{}{
		"command":    "echo bg",
		"background": true,
	})

	result, err := tool.Execute(context.Background(), params)
	requireNoError(t, err)
	requireContains(t, result.Text, "bg")
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

	tool := NewExecTool(store, nil, 0, nil, "", nil, 0, "")

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
	tool := newTestExecTool()
	_, err := runExec(t, tool, "curl -H 'Authorization: {{secret:api.key}}' https://example.com")
	requireError(t, err, "not allowed in exec")
}

func TestExecBitwardenSecretsAllowed(t *testing.T) {
	// Bitwarden refs (bw.*) should NOT be blocked — they're approval-gated
	tool := newTestExecTool()
	// Without a bwStore, the template passes through unresolved (not blocked)
	result, err := runExec(t, tool, "echo '{{secret:bw.aaaa-1111}}'")
	requireNoError(t, err)
	// With nil bwStore, template passes through literally
	requireContains(t, result.Text, "{{secret:bw.aaaa-1111}}")
}

func TestExecMixedSecretsBlocked(t *testing.T) {
	// A mix of regular and bitwarden refs should still be blocked
	// (because regular refs are present)
	tool := newTestExecTool()
	_, err := runExec(t, tool, "curl -H '{{secret:api.key}}' -H '{{secret:bw.aaaa}}' https://example.com")
	requireError(t, err, "not allowed in exec")
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

	tool := NewExecTool(store, nil, 0, nil, "", nil, 0, "")

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

func TestExecOutputSpill(t *testing.T) {
	// Large output spills to a temp file; result.Text contains head portion,
	// ResultFile points to the full output on disk.
	tmpDir := t.TempDir()
	threshold := 1000
	tool := NewExecTool(nil, nil, 0, nil, "", nil, threshold, tmpDir)

	params, _ := json.Marshal(map[string]interface{}{
		"command": "python3 -c 'print(\"x\" * 110000)'",
		"timeout": 10,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ResultFile == "" {
		t.Fatal("expected ResultFile to be set for large output")
	}
	if result.ResultSize < 110000 {
		t.Errorf("ResultSize = %d, want >= 110000", result.ResultSize)
	}
	// Text should contain the head portion, not the full output
	if len(result.Text) > threshold+500 { // some slack for formatting
		t.Errorf("result.Text length = %d, expected roughly %d (head portion)", len(result.Text), threshold)
	}
	// Verify the full output exists on disk
	data, err := os.ReadFile(result.ResultFile)
	if err != nil {
		t.Fatalf("read spill file: %v", err)
	}
	if len(data) < 110000 {
		t.Errorf("spill file size = %d, want >= 110000", len(data))
	}
}

func TestExecNilStoreWithTemplate(t *testing.T) {
	// Even with nil store, secret templates should be blocked
	tool := newTestExecTool()
	_, err := runExec(t, tool, "echo '{{secret:test.key}}'")
	requireError(t, err, "not allowed in exec")
}

func TestExecAutoBackgroundFastCommand(t *testing.T) {
	// A fast command should complete before the threshold
	var called bool
	tool := NewExecTool(nil, nil, 5, NewAsyncNotifier(func(sk, msg string, replyTo string) {
		called = true
	}), "", nil, 0, "")

	params, _ := json.Marshal(map[string]interface{}{
		"command": "echo fast",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text,"fast") {
		t.Errorf("result = %q, want 'fast'", result.Text)
	}
	if called {
		t.Error("notifier should not be called for fast commands")
	}
}

func TestExecAutoBackgroundSlowCommand(t *testing.T) {
	t.Parallel()
	// A slow command should auto-background after 1 second
	completeCh := make(chan string, 1)
	tool := NewExecTool(nil, nil, 1, NewAsyncNotifier(func(sk, msg string, replyTo string) {
		completeCh <- msg
	}), "", nil, 0, "")

	params, _ := json.Marshal(map[string]interface{}{
		"command": "timeout 1.5 tail -f /dev/null",
		"timeout": 10,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should get the auto-background message
	if !strings.Contains(result.Text,"still running") {
		t.Errorf("expected auto-background message, got %q", result.Text)
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
	t.Parallel()
	// Verify the session key from context reaches the notifier callback
	type result struct {
		sk, msg string
	}
	ch := make(chan result, 1)
	tool := NewExecTool(nil, nil, 1, NewAsyncNotifier(func(sk, msg string, replyTo string) {
		ch <- result{sk, msg}
	}), "", nil, 0, "")

	params, _ := json.Marshal(map[string]interface{}{
		"command": "timeout 1.5 tail -f /dev/null",
		"timeout": 10,
	})

	ctx := WithSessionKey(context.Background(), "agent:test:branch-42")
	out, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "still running") {
		t.Fatalf("expected auto-background message, got %q", out.Text)
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
	tool := newTestExecTool()
	_, err := runExec(t, tool, `foci_http_request --header "Authorization: Bearer {{secret:coolify.api_token}}" "https://example.com/api" | jq '.name'`)
	requireNoError(t, err)
}

func TestExecSecretInHTTPRequestMultipleArgs(t *testing.T) {
	// Multiple secret refs, all inside foci_http_request
	tool := newTestExecTool()
	_, err := runExec(t, tool, `foci_http_request --header "Authorization: {{secret:api.token}}" --header "X-Key: {{secret:api.key}}" "https://example.com"`)
	requireNoError(t, err)
}

func TestExecSecretOutsideHTTPRequestBlocked(t *testing.T) {
	// Secret ref after a pipe (outside foci_http_request scope) should be blocked
	tool := newTestExecTool()
	_, err := runExec(t, tool, `foci_http_request --header "Authorization: {{secret:api.token}}" "https://example.com" | echo {{secret:api.key}}`)
	requireError(t, err, "not allowed in exec")
}

func TestExecSecretInHTTPRequestAndBareBlocked(t *testing.T) {
	// One secret in foci_http_request, another in a separate command — should block
	tool := newTestExecTool()
	_, err := runExec(t, tool, `foci_http_request --header "{{secret:api.token}}" url && curl -H "{{secret:api.key}}" url2`)
	requireError(t, err, "")
}

func TestExecSecretInHTTPRequestWithSemicolon(t *testing.T) {
	// Secret ref after semicolon is a new command — should block
	tool := newTestExecTool()
	_, err := runExec(t, tool, `foci_http_request url; echo {{secret:leak.me}}`)
	requireError(t, err, "")
}

func TestExecBareSecretStillBlocked(t *testing.T) {
	// No foci_http_request at all — secret refs should be blocked as before
	tool := newTestExecTool()
	_, err := runExec(t, tool, `curl -H "Authorization: {{secret:api.token}}" https://example.com`)
	requireError(t, err, "")
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
	tests := []struct {
		name       string
		cmd        string
		wantStdout string
		wantStderr string
		wantExit   int
	}{
		{"both", "echo out && echo err >&2", "out", "err", 0},
		{"stdout only", "echo hello", "hello", "", 0},
		{"stderr only", "echo err >&2", "", "err", 0},
		{"failure", "echo before-fail; exit 42", "before-fail", "", 42},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := newTestExecTool()
			params, _ := json.Marshal(map[string]interface{}{
				"command":     tt.cmd,
				"output_mode": "separated",
			})
			result, err := tool.Execute(context.Background(), params)
			requireNoError(t, err)

			var out separatedOutput
			if err := json.Unmarshal([]byte(result.Text), &out); err != nil {
				t.Fatalf("unmarshal: %v (raw: %q)", err, result.Text)
			}
			if strings.TrimSpace(out.Stdout) != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", out.Stdout, tt.wantStdout)
			}
			if strings.TrimSpace(out.Stderr) != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", out.Stderr, tt.wantStderr)
			}
			if out.ExitCode != tt.wantExit {
				t.Errorf("exit_code = %d, want %d", out.ExitCode, tt.wantExit)
			}
		})
	}
}

func TestExecOutputModeCombinedDefault(t *testing.T) {
	// Omitting output_mode should behave exactly like the original combined mode
	tool := newTestExecTool()
	result, err := runExec(t, tool, "echo out && echo err >&2")
	requireNoError(t, err)
	// Combined mode returns raw text, not JSON
	requireContains(t, result.Text, "out")
	requireContains(t, result.Text, "err")
	// Should NOT be valid separatedOutput JSON
	var out separatedOutput
	if json.Unmarshal([]byte(result.Text), &out) == nil && out.Stdout != "" {
		t.Error("combined mode should not return separated JSON")
	}
}

func TestExecSleepBlocked(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
	}{
		{"bare sleep", "sleep 5"},
		{"with time unit", "sleep 30s"},
		{"case insensitive", "SLEEP 10"},
		{"leading whitespace", "  sleep 5"},
		{"chained command", "sleep 5 && do_thing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := newTestExecTool()
			_, err := runExec(t, tool, tt.cmd)
			if err == nil {
				t.Fatalf("expected error for %q", tt.cmd)
			}
		})
	}
}

func TestExecAutoBackgroundCtxCancelled(t *testing.T) {
	t.Parallel()
	// When the parent context is cancelled mid-execution (turn cancelled),
	// results should still be delivered via the notifier — not silently lost.
	completeCh := make(chan string, 1)
	notifier := NewAsyncNotifier(func(sk, msg string, replyTo string) {
		completeCh <- msg
	})
	// Use a 10s threshold so the ctx.Done() path fires before the threshold.
	tool := NewExecTool(nil, nil, 10, notifier, "", nil, 0, "")

	params, _ := json.Marshal(map[string]interface{}{
		"command": "echo ctx-cancel-result; timeout 1.5 tail -f /dev/null",
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
	tool := newTestExecTool()
	result, err := runExec(t, tool, "echo 'going to sleep'")
	requireNoError(t, err)
	requireContains(t, result.Text, "going to sleep")
}
