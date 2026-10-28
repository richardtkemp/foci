package config

import (
	"fmt"
	"slices"
	"strings"
)

// FociToolNames is every foci tool an allow_tools / deny_tools list may name
// (#2278): the row names of the tool registration table (toolTable in
// cmd/foci-gw/tool_table.go, which a test there keeps identical to this
// list). These are the registry names, not the foci_-prefixed shell function
// names delegated backends see. Claude Code / opencode built-in tools (Bash,
// Read, ...) are not foci tools and are not covered.
var FociToolNames = []string{
	"app_android", "ask", "bitwarden_search", "bitwarden_unlock", "browser",
	"close_session", "depth_claims", "depth_fanout", "depth_report",
	"depth_status", "edit", "http_request", "mcp", "memory_search", "read",
	"remind", "scratchpad", "send_to_chat", "send_to_session",
	"set_session_alias", "shell", "spawn", "spawn_send", "spawn_status",
	"summary", "task_list", "tmux", "todo", "web_fetch", "web_search",
	"whoami", "write",
}

// ToolPermitted reports whether the agent may have the foci tool name:
// AllowTools, when non-empty, must list it, and DenyTools must not (deny is
// applied after allow, so a tool in both is withheld). An empty AllowTools is
// "no allowlist": every tool not denied is permitted.
func (t ResolvedTool) ToolPermitted(name string) bool {
	if len(t.AllowTools) > 0 && !slices.Contains(t.AllowTools, name) {
		return false
	}
	return !slices.Contains(t.DenyTools, name)
}

// resolveToolAccess sets the resolved allow and deny lists from the agent's
// and the global [tools] settings. Allow: the agent's list when the agent set
// one (even an empty one, which lifts a global allowlist for that agent),
// else the global list. Deny: the union of both lists, so an agent cannot
// remove a globally denied tool.
func resolveToolAccess(t *ResolvedTool, agent, global ToolConfig) {
	t.AllowTools = global.AllowTools
	if agent.AllowTools != nil {
		t.AllowTools = agent.AllowTools
	}
	var deny []string
	for _, name := range slices.Concat(global.DenyTools, agent.DenyTools) {
		if !slices.Contains(deny, name) {
			deny = append(deny, name)
		}
	}
	t.DenyTools = deny
}

// validateToolLists rejects any allow_tools / deny_tools entry, global or
// per agent, that names no foci tool. A misspelt deny entry would otherwise
// leave the tool switched on with no sign, so this fails the load.
func (cfg *Config) validateToolLists() error {
	if err := checkToolLists("tools", cfg.Tools.ToolConfig); err != nil {
		return err
	}
	for _, a := range cfg.Agents {
		if err := checkToolLists(fmt.Sprintf("agents[%s].tools", a.ID), a.Tools.ToolConfig); err != nil {
			return err
		}
	}
	return nil
}

func checkToolLists(scope string, tc ToolConfig) error {
	for _, l := range []struct {
		key   string
		names []string
	}{{"allow_tools", tc.AllowTools}, {"deny_tools", tc.DenyTools}} {
		for _, name := range l.names {
			if !slices.Contains(FociToolNames, name) {
				return fmt.Errorf("%s.%s: unknown tool %q; valid tool names: %s",
					scope, l.key, name, strings.Join(FociToolNames, ", "))
			}
		}
	}
	return nil
}
