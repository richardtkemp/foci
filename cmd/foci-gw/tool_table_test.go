package main

import (
	"path/filepath"
	"testing"

	"foci/internal/agent"
	"foci/internal/memory"
	"foci/internal/tools"
)

// TestToolTable_PerPathSets locks the unified tool table (tool_table.go) as the
// single source of truth for which tools exist on which path. It asserts the
// exact, ordered list of entry names for the API path and the (subset) exec
// path. The API order is load-bearing: it fixes the model's tool-list order and
// therefore the prompt-cache prefix, so this test fails loudly if an entry is
// added, removed, reordered, or has its path flags changed.
func TestToolTable_PerPathSets(t *testing.T) {
	t.Parallel()

	var api, exec []string
	for _, e := range toolTable {
		if e.paths&pathAPI != 0 {
			api = append(api, e.name)
		}
		if e.paths&pathExec != 0 {
			exec = append(exec, e.name)
		}
	}

	wantAPI := []string{
		"shell", "tmux", "browser", "read", "write", "edit", "summary",
		"http_request", "web_search", "web_fetch", "memory_search",
		"scratchpad", "todo", "task_list", "bitwarden_search",
		"bitwarden_unlock", "mcp", "send_to_chat", "send_to_session",
		"ask", "spawn", "remind", "app_android", "set_session_alias",
	}
	wantExec := []string{
		"summary", "http_request", "web_search", "web_fetch",
		"memory_search", "todo", "send_to_chat", "send_to_session",
		"ask", "spawn", "remind", "app_android", "set_session_alias",
	}

	if !equalStrings(api, wantAPI) {
		t.Errorf("API path entries (ordered) = %v, want %v", api, wantAPI)
	}
	if !equalStrings(exec, wantExec) {
		t.Errorf("exec path entries (ordered) = %v, want %v", exec, wantExec)
	}
}

// TestToolTable_APISet drives registerTools(pathAPI) end-to-end with every
// conditional dependency populated, then asserts the actually-registered tool
// names match the expected API set. This proves the table's build closures run
// without panicking and register the tools their rows promise — complementing
// the table-level name lock above (which checks the rows, not the closures).
// tmux is host-dependent (gated on the tmux binary) and mcp requires an
// mcp.toml, so both are folded into the expected set conditionally.
func TestToolTable_APISet(t *testing.T) {
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
	p.braveKey = "stub-key"
	p.scratchpadStore = &memory.Scratchpad{} // non-nil enables scratchpad
	p.memBackends = map[string]memory.Searcher{"stub": nil}
	p.resolved.Browser.Enabled = true
	p.resolved.Tools.SearchProvider = "brave" // brave web_search (not server-tool)

	registry := tools.NewRegistry()
	registerTools(&toolDeps{
		p:        p,
		path:     pathAPI,
		registry: registry,
		connMgr:  stubConnMgr{},
		agLazy:   func() *agent.Agent { return nil },
		wakeFn:   stubWakeFn,
		out:      &toolOutputs{},
	})

	want := map[string]bool{
		"shell": true, "browser": true, "read": true, "write": true,
		"edit": true, "summary": true, "http_request": true,
		"web_search": true, "web_fetch": true, "memory_search": true,
		"scratchpad": true, "todo": true, "send_to_chat": true,
		"send_to_session": true, "ask": true, "spawn": true, "remind": true,
		"set_session_alias": true,
	}
	// task_list and bitwarden need their stores; taskListStore/bwStore are left
	// nil here, so those rows are intentionally absent from `want`.
	if tmuxAvailable(nil) {
		want["tmux"] = true
	}

	// mcp registration depends on whether an mcp.toml/dynamic config is
	// discoverable from the cwd, not on the table — ignore it here. Its
	// pathAPI membership is locked by TestToolTable_PerPathSets.
	assertRegistrySet(t, registry, want, "mcp")
}

// TestToolTable_ExecSet drives registerTools(pathExec) with full deps and
// asserts the registered set is exactly the exec-exported subset. The exec path
// touches neither tmux nor browser nor mcp, so the expected set is fully
// deterministic — this is the per-path counterpart to the buildExecRegistry
// integration tests, but exercising the table driver directly.
func TestToolTable_ExecSet(t *testing.T) {
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
	p.braveKey = "stub-key"
	p.memBackends = map[string]memory.Searcher{"stub": nil}

	registry := tools.NewRegistry()
	registerTools(&toolDeps{
		p:        p,
		path:     pathExec,
		registry: registry,
		connMgr:  stubConnMgr{},
		agLazy:   func() *agent.Agent { return nil },
		wakeFn:   stubWakeFn,
		out:      &toolOutputs{},
	})

	want := map[string]bool{
		"summary": true, "http_request": true, "web_search": true,
		"web_fetch": true, "memory_search": true, "todo": true,
		"send_to_chat": true, "send_to_session": true, "ask": true,
		"remind": true, "set_session_alias": true,
	}
	assertRegistrySet(t, registry, want)
}

// assertRegistrySet checks the registry's tool-name set equals want exactly,
// skipping any names in ignore (config/host-dependent rows).
func assertRegistrySet(t *testing.T, registry *tools.Registry, want map[string]bool, ignore ...string) {
	t.Helper()
	skip := map[string]bool{}
	for _, n := range ignore {
		skip[n] = true
	}
	got := map[string]bool{}
	for _, tool := range registry.All() {
		if skip[tool.Name] {
			continue
		}
		got[tool.Name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("expected tool %q registered, missing", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("unexpected tool %q registered", name)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
