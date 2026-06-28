//go:build ignore
// Content below is fully disabled (no kept tests); Step 9+ replaces with fresh tests.
package opencode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Hook settings JSON build
// ---------------------------------------------------------------------------

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestBuildHookSettingsJSON(t *testing.T) {
// 	cmd := buildHookCommand("/bin/foci-cc-hook", "abc123")
// 	body, err := buildHookSettingsJSON(cmd)
// 	if err != nil {
// 		t.Fatalf("buildHookSettingsJSON: %v", err)
// 	}
//
// 	var parsed struct {
// 		Hooks map[string][]hookMatcher `json:"hooks"`
// 	}
// 	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
// 		t.Fatalf("parse generated settings: %v (body: %s)", err, body)
// 	}
//
// 	for _, event := range []string{eventPostToolUse, eventPostToolUseFailure} {
// 		matchers, ok := parsed.Hooks[event]
// 		if !ok {
// 			t.Errorf("generated settings missing event %q", event)
// 			continue
// 		}
// 		if len(matchers) != 1 {
// 			t.Errorf("%s matchers = %d, want 1", event, len(matchers))
// 			continue
// 		}
// 		m := matchers[0]
// 		if m.Matcher != "*" {
// 			t.Errorf("%s matcher = %q, want *", event, m.Matcher)
// 		}
// 		if len(m.Hooks) != 1 {
// 			t.Fatalf("%s hook specs = %d, want 1", event, len(m.Hooks))
// 		}
// 		h := m.Hooks[0]
// 		if h.Type != "command" {
// 			t.Errorf("%s hook.type = %q, want command", event, h.Type)
// 		}
// 		if h.Command != cmd {
// 			t.Errorf("%s hook.command = %q, want %q", event, h.Command, cmd)
// 		}
// 		if h.Timeout != hookTimeoutSeconds {
// 			t.Errorf("%s hook.timeout = %d, want %d", event, h.Timeout, hookTimeoutSeconds)
// 		}
// 	}
// }

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestBuildHookCommand_Format(t *testing.T) {
// 	got := buildHookCommand("/bin/foci-cc-hook", "abc123")
// 	want := `"/bin/foci-cc-hook" --install abc123`
// 	if got != want {
// 		t.Errorf("buildHookCommand = %q, want %q", got, want)
// 	}
// }

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestBuildHookCommand_QuotesPathWithSpaces(t *testing.T) {
// 	got := buildHookCommand("/home/user name/bin/foci-cc-hook", "id-1")
// 	if !strings.Contains(got, `"/home/user name/bin/foci-cc-hook"`) {
// 		t.Errorf("expected quoted path with spaces, got %q", got)
// 	}
// }

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestNewInstallID_Unique(t *testing.T) {
// 	seen := map[string]bool{}
// 	for i := 0; i < 100; i++ {
// 		id := newInstallID()
// 		if id == "" {
// 			t.Fatal("empty install ID")
// 		}
// 		if seen[id] {
// 			t.Fatalf("duplicate install ID after %d iterations: %s", i, id)
// 		}
// 		seen[id] = true
// 	}
// }

// ---------------------------------------------------------------------------
// Hook binary resolution
// ---------------------------------------------------------------------------

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestIsExecutableFile(t *testing.T) {
// 	dir := t.TempDir()
//
// 	exe := filepath.Join(dir, "exe")
// 	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
// 		t.Fatal(err)
// 	}
// 	if !isExecutableFile(exe) {
// 		t.Error("executable file not recognised")
// 	}
//
// 	plain := filepath.Join(dir, "plain")
// 	if err := os.WriteFile(plain, []byte("hi"), 0o644); err != nil {
// 		t.Fatal(err)
// 	}
// 	if isExecutableFile(plain) {
// 		t.Error("plain file classified as executable")
// 	}
//
// 	if isExecutableFile(dir) {
// 		t.Error("directory classified as executable")
// 	}
//
// 	if isExecutableFile(filepath.Join(dir, "missing")) {
// 		t.Error("missing path classified as executable")
// 	}
// }

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestResolveHookBinary_SiblingFound(t *testing.T) {
// 	self, err := os.Executable()
// 	if err != nil {
// 		t.Skipf("os.Executable unavailable: %v", err)
// 	}
// 	sibling := filepath.Join(filepath.Dir(self), hookCommandName)
// 	if !isExecutableFile(sibling) {
// 		t.Skipf("no foci-cc-hook sibling at %s (run `make foci-cc-hook` before `make test` to enable)", sibling)
// 	}
//
// 	t.Setenv("PATH", "")
//
// 	got, err := resolveHookBinary()
// 	if err != nil {
// 		t.Fatalf("resolveHookBinary: %v", err)
// 	}
// 	if got != sibling {
// 		t.Errorf("got %q, want sibling %q", got, sibling)
// 	}
// }

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestResolveHookBinary_PathFallback(t *testing.T) {
// 	dir := t.TempDir()
// 	fake := filepath.Join(dir, hookCommandName)
// 	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
// 		t.Fatal(err)
// 	}
// 	t.Setenv("PATH", dir)
//
// 	got, err := resolveHookBinary()
// 	if err != nil {
// 		t.Fatalf("resolveHookBinary: %v", err)
// 	}
// 	if got != fake {
// 		t.Errorf("got %q, want %q", got, fake)
// 	}
// }

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestResolveHookBinary_NotFound(t *testing.T) {
// 	t.Setenv("PATH", "")
//
// 	if self, err := os.Executable(); err == nil {
// 		sibling := filepath.Join(filepath.Dir(self), hookCommandName)
// 		if isExecutableFile(sibling) {
// 			t.Skipf("sibling %s exists; can't test NotFound path here", sibling)
// 		}
// 	}
//
// 	_, err := resolveHookBinary()
// 	if err == nil {
// 		t.Fatal("resolveHookBinary succeeded, want error")
// 	}
// 	if msg := err.Error(); !strings.Contains(msg, hookCommandName) {
// 		t.Errorf("error message = %q, want to mention %q", msg, hookCommandName)
// 	}
// }

// ---------------------------------------------------------------------------
// handleHookResponse dispatch
// ---------------------------------------------------------------------------

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestHandleHookResponse_PostToolUse(t *testing.T) {
// 	b := &Backend{hookInstallID: "install-a"}
//
// 	type captured struct {
// 		id, name, output string
// 		isErr            bool
// 	}
// 	var got []captured
// 	handler := &testHandler{
// 		OnToolEnd: func(id, name, output string, isError bool) {
// 			got = append(got, captured{id, name, output, isError})
// 		},
// 	}
// 	applyHandler(b, handler)
//
// 	stdout, _ := json.Marshal(hookScriptOutput{
// 		HookEvent:    "PostToolUse",
// 		InstallID:    "install-a",
// 		ToolUseID:    "toolu_1",
// 		ToolName:     "Read",
// 		ToolResponse: "file contents",
// 		IsError:      false,
// 	})
// 	env, _ := json.Marshal(hookResponseEnvelope{
// 		HookEvent: "PostToolUse",
// 		Stdout:    string(stdout),
// 		ExitCode:  0,
// 		Outcome:   "success",
// 	})
// 	b.handleHookResponse(env)
//
// 	if len(got) != 1 {
// 		t.Fatalf("OnToolEnd calls = %d, want 1", len(got))
// 	}
// 	c := got[0]
// 	if c.id != "toolu_1" || c.name != "Read" || c.output != "file contents" || c.isErr {
// 		t.Errorf("captured = %+v", c)
// 	}
// }

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestHandleHookResponse_PostToolUseFailure(t *testing.T) {
// 	b := &Backend{hookInstallID: "install-b"}
// 	var captured struct {
// 		id, name, output string
// 		isErr            bool
// 	}
// 	handler := &testHandler{
// 		OnToolEnd: func(id, name, output string, isError bool) {
// 			captured.id = id
// 			captured.name = name
// 			captured.output = output
// 			captured.isErr = isError
// 		},
// 	}
// 	applyHandler(b, handler)
//
// 	stdout, _ := json.Marshal(hookScriptOutput{
// 		HookEvent: "PostToolUseFailure",
// 		InstallID: "install-b",
// 		ToolUseID: "toolu_2",
// 		ToolName:  "Write",
// 		Error:     "Permission denied",
// 		IsError:   true,
// 	})
// 	env, _ := json.Marshal(hookResponseEnvelope{
// 		HookEvent: "PostToolUseFailure",
// 		Stdout:    string(stdout),
// 	})
// 	b.handleHookResponse(env)
//
// 	if !captured.isErr {
// 		t.Error("isError = false, want true for PostToolUseFailure")
// 	}
// 	if captured.output != "Permission denied" {
// 		t.Errorf("output = %q, want Permission denied", captured.output)
// 	}
// }

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestHandleHookResponse_FiltersForeignInstallID(t *testing.T) {
// 	b := &Backend{hookInstallID: "install-us"}
// 	fired := false
// 	handler := &testHandler{
// 		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
// 	}
// 	applyHandler(b, handler)
//
// 	stdout, _ := json.Marshal(hookScriptOutput{
// 		HookEvent:    "PostToolUse",
// 		InstallID:    "install-someone-else",
// 		ToolUseID:    "toolu_x",
// 		ToolName:     "Read",
// 		ToolResponse: "not ours",
// 	})
// 	env, _ := json.Marshal(hookResponseEnvelope{
// 		HookEvent: "PostToolUse",
// 		Stdout:    string(stdout),
// 	})
// 	b.handleHookResponse(env)
//
// 	if fired {
// 		t.Error("OnToolEnd fired for foreign install_id")
// 	}
// }

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestHandleHookResponse_FiltersUserHookNoID(t *testing.T) {
// 	b := &Backend{hookInstallID: "install-us"}
// 	fired := false
// 	handler := &testHandler{
// 		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
// 	}
// 	applyHandler(b, handler)
//
// 	// Payload without install_id — the user's hook script doesn't echo one.
// 	env, _ := json.Marshal(hookResponseEnvelope{
// 		HookEvent: "PostToolUse",
// 		Stdout:    `{"tool_use_id":"toolu_user","tool_name":"Bash"}`,
// 	})
// 	b.handleHookResponse(env)
//
// 	if fired {
// 		t.Error("OnToolEnd fired for payload with no install_id")
// 	}
// }

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestHandleHookResponse_DropsEventsWhenHooksDisabled(t *testing.T) {
// 	b := &Backend{} // hookInstallID intentionally empty — install failed
// 	fired := false
// 	handler := &testHandler{
// 		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
// 	}
// 	applyHandler(b, handler)
//
// 	// A user-configured hook fires with no install_id (it wasn't foci's).
// 	env, _ := json.Marshal(hookResponseEnvelope{
// 		HookEvent: "PostToolUse",
// 		Stdout:    `{"tool_use_id":"toolu_user","tool_name":"Bash","tool_response":"ok"}`,
// 	})
// 	b.handleHookResponse(env)
//
// 	if fired {
// 		t.Error("OnToolEnd fired for user hook event when foci hooks were not installed")
// 	}
// }

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestHandleHookResponse_SkipsSubagent(t *testing.T) {
// 	b := &Backend{hookInstallID: "install-us"}
// 	fired := false
// 	handler := &testHandler{
// 		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
// 	}
// 	applyHandler(b, handler)
//
// 	stdout, _ := json.Marshal(hookScriptOutput{
// 		HookEvent:    "PostToolUse",
// 		InstallID:    "install-us",
// 		ToolUseID:    "toolu_sub",
// 		ToolName:     "Read",
// 		ToolResponse: "nested result",
// 		AgentID:      "agent-child-7",
// 	})
// 	env, _ := json.Marshal(hookResponseEnvelope{
// 		HookEvent: "PostToolUse",
// 		Stdout:    string(stdout),
// 	})
// 	b.handleHookResponse(env)
//
// 	if fired {
// 		t.Error("OnToolEnd fired for sub-agent hook event")
// 	}
// }

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestHandleHookResponse_SkipsUnknownHookEvent(t *testing.T) {
// 	b := &Backend{hookInstallID: "install-us"}
// 	fired := false
// 	handler := &testHandler{
// 		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
// 	}
// 	applyHandler(b, handler)
//
// 	env, _ := json.Marshal(hookResponseEnvelope{
// 		HookEvent: "PreToolUse",
// 		Stdout:    `{"tool_use_id":"x","tool_name":"y","install_id":"install-us"}`,
// 	})
// 	b.handleHookResponse(env)
//
// 	if fired {
// 		t.Error("OnToolEnd fired for PreToolUse event")
// 	}
// }

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestHandleHookResponse_MalformedStdoutGracefulSkip(t *testing.T) {
// 	b := &Backend{hookInstallID: "install-us"}
// 	fired := false
// 	handler := &testHandler{
// 		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
// 	}
// 	applyHandler(b, handler)
//
// 	env, _ := json.Marshal(hookResponseEnvelope{
// 		HookEvent: "PostToolUse",
// 		Stdout:    "not valid json {{{",
// 	})
// 	b.handleHookResponse(env)
//
// 	if fired {
// 		t.Error("OnToolEnd fired despite malformed stdout")
// 	}
// }

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestHandleHookResponse_EmptyStdoutSilent(t *testing.T) {
// 	b := &Backend{hookInstallID: "install-us"}
// 	fired := false
// 	handler := &testHandler{
// 		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
// 	}
// 	applyHandler(b, handler)
//
// 	env, _ := json.Marshal(hookResponseEnvelope{
// 		HookEvent: "PostToolUse",
// 		Stdout:    "",
// 	})
// 	b.handleHookResponse(env)
//
// 	if fired {
// 		t.Error("OnToolEnd fired for empty stdout")
// 	}
// }

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestHandleHookResponse_PostToolNudgeDispatched(t *testing.T) {
// 	var buf bytes.Buffer
// 	b := &Backend{
// 		hookInstallID: "install-us",
// 		writer:        NewWriter(nopWriteCloser{&buf}),
// 	}
//
// 	handler := &testHandler{
// 		OnToolEnd: func(_, _, _ string, _ bool) {},
// 		PostToolNudgeFunc: func(name, _ string, isErr bool) []string {
// 			if name == "Bash" && !isErr {
// 				return []string{"reminder-text"}
// 			}
// 			return nil
// 		},
// 	}
// 	applyHandler(b, handler)
//
// 	stdout, _ := json.Marshal(hookScriptOutput{
// 		HookEvent:    "PostToolUse",
// 		InstallID:    "install-us",
// 		ToolUseID:    "toolu_1",
// 		ToolName:     "Bash",
// 		ToolResponse: "ok",
// 	})
// 	env, _ := json.Marshal(hookResponseEnvelope{
// 		HookEvent: "PostToolUse",
// 		Stdout:    string(stdout),
// 	})
// 	b.handleHookResponse(env)
//
// 	if !strings.Contains(buf.String(), "[user] reminder-text") {
// 		t.Errorf("expected [user] reminder-text in writer output, got: %q", buf.String())
// 	}
// 	if strings.Contains(buf.String(), `"priority"`) {
// 		t.Errorf("priority field should be absent on default-priority post-tool nudge, got: %q", buf.String())
// 	}
// }

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestHandleHookResponse_PostToolNudgeNilFunc(t *testing.T) {
// 	var buf bytes.Buffer
// 	b := &Backend{
// 		hookInstallID: "install-us",
// 		writer:        NewWriter(nopWriteCloser{&buf}),
// 	}
// 	handler := &testHandler{
// 		OnToolEnd: func(_, _, _ string, _ bool) {},
// 	}
// 	applyHandler(b, handler)
//
// 	stdout, _ := json.Marshal(hookScriptOutput{
// 		HookEvent:    "PostToolUse",
// 		InstallID:    "install-us",
// 		ToolUseID:    "toolu_1",
// 		ToolName:     "Read",
// 		ToolResponse: "ok",
// 	})
// 	env, _ := json.Marshal(hookResponseEnvelope{
// 		HookEvent: "PostToolUse",
// 		Stdout:    string(stdout),
// 	})
// 	b.handleHookResponse(env)
//
// 	if buf.Len() != 0 {
// 		t.Errorf("writer should be empty with nil PostToolNudgeFunc, got: %q", buf.String())
// 	}
// }

// DISABLED(opencode): ccstream's foci-cc-hook helper — opencode emits tool parts directly on its event bus, no hook needed.
// func TestHandleHookResponse_PostToolNudgeSkipsEmpty(t *testing.T) {
// 	var buf bytes.Buffer
// 	b := &Backend{
// 		hookInstallID: "install-us",
// 		writer:        NewWriter(nopWriteCloser{&buf}),
// 	}
// 	handler := &testHandler{
// 		OnToolEnd: func(_, _, _ string, _ bool) {},
// 		PostToolNudgeFunc: func(_, _ string, _ bool) []string {
// 			return []string{"", "real", ""}
// 		},
// 	}
// 	applyHandler(b, handler)
//
// 	stdout, _ := json.Marshal(hookScriptOutput{
// 		HookEvent:    "PostToolUse",
// 		InstallID:    "install-us",
// 		ToolUseID:    "toolu_1",
// 		ToolName:     "Read",
// 		ToolResponse: "ok",
// 	})
// 	env, _ := json.Marshal(hookResponseEnvelope{
// 		HookEvent: "PostToolUse",
// 		Stdout:    string(stdout),
// 	})
// 	b.handleHookResponse(env)
//
// 	lines := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1
// 	if !strings.Contains(buf.String(), "[user] real") {
// 		t.Errorf("expected [user] real in output, got: %q", buf.String())
// 	}
// 	if lines != 1 {
// 		t.Errorf("expected exactly 1 writer line (empty nudges skipped), got %d: %q", lines, buf.String())
// 	}
// }
