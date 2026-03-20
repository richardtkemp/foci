package skills

import "path/filepath"

// ResolveDirs returns the skill directories to scan, in load order.
// Shared dir comes first; per-agent dir comes second (wins on name collision).
// home is the foci home directory (parent of agent workspaces).
// agentWorkspace is the agent's workspace path.
// sharedOverride, if non-empty, replaces the default shared dir.
// agentOverride, if non-empty, replaces the default per-agent dir.
func ResolveDirs(home, agentWorkspace, sharedOverride, agentOverride string) []string {
	sharedDir := filepath.Join(home, "shared", "skills")
	if sharedOverride != "" {
		sharedDir = sharedOverride
	}
	agentDir := filepath.Join(agentWorkspace, "skills")
	if agentOverride != "" {
		agentDir = agentOverride
	}
	return []string{sharedDir, agentDir}
}
