package config

import (
	"strings"
	"testing"

	tomlParser "github.com/BurntSushi/toml"
)

func TestGenerateConfig(t *testing.T) {
	// Proves GenerateConfig produces valid, parseable TOML containing the
	// agent ID, model, and system files, while omitting default-restating
	// keys and platform-specific sections.
	opts := SetupOptions{
		AgentID: "fotini",
		Model:   "claude-sonnet-4-6",
		SystemFiles: []string{
			"character/SOUL.md",
			"character/CRAFT.md",
			"character/COHERENCE.md",
			"character/USER.md",
			"character/MEMORY.md",
		},
	}

	result := GenerateConfig(opts)

	// Must be valid TOML
	var parsed map[string]any
	if _, err := tomlParser.Decode(result, &parsed); err != nil {
		t.Fatalf("generated config is not valid TOML: %v\nOutput:\n%s", err, result)
	}

	if !strings.Contains(result, `id = "fotini"`) {
		t.Error("missing agent id")
	}
	if !strings.Contains(result, `model = "claude-sonnet-4-6"`) {
		t.Error("missing model")
	}
	if !strings.Contains(result, `"character/SOUL.md"`) {
		t.Error("missing system_files entry")
	}

	// Must NOT contain values that restate defaults
	for _, banned := range []string{
		"compaction_threshold",
		"http",
		"port",
		"bind",
		"logging",
		"sessions",
		"data_dir",
	} {
		if strings.Contains(result, banned) {
			t.Errorf("generated config should not contain default-restating key %q", banned)
		}
	}

	// Must NOT contain platform-specific sections (contributed by providers)
	if strings.Contains(result, "[telegram]") {
		t.Error("should not contain [telegram] section — providers contribute that")
	}
}

func TestGenerateConfigMinimal(t *testing.T) {
	// Proves GenerateConfig works with just an agent ID, producing valid TOML
	// without a [defaults] section when model is empty.
	opts := SetupOptions{
		AgentID: "main",
	}
	result := GenerateConfig(opts)

	var parsed map[string]any
	if _, err := tomlParser.Decode(result, &parsed); err != nil {
		t.Fatalf("minimal config is not valid TOML: %v\nOutput:\n%s", err, result)
	}

	if !strings.Contains(result, `id = "main"`) {
		t.Error("missing agent id")
	}
	if strings.Contains(result, "[defaults]") {
		t.Error("should not have [defaults] section when model is empty")
	}
}

func TestGenerateConfigWithAgentBlock(t *testing.T) {
	// Proves that when a pre-built AgentBlock string is supplied, GenerateConfig
	// embeds it verbatim, including workspace and system_files entries.
	agentBlock := `[[agents]]
id = "fotini"
model = "claude-sonnet-4-6"
workspace = "/home/foci/fotini"
system_files = ["character/SOUL.md", "character/COHERENCE.md", "character/CRAFT.md", "character/USER.md", "character/MEMORY.md"]
`
	opts := SetupOptions{
		Model:      "claude-sonnet-4-6",
		AgentBlock: agentBlock,
	}

	result := GenerateConfig(opts)

	var parsed map[string]any
	if _, err := tomlParser.Decode(result, &parsed); err != nil {
		t.Fatalf("generated config is not valid TOML: %v\nOutput:\n%s", err, result)
	}

	if !strings.Contains(result, `id = "fotini"`) {
		t.Error("missing agent id from agent block")
	}
	if !strings.Contains(result, `workspace = "/home/foci/fotini"`) {
		t.Error("missing workspace from agent block")
	}
}

func TestGenerateSecretsSetupToken(t *testing.T) {
	// Proves GenerateSecrets emits valid TOML containing the setup_token and
	// does not include a [telegram] section (platform secrets are provider-managed).
	opts := SecretsOptions{
		SetupToken: "sk-ant-oat01-testtoken123456789012345678901234567890123456789012345678901234567",
	}

	result := GenerateSecrets(opts)

	var parsed map[string]any
	if _, err := tomlParser.Decode(result, &parsed); err != nil {
		t.Fatalf("generated secrets is not valid TOML: %v\nOutput:\n%s", err, result)
	}

	if !strings.Contains(result, `setup_token = "sk-ant-oat01-`) {
		t.Error("missing setup_token")
	}
	// Platform-specific secrets are handled by providers, not GenerateSecrets
	if strings.Contains(result, "[telegram]") {
		t.Error("should not contain [telegram] section")
	}
}

func TestGenerateSecretsAPIKey(t *testing.T) {
	// Proves that a direct API key token (sk-ant-api03-* prefix) is stored as
	// setup_token and does not generate oauth-specific fields.
	opts := SecretsOptions{
		SetupToken: "sk-ant-api03-test",
	}

	result := GenerateSecrets(opts)

	var parsed map[string]any
	if _, err := tomlParser.Decode(result, &parsed); err != nil {
		t.Fatalf("generated secrets is not valid TOML: %v\nOutput:\n%s", err, result)
	}

	if !strings.Contains(result, `setup_token = "sk-ant-api03-test"`) {
		t.Error("missing setup_token")
	}
	if strings.Contains(result, "oauth_access_token") {
		t.Error("API key mode should not include oauth fields")
	}
}

func TestGenerateSecretsNoAuth(t *testing.T) {
	// Proves that GenerateSecrets returns empty output when SecretsOptions has
	// no token, producing nothing rather than an empty [anthropic] section.
	opts := SecretsOptions{}

	result := GenerateSecrets(opts)

	if strings.Contains(result, "[anthropic]") {
		t.Error("should not have [anthropic] section when no auth configured")
	}
	if result != "" {
		t.Errorf("expected empty output, got: %q", result)
	}
}
