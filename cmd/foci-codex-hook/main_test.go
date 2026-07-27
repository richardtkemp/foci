package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/delegator/sessionenv"
)

func writeBinding(t *testing.T, dir, threadID, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, threadID+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const payload = `{"hook_event_name":"PreToolUse","session_id":"thread-1","cwd":"/w",` +
	`"tool_name":"Bash","tool_input":{"command":"echo hi"},"tool_use_id":"call_1"}`

func TestDecide_RewritesBashForABoundThread(t *testing.T) {
	dir := t.TempDir()
	writeBinding(t, dir, "thread-1", `{"FOCI_SOCK":"/tmp/a.sock","FOCI_SESSION_KEY":"agent/cA","BASH_ENV":"/tmp/a.sh"}`)

	out, ok := decide(strings.NewReader(payload), dir)
	if !ok {
		t.Fatal("expected a rewrite for a bound thread")
	}
	if out.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Errorf("hookEventName = %q", out.HookSpecificOutput.HookEventName)
	}
	// Codex rejects updatedInput unless the decision is exactly "allow".
	if out.HookSpecificOutput.PermissionDecision != "allow" {
		t.Errorf("permissionDecision = %q, want allow", out.HookSpecificOutput.PermissionDecision)
	}
	got, unwrapped := sessionenv.UnwrapCommand(out.HookSpecificOutput.UpdatedInput.Command)
	if !unwrapped || got != "echo hi" {
		t.Errorf("rewritten command %q does not unwrap to the original", out.HookSpecificOutput.UpdatedInput.Command)
	}
	for _, want := range []string{"FOCI_SESSION_KEY=agent/cA", "FOCI_SOCK=/tmp/a.sock", "BASH_ENV=/tmp/a.sh"} {
		if !strings.Contains(out.HookSpecificOutput.UpdatedInput.Command, want) {
			t.Errorf("rewritten command missing %s: %q", want, out.HookSpecificOutput.UpdatedInput.Command)
		}
	}
}

// Two threads on one shared app-server must each pick up their own binding —
// this is the whole bug the hook exists to fix.
func TestDecide_IsPerThread(t *testing.T) {
	dir := t.TempDir()
	writeBinding(t, dir, "thread-A", `{"FOCI_SESSION_KEY":"agent/cA","FOCI_SOCK":"/tmp/a.sock"}`)
	writeBinding(t, dir, "thread-B", `{"FOCI_SESSION_KEY":"agent/cB","FOCI_SOCK":"/tmp/b.sock"}`)

	for _, tc := range []struct{ thread, wantKey, wantSock string }{
		{"thread-A", "agent/cA", "/tmp/a.sock"},
		{"thread-B", "agent/cB", "/tmp/b.sock"},
	} {
		in := strings.Replace(payload, `"session_id":"thread-1"`, `"session_id":"`+tc.thread+`"`, 1)
		out, ok := decide(strings.NewReader(in), dir)
		if !ok {
			t.Fatalf("%s: expected a rewrite", tc.thread)
		}
		cmd := out.HookSpecificOutput.UpdatedInput.Command
		if !strings.Contains(cmd, "FOCI_SESSION_KEY="+tc.wantKey) || !strings.Contains(cmd, "FOCI_SOCK="+tc.wantSock) {
			t.Errorf("%s got %q, want key=%s sock=%s", tc.thread, cmd, tc.wantKey, tc.wantSock)
		}
	}
}

// Silence is the safe answer: a hook that emits garbage or exits non-zero
// BLOCKS the tool call, so every unrecognised case must produce nothing.
func TestDecide_StaysSilent(t *testing.T) {
	dir := t.TempDir()
	writeBinding(t, dir, "thread-1", `{"FOCI_SESSION_KEY":"agent/cA"}`)
	writeBinding(t, dir, "thread-empty", `{}`)
	writeBinding(t, dir, "thread-bad", `not json`)

	cases := map[string]string{
		"unbound thread":  strings.Replace(payload, `"thread-1"`, `"thread-unknown"`, 1),
		"non-bash tool":   strings.Replace(payload, `"tool_name":"Bash"`, `"tool_name":"Read"`, 1),
		"empty command":   strings.Replace(payload, `"command":"echo hi"`, `"command":""`, 1),
		"no session id":   strings.Replace(payload, `"session_id":"thread-1"`, `"session_id":""`, 1),
		"empty binding":   strings.Replace(payload, `"thread-1"`, `"thread-empty"`, 1),
		"corrupt binding": strings.Replace(payload, `"thread-1"`, `"thread-bad"`, 1),
		"malformed input": `{"tool_name":`,
		"empty input":     ``,
	}
	for name, in := range cases {
		if _, ok := decide(strings.NewReader(in), dir); ok {
			t.Errorf("%s: expected silence", name)
		}
	}
}

func TestDecide_SilentWhenEnvDirUnset(t *testing.T) {
	if _, ok := decide(strings.NewReader(payload), ""); ok {
		t.Error("expected silence with no --env-dir")
	}
}

// The emitted JSON is a wire contract with codex; assert the exact key names.
func TestOutputWireShape(t *testing.T) {
	dir := t.TempDir()
	writeBinding(t, dir, "thread-1", `{"FOCI_SESSION_KEY":"agent/cA"}`)
	out, ok := decide(strings.NewReader(payload), dir)
	if !ok {
		t.Fatal("expected a rewrite")
	}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatal(err)
	}
	hso, ok := generic["hookSpecificOutput"]
	if !ok {
		t.Fatalf("missing hookSpecificOutput: %s", body)
	}
	for _, k := range []string{"hookEventName", "permissionDecision", "updatedInput"} {
		if _, ok := hso[k]; !ok {
			t.Errorf("missing %s: %s", k, body)
		}
	}
}
