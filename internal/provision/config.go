package provision

import (
	"fmt"
	"strings"
)

// GenerateAgentBlock produces a [[agents]] TOML fragment for an agent.
// BotName defaults to spec.ID; only emitted when different.
// Token secret follows the convention telegram.<BotName>; bot_secret only emitted when different.
func GenerateAgentBlock(spec AgentSpec) string {
	var sb strings.Builder

	sb.WriteString("\n")
	sb.WriteString("[[agents]]\n")
	fmt.Fprintf(&sb, "id = %q\n", spec.ID)
	fmt.Fprintf(&sb, "model = %q\n", spec.Model)

	workspace := spec.workspacePath()
	fmt.Fprintf(&sb, "workspace = %q\n", workspace)

	sysFiles := spec.SystemFiles
	if len(sysFiles) == 0 {
		sysFiles = DefaultSystemFiles
	}
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
