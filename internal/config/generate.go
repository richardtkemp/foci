package config

import (
	"fmt"
	"strings"
)

// SetupOptions holds the inputs collected by the setup wizard.
type SetupOptions struct {
	AgentID     string   // agent identifier (e.g. "fotini")
	Model       string   // model ID (e.g. "claude-sonnet-4-6")
	AgentBlock  string   // pre-built [[agents]] TOML from provision.GenerateAgentBlock (if set, overrides AgentID/SystemFiles)
	SystemFiles []string // workspace-relative character file paths (used when AgentBlock is empty)
}

// SecretsOptions holds credentials for secrets.toml generation.
type SecretsOptions struct {
	// Anthropic auth
	SetupToken string // setup-token from `claude setup-token` or API key
}

// GenerateConfig produces a minimal foci.toml containing only the values
// that differ from defaults. The output is valid TOML ready to write to disk.
func GenerateConfig(opts SetupOptions) string {
	var b strings.Builder

	// [defaults]
	if opts.Model != "" {
		b.WriteString("[defaults]\n")
		b.WriteString(fmt.Sprintf("model = %q\n", opts.Model))
		b.WriteString("\n")
	}

	// [[agents]]
	if opts.AgentBlock != "" {
		b.WriteString(strings.TrimLeft(opts.AgentBlock, "\n"))
		b.WriteString("\n")
	} else {
		b.WriteString("[[agents]]\n")
		b.WriteString(fmt.Sprintf("id = %q\n", opts.AgentID))
		if len(opts.SystemFiles) > 0 {
			b.WriteString("system_files = [\n")
			for _, f := range opts.SystemFiles {
				b.WriteString(fmt.Sprintf("  %q,\n", f))
			}
			b.WriteString("]\n")
		}
		b.WriteString("\n")
	}

	return b.String()
}

