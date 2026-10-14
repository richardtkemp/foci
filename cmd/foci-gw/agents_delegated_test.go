package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/delegator"
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
	resolved := &config.ResolvedAgentConfig{}
	return setupParams{
		acfg:         config.AgentConfig{ID: agentID},
		cfg:          &config.Config{},
		resolved:     resolved,
		resolvedLive: config.NewLiveValue(resolved),
		connMgr:      stubConnMgr{},
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
	buildExecRegistry(p, stubWakeFn, nil, func() *agent.Agent { return ag })

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

	registry := buildExecRegistry(p, stubWakeFn, nil, nil)
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

	registry := buildExecRegistry(p, stubWakeFn, nil, nil)
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

	registry := buildExecRegistry(p, stubWakeFn, nil, nil)

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

	registry := buildExecRegistry(p, stubWakeFn, nil, nil)
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
	registry := buildExecRegistry(p, nilFn, nil, nil)
	if got := registry.Get("remind"); got != nil {
		t.Error("registry contains remind tool despite nil wakeFn")
	}
}

// TestFoldGlobalBackendConfig_OpencodeDefaultPermission pins the #2330
// threat-scan fix: the global [opencode_backend] default_permission must
// actually reach every opencode agent's backend_config. It was documented
// as "applies to every agent" but never read — an operator setting a
// lockdown there silently got opencode's permissive defaults instead.
// Per-agent backend_config wins, an unset global stays unset (so Start
// sends no permission request — the field deliberately has no tag
// default), and other config families skip opencode keys entirely.
func TestFoldGlobalBackendConfig_OpencodeDefaultPermission(t *testing.T) {
	oc := delegator.Spec{ConfigFamily: delegator.ConfigFamilyOpencode}

	t.Run("global reaches backend_config", func(t *testing.T) {
		cfg := &config.Config{OpencodeBackend: config.OpencodeBackendConfig{DefaultPermission: "deny"}}
		bc := foldGlobalBackendConfig(cfg, oc, config.BackendConfig{})
		if bc.DefaultPermission == nil || *bc.DefaultPermission != "deny" {
			t.Errorf("DefaultPermission = %v, want the global \"deny\" folded in", bc.DefaultPermission)
		}
		if got := bc.ToMap()["default_permission"]; got != "deny" {
			t.Errorf("ToMap default_permission = %v, want \"deny\"", got)
		}
	})

	t.Run("per-agent value wins", func(t *testing.T) {
		agentVal := "ask"
		cfg := &config.Config{OpencodeBackend: config.OpencodeBackendConfig{DefaultPermission: "deny"}}
		bc := foldGlobalBackendConfig(cfg, oc, config.BackendConfig{DefaultPermission: &agentVal})
		if bc.DefaultPermission == nil || *bc.DefaultPermission != "ask" {
			t.Errorf("DefaultPermission = %v, want the per-agent \"ask\" to win", bc.DefaultPermission)
		}
	})

	t.Run("unset global stays unset", func(t *testing.T) {
		cfg := &config.Config{} // no tag default: DefaultPermission is ""
		bc := foldGlobalBackendConfig(cfg, oc, config.BackendConfig{})
		if bc.DefaultPermission != nil {
			t.Errorf("DefaultPermission = %q, want nil (Start must send no permission request)", *bc.DefaultPermission)
		}
		if _, ok := bc.ToMap()["default_permission"]; ok {
			t.Error("ToMap carries default_permission — Start would send a permission request")
		}
	})

	t.Run("other family skips opencode keys", func(t *testing.T) {
		cfg := &config.Config{OpencodeBackend: config.OpencodeBackendConfig{DefaultPermission: "deny"}}
		cc := delegator.Spec{ConfigFamily: delegator.ConfigFamilyClaudeCode}
		bc := foldGlobalBackendConfig(cfg, cc, config.BackendConfig{})
		if bc.DefaultPermission != nil {
			t.Error("cc agent folded the opencode global default_permission")
		}
	})
}

// cfgRecord is the shared storage of a registered cfgCatcher backend: the
// last backend_config map its constructor was handed.
type cfgRecord struct{ got map[string]any }

// cfgCatcher is a registered test-only backend that records the config map
// the gateway's NewBackend closure hands its constructor.
type cfgCatcher struct {
	delegator.Delegator
	rec *cfgRecord
}

// registerCfgCatcher registers a test-only backend in config family family
// that records, into the returned record, the backend_config map
// configureDelegated's NewBackend closure builds for it — the folded config a
// real backend launches with. The registry is process-global, so each test
// uses its own name and a rerun (-count) resets the record. Mirrors
// registerHookCatcher (agents_delegated_hosthooks_test.go).
func registerCfgCatcher(name, family string) *cfgRecord {
	if s, ok := delegator.SpecFor(name); ok {
		be, _ := s.New(nil)
		rec := be.(*cfgCatcher).rec
		rec.got = nil
		return rec
	}
	rec := &cfgRecord{}
	spec := delegator.Spec{
		Name:         name,
		DisplayName:  name,
		New:          func(cfg map[string]any) (delegator.Delegator, error) { rec.got = cfg; return &cfgCatcher{rec: rec}, nil },
		Prototype:    (*cfgCatcher)(nil),
		Caps:         map[delegator.Capability]delegator.Support{},
		ModelcapsKey: name,
		LedgerKey:    name,
		ConfigFamily: family,
	}
	for _, c := range delegator.AllCapabilities() {
		spec.Caps[c] = delegator.No("test backend")
	}
	delegator.Register(spec)
	return rec
}

// TestConfigureDelegated_FoldsGlobalDefaultPermissionIntoBackendConfig pins
// foldGlobalBackendConfig's CALL SITE, which the unit test above cannot see:
// configureDelegated must hand every backend it creates the folded
// backend_config, so the global [opencode_backend] default_permission reaches
// the backend's config map (the key opencode's Start reads, #2330), the
// per-agent value still wins, and an unset global sends no
// default_permission at all (requirement 4: no permission request at Start).
// The call site once survived the mutation gate precisely because only the
// extracted function was tested, never its caller.
func TestConfigureDelegated_FoldsGlobalDefaultPermissionIntoBackendConfig(t *testing.T) {
	t.Parallel()

	ask := "ask"
	for _, tc := range []struct {
		name     string
		backend  string
		perAgent *string
		global   string
		want     string // "" = the key must be absent
	}{
		{"global default_permission reaches the backend", "test-cfg-fold-global", nil, "deny", "deny"},
		{"per-agent default_permission wins over the global", "test-cfg-fold-agent", &ask, "deny", "ask"},
		{"no global and no per-agent sends none", "test-cfg-fold-none", nil, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := registerCfgCatcher(tc.backend, delegator.ConfigFamilyOpencode)
			p, shared := wiringFixture(t)
			p.cfg.OpencodeBackend.DefaultPermission = tc.global
			ag := &agent.Agent{}
			if _, ok := configureDelegated(ag, p, shared, tc.backend, config.BackendConfig{DefaultPermission: tc.perAgent}); !ok {
				t.Fatal("configureDelegated returned ok=false")
			}
			if _, err := ag.DelegatedManager.NewBackend(); err != nil {
				t.Fatalf("NewBackend: %v", err)
			}
			if rec.got == nil {
				t.Fatal("test backend constructor was not handed a config map")
			}
			if tc.want == "" {
				if _, ok := rec.got["default_permission"]; ok {
					t.Errorf("backend config carries default_permission %v; an unset global must send none", rec.got["default_permission"])
				}
				return
			}
			if got := rec.got["default_permission"]; got != tc.want {
				t.Errorf("backend config default_permission = %v, want %q", got, tc.want)
			}
		})
	}
}
