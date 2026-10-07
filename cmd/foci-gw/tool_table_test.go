package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/memory"
	"foci/internal/provider"
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
		"whoami",
	}
	wantExec := []string{
		"browser", "summary", "http_request", "web_search", "web_fetch",
		"memory_search", "todo", "send_to_chat", "send_to_session",
		"ask", "remind", "app_android", "set_session_alias", "whoami",
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
		"set_session_alias": true, "whoami": true,
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
// touches neither tmux nor mcp, and browser is enabled explicitly, so the
// expected set is fully deterministic — this is the per-path counterpart to the buildExecRegistry
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
	p.resolved.Browser.Enabled = true

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
		"browser": true, "summary": true, "http_request": true, "web_search": true,
		"web_fetch": true, "memory_search": true, "todo": true,
		"send_to_chat": true, "send_to_session": true, "ask": true,
		"remind": true, "set_session_alias": true, "whoami": true,
	}
	assertRegistrySet(t, registry, want)
}

// routingCaptureClient records every MessageRequest a spawn one-shot sends,
// so a test can assert provider routing wired through the tool table reached
// the request object (ProviderRouting is json:"-", invisible to wire-capture
// stubs). Responses are consumed in order; HandlesOwnRetries keeps
// sendWithRetry a single pass, so the script controls the call count.
type routingCaptureClient struct {
	responses []*provider.MessageResponse
	next      int
	reqs      []*provider.MessageRequest
}

func (c *routingCaptureClient) SendMessage(_ context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	if c.next >= len(c.responses) {
		return nil, fmt.Errorf("routingCaptureClient: no scripted response left")
	}
	c.reqs = append(c.reqs, req)
	r := c.responses[c.next]
	c.next++
	return r, nil
}

func (c *routingCaptureClient) CountTokens(_ context.Context, _ *provider.MessageRequest) (int, error) {
	return 0, nil
}

func (c *routingCaptureClient) IsCachingAvailable() bool { return false }

// HandlesOwnRetries makes sendWithRetry skip its retry loop.
func (c *routingCaptureClient) HandlesOwnRetries() bool { return true }

// TestToolTable_SpawnWiresProviderRouting proves the spawn row of the tool
// table wires SpawnDeps.ProviderRoutingFor from [models.*.provider] config
// (#2219): a raw one-shot on a pinned OpenRouter model carries that model's
// routing on every request of its tool loop, keeping its prompt cache on one
// upstream provider. Losing the wiring leaves the deps field nil, and every
// spawn request load-balances across providers instead.
func TestToolTable_SpawnWiresProviderRouting(t *testing.T) {
	t.Parallel()

	routing := &provider.ProviderRouting{Order: []string{"google-vertex/global"}}
	p := minimalSetupParams(t, "test")
	p.cfg.Models = map[string]config.ModelConfig{
		"pinned": {Model: "openrouter/pinned-model", Provider: routing},
	}
	p.resolved.Loop.MaxToolLoops = 5

	registry := tools.NewRegistry()
	registry.Register(&tools.Tool{
		Name:       "echo_tool",
		Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Execute: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.TextResult("echo"), nil
		},
	})
	client := &routingCaptureClient{responses: []*provider.MessageResponse{
		{
			ID: "msg_1", Type: "message", Role: "assistant",
			Content: []provider.ContentBlock{
				{Type: "tool_use", ID: "tu_1", Name: "echo_tool", Input: json.RawMessage(`{"text":"hello"}`)},
			},
			StopReason: "tool_use",
		},
		{
			ID: "msg_2", Type: "message", Role: "assistant",
			Content:    provider.TextContent("done"),
			StopReason: "end_turn",
		},
	}}
	registerTools(&toolDeps{
		p:             p,
		path:          pathAPI,
		registry:      registry,
		connMgr:       stubConnMgr{},
		agLazy:        func() *agent.Agent { return nil },
		wakeFn:        stubWakeFn,
		out:           &toolOutputs{},
		client:        client,
		resolvedModel: "openrouter/pinned-model",
		defaultFormat: "openai",
	})

	spawn := registry.Get("spawn")
	if spawn == nil {
		t.Fatal("spawn tool not registered on the API path")
	}
	if _, err := spawn.Execute(context.Background(), json.RawMessage(`{"prompt":"test","context":"raw"}`)); err != nil {
		t.Fatalf("spawn execute: %v", err)
	}

	if len(client.reqs) != 2 {
		t.Fatalf("spawn sent %d requests, want 2 (one per tool-loop round)", len(client.reqs))
	}
	for i, req := range client.reqs {
		if req.Model != "openrouter/pinned-model" {
			t.Errorf("request %d model = %q, want openrouter/pinned-model", i, req.Model)
		}
		if req.ProviderRouting != routing {
			t.Errorf("request %d ProviderRouting = %+v, want the [models.pinned.provider] routing", i, req.ProviderRouting)
		}
	}
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
