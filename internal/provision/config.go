package provision

import (
	"fmt"
	"strings"
)

// GenerateAgentBlock produces a [[agents]] TOML fragment for an agent.
func GenerateAgentBlock(spec AgentSpec) string {
	var sb strings.Builder

	sb.WriteString("\n")
	sb.WriteString("[[agents]]\n")
	fmt.Fprintf(&sb, "id = %q\n", spec.ID)

	workspace := spec.workspacePath()
	fmt.Fprintf(&sb, "workspace = %q\n", workspace)

	// Backend selection. Top-level [[agents]] keys must precede any sub-table
	// header ([agents.system] below), so emit them here. An empty backend is
	// written as explicit "api" rather than omitted, so a new agent records its
	// execution mode instead of falling through to the silent API default.
	backend := spec.Backend
	if backend == "" {
		backend = "api"
	}
	fmt.Fprintf(&sb, "backend = %q\n", backend)
	if backend != "api" && spec.Model != "" {
		// Delegated backends take their model from backend_config.model
		// (e.g. CC's --model). API agents resolve via [groups]/[models].
		fmt.Fprintf(&sb, "backend_config.model = %q\n", spec.Model)
	}

	sysFiles := spec.SystemFiles
	if len(sysFiles) == 0 {
		sysFiles = DefaultSystemFiles
	}
	sb.WriteString("\n[agents.system]\n")
	sb.WriteString("system_files = [")
	for i, f := range sysFiles {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%q", f)
	}
	sb.WriteString("]\n")

	return sb.String()
}
