package ccstream

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

// TestBuildHookSettingsJSON proves the generated JSON has the shape CC
// expects (top-level hooks.PostToolUse and hooks.PostToolUseFailure each
// with a single matcher:"*" entry carrying the foci hook command). CC
// loads this via --settings <json> as a flagSettings source.
func TestBuildHookSettingsJSON(t *testing.T) {
	cmd := buildHookCommand("/bin/foci-cc-hook", "abc123")
	body, err := buildHookSettingsJSON(cmd)
	if err != nil {
		t.Fatalf("buildHookSettingsJSON: %v", err)
	}

	var parsed struct {
		Hooks map[string][]hookMatcher `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("parse generated settings: %v (body: %s)", err, body)
	}

	for _, event := range []string{eventPostToolUse, eventPostToolUseFailure} {
		matchers, ok := parsed.Hooks[event]
		if !ok {
			t.Errorf("generated settings missing event %q", event)
			continue
		}
		if len(matchers) != 1 {
			t.Errorf("%s matchers = %d, want 1", event, len(matchers))
			continue
		}
		m := matchers[0]
		if m.Matcher != "*" {
			t.Errorf("%s matcher = %q, want *", event, m.Matcher)
		}
		if len(m.Hooks) != 1 {
			t.Fatalf("%s hook specs = %d, want 1", event, len(m.Hooks))
		}
		h := m.Hooks[0]
		if h.Type != "command" {
			t.Errorf("%s hook.type = %q, want command", event, h.Type)
		}
		if h.Command != cmd {
			t.Errorf("%s hook.command = %q, want %q", event, h.Command, cmd)
		}
		if h.Timeout != hookTimeoutSeconds {
			t.Errorf("%s hook.timeout = %d, want %d", event, h.Timeout, hookTimeoutSeconds)
		}
	}

	// PreToolUse is installed ONLY for the Agent tool (the subagent-start signal),
	// not "*" — else every tool call would spawn an extra hook process.
	pre, ok := parsed.Hooks[eventPreToolUse]
	if !ok || len(pre) != 1 {
		t.Fatalf("PreToolUse matchers = %v, want exactly one", pre)
	}
	if pre[0].Matcher != agentToolMatcher {
		t.Errorf("PreToolUse matcher = %q, want %q", pre[0].Matcher, agentToolMatcher)
	}
}

// TestBuildHookCommand_Format proves the generated command string is a
// valid shell command with the binary path quoted and the install ID
// appended via the --install flag. CC passes this to bash verbatim.
func TestBuildHookCommand_Format(t *testing.T) {
	got := buildHookCommand("/bin/foci-cc-hook", "abc123")
	want := `"/bin/foci-cc-hook" --install abc123`
	if got != want {
		t.Errorf("buildHookCommand = %q, want %q", got, want)
	}
}

// TestBuildHookCommand_QuotesPathWithSpaces proves paths containing
// spaces survive by being wrapped in double quotes — `%q` produces
// a Go-escaped string which is also valid bash with respect to spaces.
func TestBuildHookCommand_QuotesPathWithSpaces(t *testing.T) {
	got := buildHookCommand("/home/user name/bin/foci-cc-hook", "id-1")
	if !strings.Contains(got, `"/home/user name/bin/foci-cc-hook"`) {
		t.Errorf("expected quoted path with spaces, got %q", got)
	}
}

// TestNewInstallID_Unique proves independent calls produce distinct IDs.
// A collision would mean two hook entries with the same ID can't be
// distinguished by handleHookResponse's install-ID filter.
func TestNewInstallID_Unique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newInstallID()
		if id == "" {
			t.Fatal("empty install ID")
		}
		if seen[id] {
			t.Fatalf("duplicate install ID after %d iterations: %s", i, id)
		}
		seen[id] = true
	}
}

// ---------------------------------------------------------------------------
// Hook binary resolution
// ---------------------------------------------------------------------------

// TestIsExecutableFile proves the helper distinguishes regular executables
// from directories and non-executable files — all three cases are live
// because a bad sibling should fall through to the $PATH fallback rather
// than erroring out.
func TestIsExecutableFile(t *testing.T) {
	dir := t.TempDir()

	exe := filepath.Join(dir, "exe")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isExecutableFile(exe) {
		t.Error("executable file not recognised")
	}

	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isExecutableFile(plain) {
		t.Error("plain file classified as executable")
	}

	if isExecutableFile(dir) {
		t.Error("directory classified as executable")
	}

	if isExecutableFile(filepath.Join(dir, "missing")) {
		t.Error("missing path classified as executable")
	}
}

// TestResolveHookBinary_SiblingFound proves the primary lookup resolves
// the binary shipped alongside the running process. Uses the real
// bin/foci-cc-hook built by `make foci-cc-hook` — tests run in the build
// tree so the sibling path exists.
func TestResolveHookBinary_SiblingFound(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	sibling := filepath.Join(filepath.Dir(self), hookCommandName)
	if !isExecutableFile(sibling) {
		t.Skipf("no foci-cc-hook sibling at %s (run `make foci-cc-hook` before `make test` to enable)", sibling)
	}

	t.Setenv("PATH", "")

	got, err := resolveHookBinary()
	if err != nil {
		t.Fatalf("resolveHookBinary: %v", err)
	}
	if got != sibling {
		t.Errorf("got %q, want sibling %q", got, sibling)
	}
}

// TestResolveHookBinary_PathFallback proves that when the sibling lookup
// misses, $PATH is searched for foci-cc-hook. This is the packaging case
// (foci-gw in /usr/local/bin, foci-cc-hook in /opt/foci/libexec but
// reachable via PATH).
func TestResolveHookBinary_PathFallback(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, hookCommandName)
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	got, err := resolveHookBinary()
	if err != nil {
		t.Fatalf("resolveHookBinary: %v", err)
	}
	if got != fake {
		t.Errorf("got %q, want %q", got, fake)
	}
}

// TestResolveHookBinary_NotFound proves the error path fires when neither
// lookup strategy finds an executable foci-cc-hook. prepareHooks logs at
// Warn in this case and skips hook install gracefully.
func TestResolveHookBinary_NotFound(t *testing.T) {
	t.Setenv("PATH", "")

	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), hookCommandName)
		if isExecutableFile(sibling) {
			t.Skipf("sibling %s exists; can't test NotFound path here", sibling)
		}
	}

	_, err := resolveHookBinary()
	if err == nil {
		t.Fatal("resolveHookBinary succeeded, want error")
	}
	if msg := err.Error(); !strings.Contains(msg, hookCommandName) {
		t.Errorf("error message = %q, want to mention %q", msg, hookCommandName)
	}
}

// ---------------------------------------------------------------------------
// handleHookResponse dispatch
// ---------------------------------------------------------------------------

// TestHandleHookResponse_PostToolUse proves a well-formed hook_response
// envelope for a PostToolUse event dispatches to OnToolEnd with
// is_error=false. The backend's install ID matches the payload so the
// install-ID filter lets the event through.
func TestHandleHookResponse_PostToolUse(t *testing.T) {
	b := &Backend{hookInstallID: "install-a"}

	type captured struct {
		id, name, output string
		isErr            bool
	}
	var got []captured
	handler := &testHandler{
		OnToolEnd: func(id, name, output string, isError bool) {
			got = append(got, captured{id, name, output, isError})
		},
	}
	applyHandler(b, handler)

	stdout, _ := json.Marshal(hookScriptOutput{
		HookEvent:    "PostToolUse",
		InstallID:    "install-a",
		ToolUseID:    "toolu_1",
		ToolName:     "Read",
		ToolResponse: "file contents",
		IsError:      false,
	})
	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUse",
		Stdout:    string(stdout),
		ExitCode:  0,
		Outcome:   "success",
	})
	b.handleHookResponse(env)

	if len(got) != 1 {
		t.Fatalf("OnToolEnd calls = %d, want 1", len(got))
	}
	c := got[0]
	if c.id != "toolu_1" || c.name != "Read" || c.output != "file contents" || c.isErr {
		t.Errorf("captured = %+v", c)
	}
}

// TestHandleHookResponse_AgentToolNoLongerFiresSubagentEnd proves the Agent
// tool's PostToolUse does NOT fire OnSubagentEnd: a background Agent tool_use
// resolves at launch, so ending there marks the chit complete while the run
// continues. The real end is task_notification:completed (see below).
func TestHandleHookResponse_AgentToolNoLongerFiresSubagentEnd(t *testing.T) {
	b := &Backend{hookInstallID: "install-a"}
	var ended []string
	handler := &testHandler{
		OnToolEnd:     func(id, name, output string, isError bool) {},
		OnSubagentEnd: func(groupKey string, runIndex int) { ended = append(ended, groupKey) },
	}
	applyHandler(b, handler)

	fire := func(toolName, toolUseID string) {
		stdout, _ := json.Marshal(hookScriptOutput{
			HookEvent: "PostToolUse", InstallID: "install-a",
			ToolUseID: toolUseID, ToolName: toolName,
		})
		env, _ := json.Marshal(hookResponseEnvelope{HookEvent: "PostToolUse", Stdout: string(stdout)})
		b.handleHookResponse(env)
	}
	fire("Read", "toolu_read")
	fire("Agent", "toolu_agent")

	if len(ended) != 0 {
		t.Fatalf("OnSubagentEnd = %v, want none (end moved to task_notification)", ended)
	}
}

// TestOnSystem_TaskNotificationCompleted_FiresSubagentEnd proves the subagent's
// true end — task_notification:completed — fires OnSubagentEnd keyed by the
// carried tool_use id (the group key), for both foreground and background runs.
func TestOnSystem_TaskNotificationCompleted_FiresSubagentEnd(t *testing.T) {
	b := &Backend{}
	var ended []string
	applyHandler(b, &testHandler{
		OnSubagentEnd: func(groupKey string, runIndex int) { ended = append(ended, groupKey) },
	})

	raw, _ := json.Marshal(TaskEvent{
		Subtype: "task_notification", Status: "completed", ToolUseID: "toolu_agent",
	})
	b.OnSystem("task_notification", raw)

	if len(ended) != 1 || ended[0] != "toolu_agent" {
		t.Fatalf("OnSubagentEnd = %v, want [toolu_agent]", ended)
	}
}

// TestHandleHookResponse_AgentPreToolUseFiresSubagentStart proves the Agent tool's
// PreToolUse fires a precise subagent START (groupKey + description) and does NOT
// fire OnToolEnd; a non-Agent PreToolUse fires nothing.
func TestHandleHookResponse_AgentPreToolUseFiresSubagentStart(t *testing.T) {
	b := &Backend{hookInstallID: "install-a"}
	type start struct{ groupKey, label string }
	var started []start
	toolEnds := 0
	applyHandler(b, &testHandler{
		OnToolEnd:       func(id, name, output string, isError bool) { toolEnds++ },
		OnSubagentStart: func(groupKey, label, prompt string, runIndex int) { started = append(started, start{groupKey, label}) },
	})

	fire := func(toolName, toolUseID, toolInput string) {
		stdout, _ := json.Marshal(hookScriptOutput{
			HookEvent: "PreToolUse", InstallID: "install-a",
			ToolUseID: toolUseID, ToolName: toolName, ToolInput: toolInput,
		})
		env, _ := json.Marshal(hookResponseEnvelope{HookEvent: "PreToolUse", Stdout: string(stdout)})
		b.handleHookResponse(env)
	}
	fire("Read", "toolu_read", "")                                 // not Agent → nothing
	fire("Agent", "toolu_agent", `{"description":"Search for X"}`) // Agent → start w/ label

	if len(started) != 1 || started[0] != (start{"toolu_agent", "Search for X"}) {
		t.Fatalf("OnSubagentStart = %+v, want [{toolu_agent Search for X}]", started)
	}
	if toolEnds != 0 {
		t.Errorf("PreToolUse fired OnToolEnd %d times, want 0", toolEnds)
	}
}

// TestHandleHookResponse_PostToolUseFailure proves failure envelopes carry
// the error message (not tool_response) into OnToolEnd with is_error=true.
func TestHandleHookResponse_PostToolUseFailure(t *testing.T) {
	b := &Backend{hookInstallID: "install-b"}
	var captured struct {
		id, name, output string
		isErr            bool
	}
	handler := &testHandler{
		OnToolEnd: func(id, name, output string, isError bool) {
			captured.id = id
			captured.name = name
			captured.output = output
			captured.isErr = isError
		},
	}
	applyHandler(b, handler)

	stdout, _ := json.Marshal(hookScriptOutput{
		HookEvent: "PostToolUseFailure",
		InstallID: "install-b",
		ToolUseID: "toolu_2",
		ToolName:  "Write",
		Error:     "Permission denied",
		IsError:   true,
	})
	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUseFailure",
		Stdout:    string(stdout),
	})
	b.handleHookResponse(env)

	if !captured.isErr {
		t.Error("isError = false, want true for PostToolUseFailure")
	}
	if captured.output != "Permission denied" {
		t.Errorf("output = %q, want Permission denied", captured.output)
	}
}

// TestHandleHookResponse_FiltersForeignInstallID proves the multi-source
// filter drops events whose install_id doesn't match this backend's — if
// the user has their own PostToolUse hook configured in settings.json,
// foci sees its hook_response events via the stream but skips them.
func TestHandleHookResponse_FiltersForeignInstallID(t *testing.T) {
	b := &Backend{hookInstallID: "install-us"}
	fired := false
	handler := &testHandler{
		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
	}
	applyHandler(b, handler)

	stdout, _ := json.Marshal(hookScriptOutput{
		HookEvent:    "PostToolUse",
		InstallID:    "install-someone-else",
		ToolUseID:    "toolu_x",
		ToolName:     "Read",
		ToolResponse: "not ours",
	})
	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUse",
		Stdout:    string(stdout),
	})
	b.handleHookResponse(env)

	if fired {
		t.Error("OnToolEnd fired for foreign install_id")
	}
}

// TestHandleHookResponse_FiltersUserHookNoID proves events from user-
// configured PostToolUse hooks (which don't pass through foci-cc-hook and
// therefore have empty install_id) are dropped when the backend has its
// own install ID set.
func TestHandleHookResponse_FiltersUserHookNoID(t *testing.T) {
	b := &Backend{hookInstallID: "install-us"}
	fired := false
	handler := &testHandler{
		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
	}
	applyHandler(b, handler)

	// Payload without install_id — the user's hook script doesn't echo one.
	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUse",
		Stdout:    `{"tool_use_id":"toolu_user","tool_name":"Bash"}`,
	})
	b.handleHookResponse(env)

	if fired {
		t.Error("OnToolEnd fired for payload with no install_id")
	}
}

// TestHandleHookResponse_DropsEventsWhenHooksDisabled proves that when
// prepareHooks failed (hookInstallID stays empty), incoming hook_response
// events from user-configured hooks that also have empty install_id are
// dropped — we can't tell them apart from ours because we never installed
// one, but since we never installed one, nothing should match. Requires
// exact install ID equality rather than the looser "matches if both set"
// rule that would have let user events through.
func TestHandleHookResponse_DropsEventsWhenHooksDisabled(t *testing.T) {
	b := &Backend{} // hookInstallID intentionally empty — install failed
	fired := false
	handler := &testHandler{
		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
	}
	applyHandler(b, handler)

	// A user-configured hook fires with no install_id (it wasn't foci's).
	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUse",
		Stdout:    `{"tool_use_id":"toolu_user","tool_name":"Bash","tool_response":"ok"}`,
	})
	b.handleHookResponse(env)

	if fired {
		t.Error("OnToolEnd fired for user hook event when foci hooks were not installed")
	}
}

// TestHandleHookResponse_SkipsSubagent proves hook events with a non-empty
// agent_id are dropped before dispatch — they belong to the sub-agent's
// transcript, not the parent turn.
func TestHandleHookResponse_SkipsSubagent(t *testing.T) {
	b := &Backend{hookInstallID: "install-us"}
	fired := false
	handler := &testHandler{
		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
	}
	applyHandler(b, handler)

	stdout, _ := json.Marshal(hookScriptOutput{
		HookEvent:    "PostToolUse",
		InstallID:    "install-us",
		ToolUseID:    "toolu_sub",
		ToolName:     "Read",
		ToolResponse: "nested result",
		AgentID:      "agent-child-7",
	})
	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUse",
		Stdout:    string(stdout),
	})
	b.handleHookResponse(env)

	if fired {
		t.Error("OnToolEnd fired for sub-agent hook event")
	}
}

// TestHandleHookResponse_SkipsUnknownHookEvent proves events that aren't
// PostToolUse or PostToolUseFailure are silently ignored — user-configured
// PreToolUse hooks or other lifecycle events shouldn't fire OnToolEnd.
func TestHandleHookResponse_SkipsUnknownHookEvent(t *testing.T) {
	b := &Backend{hookInstallID: "install-us"}
	fired := false
	handler := &testHandler{
		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
	}
	applyHandler(b, handler)

	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PreToolUse",
		Stdout:    `{"tool_use_id":"x","tool_name":"y","install_id":"install-us"}`,
	})
	b.handleHookResponse(env)

	if fired {
		t.Error("OnToolEnd fired for PreToolUse event")
	}
}

// TestHandleHookResponse_MalformedStdoutGracefulSkip proves malformed JSON
// in the hook script's stdout doesn't crash — we log at debug and drop the
// event, keeping the rest of the turn flowing.
func TestHandleHookResponse_MalformedStdoutGracefulSkip(t *testing.T) {
	b := &Backend{hookInstallID: "install-us"}
	fired := false
	handler := &testHandler{
		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
	}
	applyHandler(b, handler)

	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUse",
		Stdout:    "not valid json {{{",
	})
	b.handleHookResponse(env)

	if fired {
		t.Error("OnToolEnd fired despite malformed stdout")
	}
}

// TestHandleHookResponse_EmptyStdoutSilent proves hook_response messages
// with empty stdout (possible when the helper binary fails silently) are
// ignored rather than triggering a spurious OnToolEnd dispatch.
func TestHandleHookResponse_EmptyStdoutSilent(t *testing.T) {
	b := &Backend{hookInstallID: "install-us"}
	fired := false
	handler := &testHandler{
		OnToolEnd: func(_, _, _ string, _ bool) { fired = true },
	}
	applyHandler(b, handler)

	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUse",
		Stdout:    "",
	})
	b.handleHookResponse(env)

	if fired {
		t.Error("OnToolEnd fired for empty stdout")
	}
}

// TestHandleHookResponse_PostToolNudgeDispatched proves that when the
// handler's PostToolNudgeFunc returns a nudge reminder, handleHookResponse
// sends it to CC as a plain `[user]` user message via the writer and arms
// the rearm cascade so the response reaches the original handler. Matches
// the API transport's CheckAfterTools injection into the tool_result batch.
func TestHandleHookResponse_PostToolNudgeDispatched(t *testing.T) {
	var buf bytes.Buffer
	b := &Backend{
		hookInstallID: "install-us",
		writer:        NewWriter(nopWriteCloser{&buf}),
	}

	handler := &testHandler{
		OnToolEnd: func(_, _, _ string, _ bool) {},
		PostToolNudgeFunc: func(name, _ string, isErr bool) []string {
			if name == "Bash" && !isErr {
				return []string{"reminder-text"}
			}
			return nil
		},
	}
	applyHandler(b, handler)

	stdout, _ := json.Marshal(hookScriptOutput{
		HookEvent:    "PostToolUse",
		InstallID:    "install-us",
		ToolUseID:    "toolu_1",
		ToolName:     "Bash",
		ToolResponse: "ok",
	})
	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUse",
		Stdout:    string(stdout),
	})
	b.handleHookResponse(env)

	if !strings.Contains(buf.String(), "[user] reminder-text") {
		t.Errorf("expected [user] reminder-text in writer output, got: %q", buf.String())
	}
	if strings.Contains(buf.String(), `"priority"`) {
		t.Errorf("priority field should be absent on default-priority post-tool nudge, got: %q", buf.String())
	}
}

// TestHandleHookResponse_PostToolNudgeNilFunc proves handleHookResponse is a
// no-op on the nudge path when PostToolNudgeFunc is nil — agents without a
// Nudger keep working without spurious writer traffic.
func TestHandleHookResponse_PostToolNudgeNilFunc(t *testing.T) {
	var buf bytes.Buffer
	b := &Backend{
		hookInstallID: "install-us",
		writer:        NewWriter(nopWriteCloser{&buf}),
	}
	handler := &testHandler{
		OnToolEnd: func(_, _, _ string, _ bool) {},
	}
	applyHandler(b, handler)

	stdout, _ := json.Marshal(hookScriptOutput{
		HookEvent:    "PostToolUse",
		InstallID:    "install-us",
		ToolUseID:    "toolu_1",
		ToolName:     "Read",
		ToolResponse: "ok",
	})
	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUse",
		Stdout:    string(stdout),
	})
	b.handleHookResponse(env)

	if buf.Len() != 0 {
		t.Errorf("writer should be empty with nil PostToolNudgeFunc, got: %q", buf.String())
	}
}

// TestHandleHookResponse_PostToolNudgeSkipsEmpty proves that empty reminder
// strings returned by PostToolNudgeFunc are skipped rather than emitted as
// a blank `[user] ` message — matches the SteerCheckFunc drain path.
func TestHandleHookResponse_PostToolNudgeSkipsEmpty(t *testing.T) {
	var buf bytes.Buffer
	b := &Backend{
		hookInstallID: "install-us",
		writer:        NewWriter(nopWriteCloser{&buf}),
	}
	handler := &testHandler{
		OnToolEnd: func(_, _, _ string, _ bool) {},
		PostToolNudgeFunc: func(_, _ string, _ bool) []string {
			return []string{"", "real", ""}
		},
	}
	applyHandler(b, handler)

	stdout, _ := json.Marshal(hookScriptOutput{
		HookEvent:    "PostToolUse",
		InstallID:    "install-us",
		ToolUseID:    "toolu_1",
		ToolName:     "Read",
		ToolResponse: "ok",
	})
	env, _ := json.Marshal(hookResponseEnvelope{
		HookEvent: "PostToolUse",
		Stdout:    string(stdout),
	})
	b.handleHookResponse(env)

	lines := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1
	if !strings.Contains(buf.String(), "[user] real") {
		t.Errorf("expected [user] real in output, got: %q", buf.String())
	}
	if lines != 1 {
		t.Errorf("expected exactly 1 writer line (empty nudges skipped), got %d: %q", lines, buf.String())
	}
}
