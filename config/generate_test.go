package config

import (
	"strings"
	"testing"

	tomlParser "github.com/BurntSushi/toml"
)

func TestGenerateConfig(t *testing.T) {
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
		AllowedUsers: []string{"12345678"},
	}

	result := GenerateConfig(opts)

	// Must be valid TOML
	var parsed map[string]any
	if _, err := tomlParser.Decode(result, &parsed); err != nil {
		t.Fatalf("generated config is not valid TOML: %v\nOutput:\n%s", err, result)
	}

	// Check required fields are present
	if !strings.Contains(result, `id = "fotini"`) {
		t.Error("missing agent id")
	}
	if !strings.Contains(result, `model = "claude-sonnet-4-6"`) {
		t.Error("missing model")
	}
	if !strings.Contains(result, `allowed_users = ["12345678"]`) {
		t.Error("missing allowed_users")
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
}

func TestGenerateConfigMinimal(t *testing.T) {
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
	// No model section when empty
	if strings.Contains(result, "[defaults]") {
		t.Error("should not have [defaults] section when model is empty")
	}
	// No allowed_users when none specified
	if strings.Contains(result, "allowed_users") {
		t.Error("should not have allowed_users when none specified")
	}
	// Should NOT have telegram.bots section (removed)
	if strings.Contains(result, "telegram.bots") {
		t.Error("should not contain [telegram.bots] section")
	}
}

func TestGenerateSecretsSetupToken(t *testing.T) {
	opts := SecretsOptions{
		AgentID:    "fotini",
		SetupToken: "sk-ant-oat01-testtoken123456789012345678901234567890123456789012345678901234567",
		BotToken:   "123456789:AAF-test",
	}

	result := GenerateSecrets(opts)

	var parsed map[string]any
	if _, err := tomlParser.Decode(result, &parsed); err != nil {
		t.Fatalf("generated secrets is not valid TOML: %v\nOutput:\n%s", err, result)
	}

	if !strings.Contains(result, `setup_token = "sk-ant-oat01-`) {
		t.Error("missing setup_token")
	}
	if !strings.Contains(result, `[telegram]`) {
		t.Error("missing telegram section")
	}
	if !strings.Contains(result, `fotini = "123456789:AAF-test"`) {
		t.Error("missing bot token")
	}
}

func TestGenerateSecretsAPIKey(t *testing.T) {
	opts := SecretsOptions{
		AgentID:    "main",
		SetupToken: "sk-ant-api03-test",
		BotToken:   "123:ABC",
	}

	result := GenerateSecrets(opts)

	var parsed map[string]any
	if _, err := tomlParser.Decode(result, &parsed); err != nil {
		t.Fatalf("generated secrets is not valid TOML: %v\nOutput:\n%s", err, result)
	}

	if !strings.Contains(result, `setup_token = "sk-ant-api03-test"`) {
		t.Error("missing setup_token")
	}
	if !strings.Contains(result, `[telegram]`) {
		t.Error("missing telegram section")
	}
	if !strings.Contains(result, `main = "123:ABC"`) {
		t.Error("missing bot token")
	}
	// API key mode should NOT have OAuth fields
	if strings.Contains(result, "oauth_access_token") {
		t.Error("API key mode should not include oauth fields")
	}
}

func TestGenerateSecretsNoAuth(t *testing.T) {
	opts := SecretsOptions{
		AgentID:  "main",
		BotToken: "123:ABC",
	}

	result := GenerateSecrets(opts)

	// Should not have [anthropic] section
	if strings.Contains(result, "[anthropic]") {
		t.Error("should not have [anthropic] section when no auth configured")
	}
	// Should still have bot token
	if !strings.Contains(result, `[telegram]`) {
		t.Error("missing telegram section")
	}
	if !strings.Contains(result, `main = "123:ABC"`) {
		t.Error("missing bot token")
	}
}
