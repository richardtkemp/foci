package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/memory"
	"foci/internal/provider"
	"foci/internal/tools"
)

// stubWakeFn is a no-op tools.ScheduleWakeFn for tests that just need a
// non-nil callback to satisfy buildExecRegistry's reminder-registration
// guard. It records nothing — tests that need to observe scheduling should
// build their own closure.
func stubWakeFn(int64, time.Duration, string, string) error { return nil }

// minimalSetupParams returns a setupParams populated with just enough fields
// for buildExecRegistry to run without a nil-deref. Optional stores (todo,
// memory backends, secrets, bitwarden) are intentionally left nil so the
// returned registry contains only the unconditional core (send_to_chat,
// web_fetch, http_request) plus whatever the test enables.
func minimalSetupParams(t *testing.T, agentID string) setupParams {
	t.Helper()
	return setupParams{
		acfg:     config.AgentConfig{ID: agentID},
		cfg:      &config.Config{},
		resolved: &config.ResolvedAgentConfig{},
		connMgr:  stubConnMgr{},
	}
}

// TestBuildExecRegistryWiresAsyncNotifier guards the delegated setup against
// regressing the AsyncNotifier assignment. A nil AsyncNotifier on a delegated
// agent silently disables the /plan EnterPlanMode injection (command/plan.go)
// and the #845 compaction-resume nudge (compaction.go:199, on the delegated
// path). The bug it locks down: the delegated path built the notifier and
// wired it into tools but never stored it on the agent, unlike the API path.
func TestBuildExecRegistryWiresAsyncNotifier(t *testing.T) {
	t.Parallel()

	p := minimalSetupParams(t, "test")
	ag := &agent.Agent{}
	buildExecRegistry(p, stubWakeFn, func() *agent.Agent { return ag })

	if ag.AsyncNotifier == nil {
		t.Error("delegated agent's AsyncNotifier is nil; /plan injection and #845 compaction-resume need it set (mirror the API path)")
	}
}

func TestBuildExecRegistryRegistersRemind(t *testing.T) {
	// Delegated (Claude Code) agents pick up shell tools from the exec
	// registry. When reminderStore is configured AND a wake-schedule callback
	// is available, the remind tool must be registered so it surfaces as
	// foci_remind. Prior to this wiring, delegated agents had no path to set
	// reminders even though the backing store existed.
	t.Parallel()

	rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "reminders.db"))
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })

	p := minimalSetupParams(t, "test")
	p.reminderStore = rs

	registry := buildExecRegistry(p, stubWakeFn, nil)
	if got := registry.Get("remind"); got == nil {
		t.Fatal("registry missing remind tool when reminderStore + wakeFn are set")
	}

	exportedHas := func(names []string, target string) bool {
		for _, n := range names {
			if n == target {
				return true
			}
		}
		return false
	}
	if !exportedHas(registry.ExportedNames(), "foci_remind") {
		t.Errorf("ExportedNames() = %v, want to include foci_remind", registry.ExportedNames())
	}
}

func TestBuildExecRegistrySkipsRemindWhenStoreNil(t *testing.T) {
	// When the agent has no reminderStore (reminders disabled at the platform
	// level), the remind tool must not be registered even if a wakeFn is
	// passed — the tool would fail at execution time with no backing store.
	t.Parallel()

	p := minimalSetupParams(t, "test")
	// p.reminderStore intentionally nil

	registry := buildExecRegistry(p, stubWakeFn, nil)
	if got := registry.Get("remind"); got != nil {
		t.Error("registry contains remind tool despite nil reminderStore")
	}
}

func TestBuildExecRegistryAllToolsHaveShellFuncParity(t *testing.T) {
	// Walks the production wiring (buildExecRegistry) with all conditional
	// dependencies populated, then creates an exec bridge — which runs
	// validateShellFuncSchemaParity on every ExecExport tool. Any tool whose
	// schema gains a parameter without a matching flag arm in its generated
	// shell-func body fails this test.
	//
	// New ExecExport tools added to the unified tool table (tool_table.go,
	// pathExec rows) are automatically covered: there is no hand-maintained
	// list to update. This is the structural fix for the foci_remind --text
	// drift (TODO #723).
	t.Parallel()

	rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "reminders.db"))
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })

	ts, err := memory.NewTodoStore(filepath.Join(t.TempDir(), "todo.db"))
	if err != nil {
		t.Fatalf("NewTodoStore: %v", err)
	}
	t.Cleanup(func() { ts.Close() })

	p := minimalSetupParams(t, "test")
	p.reminderStore = rs
	p.todoStore = ts
	p.braveKey = "stub-key" // non-empty enables web_search registration
	p.resolved = &config.ResolvedAgentConfig{}
	// Populate memBackends with a stub so memory_search registers. The map
	// just needs len > 0; the searcher value is never invoked here.
	p.memBackends = map[string]memory.Searcher{"stub": nil}

	registry := buildExecRegistry(p, stubWakeFn, nil)

	// Sanity: every conditional tool we expect should be present. If the
	// tool table's pathExec set changes, update this list AND the test
	// will still cover all registered tools because the parity check runs
	// over registry.All().
	for _, want := range []string{"send_to_chat", "send_to_session", "web_fetch", "http_request", "todo", "web_search", "memory_search", "remind"} {
		if registry.Get(want) == nil {
			t.Errorf("expected tool %q to be registered with full deps", want)
		}
	}

	// Creating the bridge runs writeShellFuncs, which calls
	// validateShellFuncSchemaParity on every ExecExport tool. A drift
	// (schema param without a matching --flag case arm in the generated
	// body) returns an error from NewExecBridge.
	bridge, err := tools.NewExecBridge(registry, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v\n\nThis usually means a tool's schema gained a parameter without a matching --flag case arm in its generated shell-func body. Check generateShellFunc and generateGenericShellFunc.", err)
	}
	t.Cleanup(bridge.Close)
}

func TestBuildExecRegistryRegistersSendToSession(t *testing.T) {
	// Delegated agents must have send_to_session for cross-session messaging
	// (foci_send_to_session shell function). Prior to this wiring, only API
	// agents had it — delegated/CC agents could send_to_chat but not address
	// other sessions directly.
	t.Parallel()

	p := minimalSetupParams(t, "test")

	registry := buildExecRegistry(p, stubWakeFn, nil)
	if got := registry.Get("send_to_session"); got == nil {
		t.Fatal("registry missing send_to_session tool")
	}

	exportedHas := func(names []string, target string) bool {
		for _, n := range names {
			if n == target {
				return true
			}
		}
		return false
	}
	if !exportedHas(registry.ExportedNames(), "foci_send_to_session") {
		t.Errorf("ExportedNames() = %v, want to include foci_send_to_session", registry.ExportedNames())
	}
}

// TestBuildDelegatedSystemPrompt locks in the structural rule that delegated
// agents get the Available Skills block appended AFTER workspace identity
// files. Delegated transport (CC subprocess) takes a single SystemPrompt
// string at startup, unlike API agents which pass ExtraSystemBlocks as a
// separate provider block — so ordering and separators are owned here, not
// by the provider layer. If skills slip in front of workspace identity, the
// character files lose their priming position in CC's context.
func TestBuildDelegatedSystemPrompt(t *testing.T) {
	t.Parallel()

	workspace := []provider.SystemBlock{
		{Type: "text", Text: "CRAFT content"},
		{Type: "text", Text: "MEMORY content"},
	}
	extra := []provider.SystemBlock{
		{Type: "text", Text: "Available Skills: foo, bar"},
	}

	got := buildDelegatedSystemPrompt(workspace, extra)
	want := "CRAFT content\n\nMEMORY content\n\nAvailable Skills: foo, bar"
	if got != want {
		t.Errorf("buildDelegatedSystemPrompt() = %q, want %q", got, want)
	}

	// Skills must come AFTER all workspace blocks (lock the order — a
	// regression that swapped them would still concatenate cleanly).
	craftIdx := strings.Index(got, "CRAFT content")
	memoryIdx := strings.Index(got, "MEMORY content")
	skillsIdx := strings.Index(got, "Available Skills")
	if !(craftIdx < memoryIdx && memoryIdx < skillsIdx) {
		t.Errorf("expected workspace blocks before skills; got craft=%d memory=%d skills=%d", craftIdx, memoryIdx, skillsIdx)
	}
}

func TestBuildDelegatedSystemPrompt_EmptyInputs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		workspace []provider.SystemBlock
		extra     []provider.SystemBlock
		want      string
	}{
		{"both empty", nil, nil, ""},
		{"workspace only", []provider.SystemBlock{{Text: "only"}}, nil, "only"},
		{"extra only", nil, []provider.SystemBlock{{Text: "skills"}}, "skills"},
		{"workspace + empty extra slice", []provider.SystemBlock{{Text: "only"}}, []provider.SystemBlock{}, "only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildDelegatedSystemPrompt(tc.workspace, tc.extra)
			if got != tc.want {
				t.Errorf("buildDelegatedSystemPrompt() = %q, want %q", got, tc.want)
			}
			// No leading or trailing separator.
			if strings.HasPrefix(got, "\n") || strings.HasSuffix(got, "\n") {
				t.Errorf("unexpected leading/trailing newline in %q", got)
			}
		})
	}
}

func TestBuildExecRegistrySkipsRemindWhenWakeFnNil(t *testing.T) {
	// Without a wake-schedule callback, wake=true reminders cannot fire. The
	// passive remind path would still work, but registering the tool with
	// half its surface broken is worse than not registering at all — keep
	// the contract clean.
	t.Parallel()

	rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "reminders.db"))
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	t.Cleanup(func() { rs.Close() })

	p := minimalSetupParams(t, "test")
	p.reminderStore = rs

	var nilFn tools.ScheduleWakeFn
	registry := buildExecRegistry(p, nilFn, nil)
	if got := registry.Get("remind"); got != nil {
		t.Error("registry contains remind tool despite nil wakeFn")
	}
}

func TestBackendDefaultModel(t *testing.T) {
	if got := backendDefaultModel("claude-code"); got != "opus" {
		t.Errorf("claude-code default = %q, want opus", got)
	}
	if got := backendDefaultModel("claude-code-tmux"); got != "opus" {
		t.Errorf("claude-code-tmux default = %q, want opus", got)
	}
	if got := backendDefaultModel("opencode"); got != "" {
		t.Errorf("opencode default = %q, want empty (TODO #1163)", got)
	}
}
