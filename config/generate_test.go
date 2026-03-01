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
	// No telegram section when no users
	if strings.Contains(result, "[telegram]") {
		t.Error("should not have [telegram] section when no allowed_users")
	}
}

func TestGenerateSecretsOAuth(t *testing.T) {
	opts := SecretsOptions{
		AgentID:           "fotini",
		OAuthAccessToken:  "sk-ant-oat01-test",
		OAuthRefreshToken: "sk-ant-ort01-test",
		OAuthExpiresAt:    1772334580401,
		BotToken:          "123456789:AAF-test",
	}

	result := GenerateSecrets(opts)

	var parsed map[string]any
	if _, err := tomlParser.Decode(result, &parsed); err != nil {
		t.Fatalf("generated secrets is not valid TOML: %v\nOutput:\n%s", err, result)
	}

	if !strings.Contains(result, `oauth_access_token = "sk-ant-oat01-test"`) {
		t.Error("missing oauth_access_token")
	}
	if !strings.Contains(result, `oauth_refresh_token = "sk-ant-ort01-test"`) {
		t.Error("missing oauth_refresh_token")
	}
	if !strings.Contains(result, "oauth_expires_at = 1772334580401") {
		t.Error("missing oauth_expires_at")
	}
	if !strings.Contains(result, `[telegram.bots.fotini]`) {
		t.Error("missing telegram bot section")
	}
	if !strings.Contains(result, `token = "123456789:AAF-test"`) {
		t.Error("missing bot token")
	}
	// OAuth mode should NOT have setup_token
	if strings.Contains(result, "setup_token") {
		t.Error("OAuth mode should not include setup_token")
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
	if !strings.Contains(result, `[telegram.bots.main]`) {
		t.Error("missing telegram bot section")
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
	if !strings.Contains(result, `[telegram.bots.main]`) {
		t.Error("missing telegram bot section")
	}
}
