package ccstream

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator/pretool"
	"foci/internal/delegator/stoprule"
	"foci/internal/log"
)

// ---------------------------------------------------------------------------
// Hook settings JSON build
// ---------------------------------------------------------------------------

// TestBuildHookSettingsJSON proves the generated JSON has the shape CC
// expects (top-level hooks.PostToolUse, hooks.PostToolUseFailure and
// hooks.PermissionDenied each with a single matcher:"*" entry carrying the
// foci hook command). CC
// loads this via --settings <json> as a flagSettings source.
func TestBuildHookSettingsJSON(t *testing.T) {
	cmd := buildHookCommand("/bin/foci-cc-hook", "abc123")
	body, err := buildHookSettingsJSON(cmd, nil, nil)
	if err != nil {
		t.Fatalf("buildHookSettingsJSON: %v", err)
	}

	var parsed struct {
		Hooks map[string][]hookMatcher `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("parse generated settings: %v (body: %s)", err, body)
	}

	for _, event := range []string{eventPostToolUse, eventPostToolUseFailure, eventPermissionDenied} {
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
	if got := pre[0].Hooks[0].Command; got != cmd {
		t.Errorf("PreToolUse command with no rules = %q, want %q", got, cmd)
	}
}

// TestBuildHookSettingsJSON_PreToolRules proves rules widen ONLY the PreToolUse
// matcher (Agent + each ruled tool, exact names) and ride ONLY its command
// line, decodable back to the same rules; the Post hooks are unchanged.
func TestBuildHookSettingsJSON_PreToolRules(t *testing.T) {
	cmd := buildHookCommand("/bin/foci-cc-hook", "abc123")
	rules := []pretool.Rule{
		{Name: "c", Tool: "CronCreate", Action: "deny", Reason: "r1"},
		{Name: "a", Tool: "AskUserQuestion", Action: "deny", Reason: "r2"},
		{Name: "g", Tool: "Agent", Action: "deny", Reason: "r3", Input: map[string]pretool.Patterns{"prompt": {"x"}}},
	}
	body, err := buildHookSettingsJSON(cmd, rules, nil)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Hooks map[string][]hookMatcher `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	pre := parsed.Hooks[eventPreToolUse]
	if len(pre) != 1 {
		t.Fatalf("PreToolUse matchers = %d, want 1", len(pre))
	}
	if pre[0].Matcher != "Agent|AskUserQuestion|CronCreate" {
		t.Errorf("PreToolUse matcher = %q", pre[0].Matcher)
	}
	preCmd := pre[0].Hooks[0].Command
	prefix := cmd + " " + rulesFlag + " "
	if !strings.HasPrefix(preCmd, prefix) {
		t.Fatalf("PreToolUse command = %q, want prefix %q", preCmd, prefix)
	}
	got, err := pretool.Decode(strings.TrimPrefix(preCmd, prefix))
	if err != nil || len(got) != len(rules) || got[0].Reason != "r1" {
		t.Errorf("decoded rules = %+v err=%v", got, err)
	}
	for _, ev := range []string{eventPostToolUse, eventPostToolUseFailure} {
		if c := parsed.Hooks[ev][0].Hooks[0].Command; c != cmd {
			t.Errorf("%s command = %q, want %q (no rules)", ev, c, cmd)
		}
	}
}

// TestBuildHookSettingsJSON_StopRules proves stop rules add a matcher-less
// Stop entry whose command carries them, and that no rules means no Stop hook.
func TestBuildHookSettingsJSON_StopRules(t *testing.T) {
	cmd := buildHookCommand("/bin/foci-cc-hook", "abc123")
	parse := func(stop []stoprule.Rule) map[string][]hookMatcher {
		body, err := buildHookSettingsJSON(cmd, nil, stop)
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Hooks map[string][]hookMatcher `json:"hooks"`
		}
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			t.Fatal(err)
		}
		return parsed.Hooks
	}
	if _, ok := parse(nil)[eventStop]; ok {
		t.Error("Stop hook installed with no stop rules")
	}
	stop := parse([]stoprule.Rule{{Name: "s", Text: []string{"starting"}, Reason: "r"}})[eventStop]
	if len(stop) != 1 || stop[0].Matcher != "" || len(stop[0].Hooks) != 1 {
		t.Fatalf("Stop entries = %+v, want one matcher-less entry", stop)
	}
	prefix := cmd + " " + stopRulesFlag + " "
	c := stop[0].Hooks[0].Command
	if !strings.HasPrefix(c, prefix) {
		t.Fatalf("Stop command = %q, want prefix %q", c, prefix)
	}
	got, err := stoprule.Decode(strings.TrimPrefix(c, prefix))
	if err != nil || len(got) != 1 || got[0].Name != "s" {
		t.Errorf("decoded stop rules = %+v err=%v", got, err)
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

// TestHandleHookResponse_RefusedAgentEndsItsGroup is #2104: CC can refuse an
// Agent call after its PreToolUse has already opened the group (observed:
// "Concurrent subagent limit reached"). The refusal arrives as an errored Agent
// tool result, and no task_started or task_notification ever follows, so that
// result is the group's only end. Without it the chit and the tracker entry
// stay "running" for good.
//
// An errored result for an Agent whose task DID start is not an end: that task
// ends on its own task_notification, and ending it here too would double the end.
func TestHandleHookResponse_RefusedAgentEndsItsGroup(t *testing.T) {
	type end struct {
		group string
		run   int
	}
	setup := func() (*Backend, *[]end) {
		b := &Backend{hookInstallID: "install-a"}
		var ends []end
		applyHandler(b, &testHandler{
			OnToolEnd:       func(string, string, string, bool) {},
			OnSubagentStart: func(string, string, string, int) {},
			OnSubagentEnd:   func(g string, run int) { ends = append(ends, end{g, run}) },
		})
		return b, &ends
	}
	agentFailure := func(b *Backend, toolUseID string) {
		stdout, _ := json.Marshal(hookScriptOutput{
			HookEvent: "PostToolUseFailure", InstallID: "install-a",
			ToolUseID: toolUseID, ToolName: "Agent", IsError: true,
			Error: "Concurrent subagent limit reached. You can run 20 subagents at once.",
		})
		env, _ := json.Marshal(hookResponseEnvelope{HookEvent: "PostToolUseFailure", Stdout: string(stdout)})
		b.handleHookResponse(env)
	}

	t.Run("refused before any task started", func(t *testing.T) {
		b, ends := setup()
		b.agents.Add("toolu_refused", "todo #1875")
		fireAgentPreToolUse(b, "toolu_refused", "install-a", `{"description":"todo #1875","prompt":"p"}`)
		agentFailure(b, "toolu_refused")

		if want := []end{{"toolu_refused", 1}}; !reflect.DeepEqual(*ends, want) {
			t.Errorf("OnSubagentEnd = %v, want %v", *ends, want)
		}
		if b.agents.Remove("toolu_refused") {
			t.Error("refused Agent still tracked as a running subagent")
		}
	})

	t.Run("errored after its task started", func(t *testing.T) {
		b, ends := setup()
		b.setAgentLabel("toolu_ran", "d")
		b.agents.Add("toolu_ran", "d")
		fireAgentPreToolUse(b, "toolu_ran", "install-a", `{"description":"d","prompt":"p"}`)
		started, _ := json.Marshal(TaskEvent{Subtype: "task_started", ToolUseID: "toolu_ran", TaskID: "task-ran"})
		b.OnSystem("task_started", started)
		agentFailure(b, "toolu_ran")

		if len(*ends) != 0 {
			t.Fatalf("errored result of a started task fired OnSubagentEnd %v, want none before its task_notification", *ends)
		}
		done, _ := json.Marshal(TaskEvent{Subtype: "task_notification", Status: "failed", ToolUseID: "toolu_ran", TaskID: "task-ran"})
		b.OnSystem("task_notification", done)
		if want := []end{{"toolu_ran", 1}}; !reflect.DeepEqual(*ends, want) {
			t.Errorf("OnSubagentEnd = %v, want %v", *ends, want)
		}
	})
}

// TestHandleHookResponse_ClassifierDenyEndsCall is #2184: CC's auto-mode
// classifier refuses a call AFTER its PreToolUse has fired, so a denied Agent
// already has its group open. CC fires no PostToolUse or PostToolUseFailure for
// a denied call (the deny is written straight into the tool_result), only a
// PermissionDenied hook. That hook is the call's end: the tool display
// resolves, the pending-work entry goes, and the Agent's group ends. A
// background Bash has the same gap minus the group. A subagent's own denied
// call touches nothing on the parent turn.
func TestHandleHookResponse_ClassifierDenyEndsCall(t *testing.T) {
	type toolEnd struct {
		id, name, output string
		isError          bool
	}
	type end struct {
		group string
		run   int
	}
	b := &Backend{hookInstallID: "install-a"}
	var toolEnds []toolEnd
	var ends []end
	applyHandler(b, &testHandler{
		OnToolEnd: func(id, name, output string, isError bool) {
			toolEnds = append(toolEnds, toolEnd{id, name, output, isError})
		},
		OnSubagentStart: func(string, string, string, int) {},
		OnSubagentEnd:   func(g string, run int) { ends = append(ends, end{g, run}) },
	})
	denied := func(toolUseID, tool, agentID string) {
		stdout, _ := json.Marshal(hookScriptOutput{
			HookEvent: "PermissionDenied", InstallID: "install-a",
			ToolUseID: toolUseID, ToolName: tool, AgentID: agentID, IsError: true,
			Error: "[Code from External]",
		})
		env, _ := json.Marshal(hookResponseEnvelope{HookEvent: "PermissionDenied", Stdout: string(stdout)})
		b.handleHookResponse(env)
	}

	b.agents.Add("toolu_agent", "probe")
	fireAgentPreToolUse(b, "toolu_agent", "install-a", `{"description":"probe","prompt":"p"}`)
	b.agents.Add("toolu_bash", "background command")
	b.agents.Add("toolu_sub", "background command")

	denied("toolu_agent", "Agent", "")
	denied("toolu_bash", "Bash", "")
	denied("toolu_sub", "Bash", "task-1")

	wantTools := []toolEnd{
		{"toolu_agent", "Agent", "[Code from External]", true},
		{"toolu_bash", "Bash", "[Code from External]", true},
	}
	if !reflect.DeepEqual(toolEnds, wantTools) {
		t.Errorf("OnToolEnd = %+v, want %+v", toolEnds, wantTools)
	}
	if want := []end{{"toolu_agent", 1}}; !reflect.DeepEqual(ends, want) {
		t.Errorf("OnSubagentEnd = %v, want %v", ends, want)
	}
	if b.agents.Remove("toolu_agent") {
		t.Error("denied Agent still tracked as a running subagent")
	}
	if b.agents.Remove("toolu_bash") {
		t.Error("denied background Bash still tracked as pending work")
	}
	if !b.agents.Remove("toolu_sub") {
		t.Error("a subagent's denied call released a parent-turn tracker entry")
	}
}

// TestOnSystem_TaskNotificationCompleted_FiresSubagentEnd proves the subagent's
// true end — task_notification:completed — fires OnSubagentEnd keyed by the
// carried tool_use id (the group key), for both foreground and background runs.
// The run's start went out first (PreToolUse): an end is sent only for a group
// the app was given (#2010).
func TestOnSystem_TaskNotificationCompleted_FiresSubagentEnd(t *testing.T) {
	b := &Backend{hookInstallID: "install-a"}
	var ended []string
	applyHandler(b, &testHandler{
		OnSubagentStart: func(string, string, string, int) {},
		OnSubagentEnd:   func(groupKey string, runIndex int) { ended = append(ended, groupKey) },
	})
	fireAgentPreToolUse(b, "toolu_agent", "install-a", `{"description":"d","prompt":"p"}`)

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

// TestHandleHookResponse_PreToolRuleDeny proves a denied call's hook_response
// is its end signal (CC sends no Post hook for it): a main-thread deny fires
// OnToolEnd with the reason as an error and releases the pending-work entry;
// a denied Agent never fires a subagent start; a subagent's deny fires nothing
// on the parent turn.
func TestHandleHookResponse_PreToolRuleDeny(t *testing.T) {
	b := &Backend{hookInstallID: "install-a"}
	type end struct {
		id, name, output string
		isError          bool
	}
	var ends []end
	starts := 0
	applyHandler(b, &testHandler{
		OnToolEnd:       func(id, name, output string, isError bool) { ends = append(ends, end{id, name, output, isError}) },
		OnSubagentStart: func(groupKey, label, prompt string, runIndex int) { starts++ },
	})
	b.agents.Add("toolu_Agent", "spawn")
	for _, c := range []struct{ tool, agentID string }{{"Agent", ""}, {"AskUserQuestion", ""}, {"CronCreate", "sub-1"}} {
		out := hookScriptOutput{
			HookEvent: "PreToolUse", InstallID: "install-a", ToolUseID: "toolu_" + c.tool,
			ToolName: c.tool, AgentID: c.agentID, DeniedRule: "r",
		}
		out.HookSpecificOutput.PermissionDecisionReason = "because " + c.tool
		stdout, _ := json.Marshal(out)
		env, _ := json.Marshal(hookResponseEnvelope{HookEvent: "PreToolUse", Stdout: string(stdout)})
		b.handleHookResponse(env)
	}
	want := []end{
		{"toolu_Agent", "Agent", "because Agent", true},
		{"toolu_AskUserQuestion", "AskUserQuestion", "because AskUserQuestion", true},
	}
	if !reflect.DeepEqual(ends, want) {
		t.Errorf("OnToolEnd = %+v, want %+v", ends, want)
	}
	if starts != 0 {
		t.Errorf("denied Agent fired %d subagent starts", starts)
	}
	if b.agents.Remove("toolu_Agent") {
		t.Error("denied Agent still tracked as pending work")
	}
}

// TestHandleHookResponse_WhenErrorLogged proves a when-check that failed open
// in the hook reaches foci's log at WARN (#2034), for a main-thread call and
// a subagent's alike, without ending the call.
//
// Not parallel: log.SetWarnHook is process-global.
func TestHandleHookResponse_WhenErrorLogged(t *testing.T) {
	var mu sync.Mutex
	var got []string
	log.SetWarnHook(func(level log.Level, component, msg string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, level.String()+" "+msg)
	})
	t.Cleanup(func() { log.SetWarnHook(nil) })

	b := &Backend{hookInstallID: "install-w"}
	ends := 0
	applyHandler(b, &testHandler{OnToolEnd: func(id, name, output string, isError bool) { ends++ }})
	for _, agentID := range []string{"", "sub-1"} {
		out := hookScriptOutput{
			HookEvent: "PreToolUse", InstallID: "install-w", ToolUseID: "toolu_w" + agentID,
			ToolName: "Bash", AgentID: agentID, WhenErrors: []string{"rule x: when: timed out (2s)"},
		}
		stdout, _ := json.Marshal(out)
		env, _ := json.Marshal(hookResponseEnvelope{HookEvent: "PreToolUse", Stdout: string(stdout)})
		b.handleHookResponse(env)
	}
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, e := range got {
		if strings.HasPrefix(e, "WARN") && strings.Contains(e, "pretool_when_error") && strings.Contains(e, "rule x: when: timed out") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("want 2 pretool_when_error warnings, got %q", got)
	}
	if ends != 0 {
		t.Errorf("a failed-open check ended the call (%d OnToolEnd)", ends)
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

// TestHandleHookResponse_StopVerdictLogged proves every Stop verdict from our
// own hook is logged as stop_rule_eval (fire and pass alike, #2089), and a
// Stop hook_response from another install is not.
//
// Not parallel: captureDebugLog is process-global.
func TestHandleHookResponse_StopVerdictLogged(t *testing.T) {
	buf := captureDebugLog(t)
	b := &Backend{hookInstallID: "install-s"}
	for _, stdout := range []string{
		`{"decision":"block","reason":"r","hook_event":"Stop","install_id":"install-s","stop_result":"fire","stop_rule":"announce","stop_excerpt":"I'm starting"}`,
		`{"hook_event":"Stop","install_id":"install-s","stop_result":"pass","stop_excerpt":"all done"}`,
		`{"hook_event":"Stop","install_id":"someone-else","stop_result":"fire","stop_rule":"theirs"}`,
	} {
		env, _ := json.Marshal(hookResponseEnvelope{HookEvent: "Stop", Stdout: stdout})
		b.handleHookResponse(env)
	}
	got := buf.String()
	for _, want := range []string{
		`stop_rule_eval result=fire rule=announce blocked=true launches=0 excerpt="I'm starting"`,
		`stop_rule_eval result=pass rule= blocked=false launches=0 excerpt="all done"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "theirs") {
		t.Errorf("logged another install's Stop verdict:\n%s", got)
	}
}

// TestHandleHookResponse_RuntimeShownLogged proves each runtime note
// foci-cc-hook gave the model (#2125) is logged as tool_runtime_shown, a
// subagent's included, so the note's effect can be measured; a fast call
// with no note logs nothing.
//
// Not parallel: captureDebugLog is process-global.
func TestHandleHookResponse_RuntimeShownLogged(t *testing.T) {
	buf := captureDebugLog(t)
	b := &Backend{hookInstallID: "install-r"}
	for _, out := range []hookScriptOutput{
		{HookEvent: "PostToolUse", ToolUseID: "toolu_slow", ToolName: "Bash", DurationMS: 42_000, RuntimeShown: true},
		{HookEvent: "PostToolUse", ToolUseID: "toolu_sub", ToolName: "Bash", AgentID: "sub-1", DurationMS: 15_000, RuntimeShown: true},
		{HookEvent: "PostToolUse", ToolUseID: "toolu_fast", ToolName: "Read", DurationMS: 30},
	} {
		out.InstallID = "install-r"
		stdout, _ := json.Marshal(out)
		env, _ := json.Marshal(hookResponseEnvelope{HookEvent: "PostToolUse", Stdout: string(stdout)})
		b.handleHookResponse(env)
	}
	got := buf.String()
	for _, want := range []string{
		"tool_runtime_shown tool=Bash tuid=toolu_slow agent_id= duration_ms=42000",
		"tool_runtime_shown tool=Bash tuid=toolu_sub agent_id=sub-1 duration_ms=15000",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "toolu_fast") {
		t.Errorf("logged a runtime note for a fast call:\n%s", got)
	}
}
