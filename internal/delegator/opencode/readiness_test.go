//go:build ignore
// Content below is fully disabled (no kept tests); Step 9+ replaces with fresh tests.
package opencode

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeStubClaude writes an executable stub that emulates `claude auth status`
// by printing body to stdout and exiting with code. Returns its path.
func writeStubClaude(t *testing.T, body string, code int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-stub")
	script := "#!/bin/sh\ncat <<'EOF'\n" + body + "\nEOF\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

// TODO(opencode): rewrite — opencode readiness uses GET /global/health, not claude subprocess; see plan section 5.2
// func TestCheckReady_Authenticated(t *testing.T) {
// 	stub := writeStubClaude(t, `{"loggedIn": true, "authMethod": "claude.ai"}`, 0)
// 	fired := false
// 	b := &Backend{
// 		cfg:           map[string]any{"claude_binary": stub},
// 		onAuthFailure: func(string) { fired = true },
// 	}
//
// 	ready, err := b.CheckReady(context.Background())
// 	if err != nil {
// 		t.Fatalf("CheckReady err: %v", err)
// 	}
// 	if !ready {
// 		t.Errorf("ready = false; want true")
// 	}
// 	if fired {
// 		t.Errorf("onAuthFailure fired for an authenticated backend")
// 	}
// }

// TODO(opencode): rewrite — opencode readiness uses GET /global/health, not claude subprocess; see plan section 5.2
// func TestCheckReady_NotAuthenticated_TriggersRelogin(t *testing.T) {
// 	// Real `claude auth status` exits NON-ZERO when not logged in but still
// 	// prints {"loggedIn": false} to stdout. The exit code must not be mistaken
// 	// for a probe failure — we must parse the body and trigger login.
// 	stub := writeStubClaude(t, `{"loggedIn": false, "authMethod": "none"}`, 1)
// 	var detail string
// 	fired := false
// 	b := &Backend{
// 		cfg:           map[string]any{"claude_binary": stub},
// 		onAuthFailure: func(d string) { fired = true; detail = d },
// 	}
//
// 	ready, err := b.CheckReady(context.Background())
// 	if err != nil {
// 		t.Fatalf("CheckReady err: %v", err)
// 	}
// 	if ready {
// 		t.Errorf("ready = true; want false when not logged in")
// 	}
// 	if !fired {
// 		t.Fatalf("onAuthFailure not fired for an unauthenticated backend")
// 	}
// 	if detail == "" {
// 		t.Errorf("onAuthFailure detail empty")
// 	}
// }

// TODO(opencode): rewrite — opencode readiness uses GET /global/health, not claude subprocess; see plan section 5.2
// func TestCheckReady_ProbeError_NoTrigger(t *testing.T) {
// 	// Non-JSON output → parse error → indeterminate. Must NOT trigger login.
// 	stub := writeStubClaude(t, `not json at all`, 0)
// 	fired := false
// 	b := &Backend{
// 		cfg:           map[string]any{"claude_binary": stub},
// 		onAuthFailure: func(string) { fired = true },
// 	}
//
// 	ready, err := b.CheckReady(context.Background())
// 	if err == nil {
// 		t.Fatalf("CheckReady err = nil; want a parse error")
// 	}
// 	if ready {
// 		t.Errorf("ready = true; want false on probe error")
// 	}
// 	if fired {
// 		t.Errorf("onAuthFailure fired on an indeterminate probe (should not)")
// 	}
// }

// TODO(opencode): rewrite — opencode readiness uses GET /global/health, not claude subprocess; see plan section 5.2
// func TestCheckReady_BinaryFails_NoTrigger(t *testing.T) {
// 	// Binary exits non-zero with no parseable output → run error, no trigger.
// 	stub := writeStubClaude(t, ``, 3)
// 	fired := false
// 	b := &Backend{
// 		cfg:           map[string]any{"claude_binary": stub},
// 		onAuthFailure: func(string) { fired = true },
// 	}
//
// 	ready, err := b.CheckReady(context.Background())
// 	if err == nil {
// 		t.Fatalf("CheckReady err = nil; want a run error")
// 	}
// 	if ready || fired {
// 		t.Errorf("ready=%v fired=%v; want false/false on a failing binary", ready, fired)
// 	}
// }
