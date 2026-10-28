package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// loadToolAccessConfig loads a config with the given [tools] body and one
// agent "a" with the given [agents.tools] body, through the real Load.
func loadToolAccessConfig(t *testing.T, globalTools, agentTools string) (*Config, error) {
	t.Helper()
	body := "[groups]\npowerful = \"anthropic/claude-haiku-4-5-20251001\"\n\n[tools]\n" + globalTools +
		"\n\n[[agents]]\nid = \"a\"\n\n[agents.tools]\n" + agentTools + "\n"
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path, nil)
}

func resolvedToolsFor(t *testing.T, globalTools, agentTools string) ResolvedTool {
	t.Helper()
	cfg, err := loadToolAccessConfig(t, globalTools, agentTools)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.UndefinedKeys) > 0 {
		t.Fatalf("unknown keys: %v", cfg.UndefinedKeys)
	}
	return Resolve(cfg, cfg.Agents[0]).Tools
}

// TestToolAccess_AgentAllowReplacesGlobal proves an agent's allow_tools
// replaces the global list rather than merging with it.
func TestToolAccess_AgentAllowReplacesGlobal(t *testing.T) {
	rt := resolvedToolsFor(t, `allow_tools = ["todo", "whoami"]`, `allow_tools = ["web_fetch"]`)
	if !slices.Equal(rt.AllowTools, []string{"web_fetch"}) {
		t.Errorf("AllowTools = %v, want [web_fetch]", rt.AllowTools)
	}
	if rt.ToolPermitted("todo") || !rt.ToolPermitted("web_fetch") {
		t.Errorf("ToolPermitted disagrees with AllowTools %v", rt.AllowTools)
	}
}

// TestToolAccess_GlobalAllowInherited proves an agent with no allow_tools of
// its own gets the global list.
func TestToolAccess_GlobalAllowInherited(t *testing.T) {
	rt := resolvedToolsFor(t, `allow_tools = ["todo", "whoami"]`, ``)
	if !slices.Equal(rt.AllowTools, []string{"todo", "whoami"}) {
		t.Errorf("AllowTools = %v, want [todo whoami]", rt.AllowTools)
	}
	if rt.ToolPermitted("shell") {
		t.Error("shell permitted outside the inherited allowlist")
	}
}

// TestToolAccess_EmptyAgentAllowMeansAllTools proves an agent's explicit
// empty allow_tools (decoded from TOML `[]`) lifts a global allowlist: every
// tool not denied is permitted.
func TestToolAccess_EmptyAgentAllowMeansAllTools(t *testing.T) {
	rt := resolvedToolsFor(t, `allow_tools = ["todo", "whoami"]`, `allow_tools = []`)
	if len(rt.AllowTools) != 0 {
		t.Errorf("AllowTools = %v, want empty", rt.AllowTools)
	}
	for _, name := range FociToolNames {
		if !rt.ToolPermitted(name) {
			t.Errorf("%s withheld under an empty agent allowlist", name)
		}
	}
}

// TestToolAccess_DenyIsUnion proves deny_tools is the union of the global
// and agent lists, so an agent cannot lift a globally denied tool, and that
// deny is applied after allow.
func TestToolAccess_DenyIsUnion(t *testing.T) {
	rt := resolvedToolsFor(t, `deny_tools = ["bitwarden_search", "browser"]`,
		`allow_tools = ["browser", "todo"]`+"\n"+`deny_tools = ["browser", "shell"]`)
	if !slices.Equal(rt.DenyTools, []string{"bitwarden_search", "browser", "shell"}) {
		t.Errorf("DenyTools = %v, want the de-duplicated union", rt.DenyTools)
	}
	for name, want := range map[string]bool{"bitwarden_search": false, "browser": false, "shell": false, "todo": true, "whoami": false} {
		if got := rt.ToolPermitted(name); got != want {
			t.Errorf("ToolPermitted(%s) = %v, want %v", name, got, want)
		}
	}
}

// TestToolAccess_NoListsNoFiltering is the characterisation case: with no
// lists anywhere, nothing is filtered.
func TestToolAccess_NoListsNoFiltering(t *testing.T) {
	rt := resolvedToolsFor(t, ``, ``)
	if rt.AllowTools != nil || rt.DenyTools != nil {
		t.Errorf("got allow %v deny %v, want none", rt.AllowTools, rt.DenyTools)
	}
	for _, name := range FociToolNames {
		if !rt.ToolPermitted(name) {
			t.Errorf("%s withheld with no lists", name)
		}
	}
}

// TestToolAccess_UnknownNameRejected proves Load rejects a tool name that is
// not a foci tool, globally and per agent, in either list, naming the scope,
// the list, the bad name and the valid names. The foci_-prefixed shell
// function name is not accepted either.
func TestToolAccess_UnknownNameRejected(t *testing.T) {
	for _, tc := range []struct {
		name, global, agent, wantScope, wantName string
	}{
		{"global deny", `deny_tools = ["browsr"]`, ``, "tools.deny_tools", `"browsr"`},
		{"global allow", `allow_tools = ["Shell"]`, ``, "tools.allow_tools", `"Shell"`},
		{"agent allow prefixed", ``, `allow_tools = ["foci_todo"]`, "agents[a].tools.allow_tools", `"foci_todo"`},
		{"agent deny", ``, `deny_tools = ["browser", "send_to_sesion"]`, "agents[a].tools.deny_tools", `"send_to_sesion"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadToolAccessConfig(t, tc.global, tc.agent)
			if err == nil {
				t.Fatal("Load accepted an unknown tool name")
			}
			for _, want := range []string{tc.wantScope, tc.wantName, "valid tool names: " + strings.Join(FociToolNames, ", ")} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
		})
	}
}
