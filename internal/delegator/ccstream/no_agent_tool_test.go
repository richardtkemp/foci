package ccstream

import (
	"strings"
	"testing"

	"foci/internal/delegator"
)

// TestStart_EveryLaunchDisallowsAgentTool (#2934, Dick 2026-11-07 "Block
// everywhere"): every Claude Code process foci launches gets
// --disallowedTools Agent,Task, so CC's subagent tool is absent from the
// model's tool list; delegation goes through foci_spawn. Covers an ordinary
// chat, a depth leaf, a background batch of each access level and a
// skip-permissions session.
func TestStart_EveryLaunchDisallowsAgentTool(t *testing.T) {
	cases := map[string]delegator.StartOptions{
		"chat":       {},
		"depth leaf": {DepthLeaf: true},
		"batch":      {SkipPermissions: true},
		"parent":     {SkipPermissions: true, BatchAccess: delegator.BatchAccessParent},
		"isolated":   {SkipPermissions: true, BatchAccess: delegator.BatchAccessIsolated},
		"read-only":  {SkipPermissions: true, BatchAccess: delegator.BatchAccessReadOnly},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			got := startArgs(t, opts)
			if !strings.Contains(" "+strings.Join(strings.Fields(got), " ")+" ", " --disallowedTools "+strings.Join(blockedCCTools, ",")+" ") {
				t.Errorf("argv = %q, want --disallowedTools %s", got, strings.Join(blockedCCTools, ","))
			}
		})
	}
}

// TestBlockedCCTools_NamesTheSubagentTool pins the list: both the current
// name and its old alias, nothing else (a wider list would silently remove
// a tool the agent relies on).
func TestBlockedCCTools_NamesTheSubagentTool(t *testing.T) {
	if got := strings.Join(blockedCCTools, ","); got != "Agent,Task" {
		t.Fatalf("blockedCCTools = %q, want Agent,Task", got)
	}
}
