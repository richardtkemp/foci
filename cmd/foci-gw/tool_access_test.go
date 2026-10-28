package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/memory"
	"foci/internal/secrets/bitwarden"
	"foci/internal/tools"
)

// toolAccessCase is one agent's tool-access configuration (#2278): the global
// [tools] lists and the agent's [[agents]] tools lists. nil means "not set".
type toolAccessCase struct {
	globalAllow, globalDeny []string
	agentAllow, agentDeny   []string
}

// fullToolParams builds setupParams with every conditional tool dependency
// populated (the TestToolTable_APISet shape plus bitwarden), resolving the
// case's lists through the real config.Resolve.
func fullToolParams(t *testing.T, c toolAccessCase) setupParams {
	t.Helper()
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
	p.cfg.Tools.AllowTools, p.cfg.Tools.DenyTools = c.globalAllow, c.globalDeny
	p.acfg.Tools.AllowTools, p.acfg.Tools.DenyTools = c.agentAllow, c.agentDeny
	p.resolved = config.Resolve(p.cfg, p.acfg)
	p.resolvedLive = config.NewLiveValue(p.resolved)
	p.reminderStore = rs
	p.todoStore = ts
	p.braveKey = "stub-key"
	p.scratchpadStore = &memory.Scratchpad{}
	p.memBackends = map[string]memory.Searcher{"stub": nil}
	p.bwStore = &bitwarden.Store{}
	p.resolved.Browser.Enabled = true
	p.resolved.Tools.SearchProvider = "brave"
	return p
}

// registerFor runs registerTools on one path and returns the registry and the
// side outputs.
func registerFor(p setupParams, path toolPath) (*tools.Registry, *toolOutputs) {
	registry := tools.NewRegistry()
	out := &toolOutputs{}
	registerTools(&toolDeps{
		p:        p,
		path:     path,
		registry: registry,
		connMgr:  stubConnMgr{},
		agLazy:   func() *agent.Agent { return nil },
		wakeFn:   stubWakeFn,
		out:      out,
	})
	return registry, out
}

func registeredNames(r *tools.Registry) []string {
	var names []string
	for _, tl := range r.All() {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	return names
}

// withoutHostDependent drops rows whose presence depends on the host (tmux on
// PATH) or the cwd (mcp.toml), so set comparisons are deterministic.
func withoutHostDependent(names []string) []string {
	return slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n == "tmux" || n == "mcp" })
}

// TestToolAccess_NoListsSameSetAsToday is the characterisation case: with no
// allow or deny list anywhere, resolving through config.Resolve registers
// exactly the set a zero ResolvedTool (the pre-#2278 shape) registers, on
// both paths.
func TestToolAccess_NoListsSameSetAsToday(t *testing.T) {
	t.Parallel()
	for _, path := range []toolPath{pathAPI, pathExec} {
		p := fullToolParams(t, toolAccessCase{})
		got, _ := registerFor(p, path)

		base := fullToolParams(t, toolAccessCase{})
		base.resolved.Tools = config.ResolvedTool{SearchProvider: "brave"}
		want, _ := registerFor(base, path)

		if g, w := registeredNames(got), registeredNames(want); !slices.Equal(g, w) {
			t.Errorf("path %d: no lists registered %v, want %v", path, g, w)
		}
		if len(registeredNames(got)) < 10 {
			t.Errorf("path %d: suspiciously few tools: %v", path, registeredNames(got))
		}
	}
}

// TestToolAccess_AgentAllowReplacesGlobal proves the allow rule: a global
// allow [todo, whoami] with agent allow [web_fetch] leaves the agent exactly
// web_fetch, on both paths.
func TestToolAccess_AgentAllowReplacesGlobal(t *testing.T) {
	t.Parallel()
	for _, path := range []toolPath{pathAPI, pathExec} {
		p := fullToolParams(t, toolAccessCase{globalAllow: []string{"todo", "whoami"}, agentAllow: []string{"web_fetch"}})
		r, _ := registerFor(p, path)
		if got := registeredNames(r); !slices.Equal(got, []string{"web_fetch"}) {
			t.Errorf("path %d: registered %v, want [web_fetch]", path, got)
		}
	}
}

// TestToolAccess_EmptyAgentAllowLiftsGlobalAllowlist proves an agent that sets
// an empty allow list gets every tool despite a global allowlist.
func TestToolAccess_EmptyAgentAllowLiftsGlobalAllowlist(t *testing.T) {
	t.Parallel()
	for _, path := range []toolPath{pathAPI, pathExec} {
		p := fullToolParams(t, toolAccessCase{globalAllow: []string{"todo", "whoami"}, agentAllow: []string{}})
		r, _ := registerFor(p, path)
		all, _ := registerFor(fullToolParams(t, toolAccessCase{}), path)
		if got, want := registeredNames(r), registeredNames(all); !slices.Equal(got, want) {
			t.Errorf("path %d: registered %v, want every tool %v", path, got, want)
		}
	}
}

// TestToolAccess_GlobalAllowAppliesWithoutAgentList proves an agent with no
// allow list of its own inherits the global allowlist.
func TestToolAccess_GlobalAllowAppliesWithoutAgentList(t *testing.T) {
	t.Parallel()
	p := fullToolParams(t, toolAccessCase{globalAllow: []string{"todo", "whoami"}})
	r, _ := registerFor(p, pathExec)
	if got := registeredNames(r); !slices.Equal(got, []string{"todo", "whoami"}) {
		t.Errorf("registered %v, want [todo whoami]", got)
	}
}

// TestToolAccess_GlobalDenySurvivesAgentDeny proves deny is the union: global
// deny [bitwarden_search] plus agent deny [browser] withholds both on the API
// path, while the rest of the set is untouched.
func TestToolAccess_GlobalDenySurvivesAgentDeny(t *testing.T) {
	t.Parallel()
	p := fullToolParams(t, toolAccessCase{globalDeny: []string{"bitwarden_search"}, agentDeny: []string{"browser"}})
	r, _ := registerFor(p, pathAPI)
	got := registeredNames(r)
	for _, withheld := range []string{"bitwarden_search", "browser"} {
		if slices.Contains(got, withheld) {
			t.Errorf("%s registered despite deny: %v", withheld, got)
		}
	}
	if !slices.Contains(got, "bitwarden_unlock") || !slices.Contains(got, "shell") {
		t.Errorf("deny removed more than it named: %v", got)
	}
}

// TestToolAccess_AllowThenDenyOnAPIPath proves allow keeps only the named
// tools and a tool in both lists is withheld (deny after allow).
func TestToolAccess_AllowThenDenyOnAPIPath(t *testing.T) {
	t.Parallel()
	allow := []string{"todo", "whoami", "shell"}
	r, _ := registerFor(fullToolParams(t, toolAccessCase{agentAllow: allow}), pathAPI)
	if got := registeredNames(r); !slices.Equal(got, []string{"shell", "todo", "whoami"}) {
		t.Errorf("allow only: registered %v, want [shell todo whoami]", got)
	}
	r, _ = registerFor(fullToolParams(t, toolAccessCase{agentAllow: allow, agentDeny: []string{"shell"}}), pathAPI)
	if got := registeredNames(r); !slices.Equal(got, []string{"todo", "whoami"}) {
		t.Errorf("allow + deny shell: registered %v, want [todo whoami]", got)
	}
}

// TestToolAccess_DeniedToolAbsentFromAPIShell proves a denied tool is absent
// from the API shell tool's description and from the foci_* functions the
// shell offers (the registry's exported names), and that a sub-agent built
// from the same registry cannot see it either.
func TestToolAccess_DeniedToolAbsentFromAPIShell(t *testing.T) {
	t.Parallel()
	r, _ := registerFor(fullToolParams(t, toolAccessCase{agentDeny: []string{"todo"}}), pathAPI)
	r.FinalizeShellDescription()
	sh := r.Get("shell")
	if sh == nil {
		t.Fatal("shell not registered")
	}
	if strings.Contains(sh.Description, "foci_todo") {
		t.Errorf("shell description mentions foci_todo:\n%s", sh.Description)
	}
	if !strings.Contains(sh.Description, "foci_whoami") {
		t.Errorf("shell description lost the permitted foci_whoami (fixture broken?):\n%s", sh.Description)
	}
	if slices.Contains(r.ExportedNames(), "foci_todo") || r.Get("todo") != nil {
		t.Errorf("todo still reachable: exported %v", r.ExportedNames())
	}
}

// TestToolAccess_DeniedServerToolAddsNothing proves a denied web_search with
// search_provider = "anthropic" adds no server tool (its build never runs),
// against a control that does add it.
func TestToolAccess_DeniedServerToolAddsNothing(t *testing.T) {
	t.Parallel()
	hasServerTool := func(out *toolOutputs, name string) bool {
		for _, st := range out.serverTools {
			if st.Name() == name {
				return true
			}
		}
		return false
	}
	ctl := fullToolParams(t, toolAccessCase{})
	ctl.resolved.Tools.SearchProvider = "anthropic"
	_, out := registerFor(ctl, pathAPI)
	if !hasServerTool(out, "web_search") {
		t.Fatalf("control: anthropic web_search server tool missing: %+v", out.serverTools)
	}

	p := fullToolParams(t, toolAccessCase{agentDeny: []string{"web_search"}})
	p.resolved.Tools.SearchProvider = "anthropic"
	_, out = registerFor(p, pathAPI)
	if hasServerTool(out, "web_search") {
		t.Errorf("denied web_search still added a server tool: %+v", out.serverTools)
	}
}

// TestToolAccess_DeniedRowIsNeverBuilt proves withheld rows are never built:
// the mcp row's build always creates an MCP manager and the ask row's always
// creates an ask router, so with both denied (and an mcp.toml present) the
// outputs stay nil, against a control where they are set.
func TestToolAccess_DeniedRowIsNeverBuilt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mcp.toml"), []byte("servers = []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctl := fullToolParams(t, toolAccessCase{})
	ctl.configPath = filepath.Join(dir, "foci.toml")
	r, out := registerFor(ctl, pathAPI)
	if out.mcpMgr == nil || out.askRouter == nil || r.Get("mcp") == nil {
		t.Fatalf("control: mcp manager %v, ask router %v, mcp tool %v", out.mcpMgr, out.askRouter, r.Get("mcp"))
	}
	_ = out.mcpMgr.Close()

	p := fullToolParams(t, toolAccessCase{agentDeny: []string{"mcp", "ask"}})
	p.configPath = filepath.Join(dir, "foci.toml")
	r, out = registerFor(p, pathAPI)
	if out.mcpMgr != nil {
		t.Error("denied mcp row was built: an MCP manager exists")
	}
	if out.askRouter != nil {
		t.Error("denied ask row was built: an ask router exists")
	}
	if r.Get("mcp") != nil || r.Get("ask") != nil {
		t.Error("denied tools registered")
	}
}

// TestToolAccess_DelegatedDenyAbsentEverywhere proves, for a claude-code agent
// with send_to_session and browser denied: buildExecRegistry exports neither,
// buildAutoApproveRules has no rule for them, the environment block's Foci
// Shell Tools list omits them, and a direct exec-bridge call to
// send_to_session is refused as an unknown tool.
func TestToolAccess_DelegatedDenyAbsentEverywhere(t *testing.T) {
	t.Parallel()
	p := fullToolParams(t, toolAccessCase{agentDeny: []string{"send_to_session", "browser"}})
	p.acfg.Backend = "claude-code"
	p.acfg.Workspace = t.TempDir()

	registry := buildExecRegistry(p, stubWakeFn, nil, nil)
	names := registry.ExportedNames()
	if !slices.Contains(names, "foci_todo") {
		t.Fatalf("fixture broken: foci_todo missing from %v", names)
	}
	rules := buildAutoApproveRules(p, names)
	env := buildEnvironmentDelegated(p.acfg, filepath.Join(t.TempDir(), "foci.toml"), p.cfg, p.resolved,
		p.resolved.Permissions, 0, nil, registry.ExportedTools(), nil, "")
	if !strings.Contains(env, "foci_todo") {
		t.Fatalf("fixture broken: env block lacks foci_todo:\n%s", env)
	}
	for _, fn := range []string{"foci_send_to_session", "foci_browser"} {
		if slices.Contains(names, fn) {
			t.Errorf("ExportedNames() has %s: %v", fn, names)
		}
		for _, r := range rules {
			if strings.Contains(r, fn) {
				t.Errorf("auto-approve rule %q for denied %s", r, fn)
			}
		}
		if strings.Contains(env, fn) {
			t.Errorf("environment block lists denied %s", fn)
		}
	}

	bridge, err := tools.NewExecBridge(registry, context.Background())
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	t.Cleanup(bridge.Close)
	errMsg := callExecBridge(t, bridge.SockPath(), `{"tool":"send_to_session","params":{"target":"x","message":"y"}}`)
	if !strings.Contains(errMsg, "unknown tool") {
		t.Errorf("bridge call to denied send_to_session: error %q, want unknown tool", errMsg)
	}
}

// TestFociToolNamesMatchToolTable keeps config.FociToolNames (what allow_tools
// / deny_tools may name) identical to the toolTable row names, so a new tool
// row cannot be left unnameable, and a removed one cannot stay valid.
func TestFociToolNamesMatchToolTable(t *testing.T) {
	t.Parallel()
	var rows []string
	for _, e := range toolTable {
		rows = append(rows, e.name)
	}
	slices.Sort(rows)
	names := slices.Clone(config.FociToolNames)
	slices.Sort(names)
	if !slices.Equal(rows, names) {
		t.Errorf("toolTable rows %v != config.FociToolNames %v", rows, names)
	}
}

// TestRunConfigCheck_UnknownToolNameFails proves -check-config exits 1 and
// names the bad entry, its scope and the valid names, for a misspelt global
// deny entry and for a foci_-prefixed agent allow entry; and exits 0 when
// every name is valid.
func TestRunConfigCheck_UnknownToolNameFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		wantCode   int
		wantMsg    []string
	}{
		{"global deny typo", "[tools]\ndeny_tools = [\"browsr\"]\n" + validConfigTOML, 1,
			[]string{"tools.deny_tools", `"browsr"`, "valid tool names:", "browser", "send_to_session"}},
		{"agent allow prefixed", validConfigTOML + "[agents.tools]\nallow_tools = [\"foci_todo\"]\n", 1,
			[]string{"agents[main].tools.allow_tools", `"foci_todo"`, "valid tool names:"}},
		{"valid names", "[tools]\ndeny_tools = [\"browser\"]\n" + validConfigTOML + "[agents.tools]\nallow_tools = [\"todo\", \"shell\"]\ndeny_tools = [\"shell\"]\n", 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeTempConfig(t, tc.body)
			var stderr strings.Builder
			if got := runConfigCheck(path, &strings.Builder{}, &stderr); got != tc.wantCode {
				t.Fatalf("exit %d, want %d; stderr:\n%s", got, tc.wantCode, stderr.String())
			}
			for _, m := range tc.wantMsg {
				if !strings.Contains(stderr.String(), m) {
					t.Errorf("stderr lacks %q:\n%s", m, stderr.String())
				}
			}
		})
	}
}

// callExecBridge sends one request line to an exec bridge socket and returns
// the response's error field.
func callExecBridge(t *testing.T, sockPath, request string) string {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(request + "\n")); err != nil {
		t.Fatalf("write bridge: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read bridge: %v", err)
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("parse bridge response %q: %v", line, err)
	}
	return resp.Error
}
