package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/memory"
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
		acfg:    config.AgentConfig{ID: agentID},
		cfg:     &config.Config{},
		connMgr: stubConnMgr{},
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
	// New ExecExport tools added to buildExecRegistry are automatically
	// covered: there is no hand-maintained list to update. This is the
	// structural fix for the foci_remind --text drift (TODO #723).
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

	// Sanity: every conditional tool we expect should be present. If
	// buildExecRegistry's wiring changes, update this list AND the test
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
