package config

import (
	"fmt"
	"strings"
)

// SetupOptions holds the inputs collected by the setup wizard.
type SetupOptions struct {
	Model      string // developer/model_id (e.g. "anthropic/claude-sonnet-4-6")
	Endpoint   string // endpoint override for model (e.g. "openrouter"); empty = auto-detect
	AgentBlock string // pre-built [[agents]] TOML from provision.GenerateAgentBlock

	// Custom endpoint (only when provider is "custom")
	CustomEndpoint *CustomEndpointSetup
}

// CustomEndpointSetup holds config for a user-defined endpoint.
type CustomEndpointSetup struct {
	Name      string // endpoint name (e.g. "local")
	URL       string // base URL (e.g. "http://localhost:8000/v1")
	Format    string // wire format: "anthropic", "openai", or "gemini"
	SecretKey string // secret key name (e.g. "custom.api_key")
}

// GenerateConfig produces a minimal foci.toml containing the groups, models,
// agents, and (optionally) custom endpoint sections.
func GenerateConfig(opts SetupOptions) string {
	var b strings.Builder

	// [groups]
	if opts.Model != "" {
		b.WriteString("[groups]\n")
		b.WriteString("powerful = \"default\"\n")
		b.WriteString("\n")

		// [models.default]
		b.WriteString("[models.default]\n")
		b.WriteString(fmt.Sprintf("model = %q\n", opts.Model))
		if opts.Endpoint != "" {
			b.WriteString(fmt.Sprintf("endpoint = %q\n", opts.Endpoint))
		}
		b.WriteString("\n")
	}

	// [endpoints.custom] (only for custom provider)
	if opts.CustomEndpoint != nil {
		ce := opts.CustomEndpoint
		b.WriteString(fmt.Sprintf("[endpoints.%s]\n", ce.Name))
		b.WriteString(fmt.Sprintf("format = %q\n", ce.Format))
		b.WriteString(fmt.Sprintf("url = %q\n", ce.URL))
		if ce.SecretKey != "" {
			b.WriteString(fmt.Sprintf("api_key = %q\n", ce.SecretKey))
		}
		b.WriteString("\n")
	}

	// [[agents]]
	if opts.AgentBlock != "" {
		b.WriteString(strings.TrimLeft(opts.AgentBlock, "\n"))
		b.WriteString("\n")
	}

	return b.String()
}
