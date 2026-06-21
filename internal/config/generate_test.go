package config

import (
	"strings"
	"testing"

	tomlParser "github.com/BurntSushi/toml"
)

func TestGenerateConfig(t *testing.T) {
	// Proves GenerateConfig produces valid TOML with groups, models, and
	// agent block sections.
	agentBlock := `[[agents]]
id = "fotini"
workspace = "/home/foci/fotini"

[agents.system]
system_files = ["character/SOUL.md", "character/CRAFT.md"]
`
	opts := SetupOptions{
		Model:      "anthropic/claude-sonnet-4-6",
		AgentBlock: agentBlock,
	}

	result := GenerateConfig(opts)

	// Must be valid TOML
	var parsed map[string]any
	if _, err := tomlParser.Decode(result, &parsed); err != nil {
		t.Fatalf("generated config is not valid TOML: %v\nOutput:\n%s", err, result)
	}

	if !strings.Contains(result, `powerful = "default"`) {
		t.Error("missing groups.powerful")
	}
	if !strings.Contains(result, `[models.default]`) {
		t.Error("missing [models.default] section")
	}
	if !strings.Contains(result, `model = "anthropic/claude-sonnet-4-6"`) {
		t.Error("missing model in [models.default]")
	}
	if !strings.Contains(result, `id = "fotini"`) {
		t.Error("missing agent id")
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
	// Proves GenerateConfig works with no model, producing valid TOML
	// without groups/models sections.
	agentBlock := `[[agents]]
id = "main"
workspace = "/home/foci/main"
`
	opts := SetupOptions{
		AgentBlock: agentBlock,
	}
	result := GenerateConfig(opts)

	var parsed map[string]any
	if _, err := tomlParser.Decode(result, &parsed); err != nil {
		t.Fatalf("minimal config is not valid TOML: %v\nOutput:\n%s", err, result)
	}

	if !strings.Contains(result, `id = "main"`) {
		t.Error("missing agent id")
	}
	if strings.Contains(result, "[groups]") {
		t.Error("should not have groups section when model is empty")
	}
	if strings.Contains(result, "[models") {
		t.Error("should not have models section when model is empty")
	}
}

func TestGenerateConfigWithEndpoint(t *testing.T) {
	// Proves endpoint override appears in [models.default] when set.
	opts := SetupOptions{
		Model:    "anthropic/claude-sonnet-4-6",
		Endpoint: "openrouter",
		AgentBlock: `[[agents]]
id = "main"
`,
	}

	result := GenerateConfig(opts)

	var parsed map[string]any
	if _, err := tomlParser.Decode(result, &parsed); err != nil {
		t.Fatalf("generated config is not valid TOML: %v\nOutput:\n%s", err, result)
	}

	if !strings.Contains(result, `endpoint = "openrouter"`) {
		t.Error("missing endpoint in [models.default]")
	}
}

func TestGenerateConfigWithCustomEndpoint(t *testing.T) {
	// Proves custom endpoint section is generated.
	opts := SetupOptions{
		Model:    "openai/my-model",
		Endpoint: "local",
		AgentBlock: `[[agents]]
id = "main"
`,
		CustomEndpoint: &CustomEndpointSetup{
			Name:      "local",
			URL:       "http://localhost:8000/v1",
			Format:    "openai",
			SecretKey: "local.api_key",
		},
	}

	result := GenerateConfig(opts)

	var parsed map[string]any
	if _, err := tomlParser.Decode(result, &parsed); err != nil {
		t.Fatalf("generated config is not valid TOML: %v\nOutput:\n%s", err, result)
	}

	if !strings.Contains(result, `[endpoints.local]`) {
		t.Error("missing [endpoints.local] section")
	}
	if !strings.Contains(result, `format = "openai"`) {
		t.Error("missing format in custom endpoint")
	}
	if !strings.Contains(result, `url = "http://localhost:8000/v1"`) {
		t.Error("missing url in custom endpoint")
	}
	if !strings.Contains(result, `api_key = "local.api_key"`) {
		t.Error("missing api_key in custom endpoint")
	}
}

func TestGenerateConfig_LocalBackendOmitsGroupsAndEndpoint(t *testing.T) {
	// A claude-code (local/delegated) agent routes everything through the
	// backend, so first-run sets Model="" and GenerateConfig must emit NO
	// [groups]/[models.default] and no anthropic reference at all. This pins the
	// generation half of the fix for the spurious startup "no Anthropic
	// credentials" error (the load half is in claude_code_only_test.go).
	agentBlock := `[[agents]]
id = "cctest"
backend = "claude-code"

[agents.backend_config]
model = "sonnet"
`
	result := GenerateConfig(SetupOptions{Model: "", AgentBlock: agentBlock})

	var parsed map[string]any
	if _, err := tomlParser.Decode(result, &parsed); err != nil {
		t.Fatalf("generated config is not valid TOML: %v\nOutput:\n%s", err, result)
	}
	for _, banned := range []string{"[groups]", "[models.default]", "powerful =", "[endpoints", "anthropic"} {
		if strings.Contains(result, banned) {
			t.Errorf("local-backend config must not contain %q\nOutput:\n%s", banned, result)
		}
	}
	if !strings.Contains(result, `backend = "claude-code"`) {
		t.Errorf("agent block missing from generated config\nOutput:\n%s", result)
	}
}
