package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitDeveloperModelLegacy(t *testing.T) {
	tests := []struct {
		input         string
		wantDeveloper string
		wantModel     string
	}{
		{"anthropic/claude-haiku-4-5", "anthropic", "claude-haiku-4-5"},
		{"google/gemini-2.5-flash", "google", "gemini-2.5-flash"},
		{"openai/gpt-4o", "openai", "gpt-4o"},
		// Bare model name returns empty developer
		{"claude-haiku-4-5", "", "claude-haiku-4-5"},
		// Whitespace trimming
		{"  anthropic/claude-haiku-4-5  ", "anthropic", "claude-haiku-4-5"},
		// Empty input
		{"", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			dev, model := SplitDeveloperModel(tt.input)
			if dev != tt.wantDeveloper {
				t.Errorf("developer = %q, want %q", dev, tt.wantDeveloper)
			}
			if model != tt.wantModel {
				t.Errorf("model = %q, want %q", model, tt.wantModel)
			}
		})
	}
}

func TestInferFormat(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"claude-haiku-4-5", "anthropic"},
		{"claude-opus-4-6", "anthropic"},
		{"claude-sonnet-4-5-20250929", "anthropic"},
		{"gemini-2.5-flash", "gemini"},
		{"gemini-2.5-pro", "gemini"},
		{"gpt-4o", "openai"},
		{"gpt-4o-mini", "openai"},
		{"o3", "openai"},
		{"o3-mini", "openai"},
		{"o4-mini", "openai"},
		{"o1", "openai"},
		{"chatgpt-4o-latest", "openai"},
		// Unknown model falls back to openai
		{"llama-3-70b", "openai"},
		{"mistral-large", "openai"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := InferFormat(tt.model)
			if got != tt.want {
				t.Errorf("InferFormat(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

func TestEndpointConfig_SupportsFormat(t *testing.T) {
	// Single-format endpoint
	single := EndpointConfig{Format: "anthropic"}
	if !single.SupportsFormat("anthropic") {
		t.Error("single-format endpoint should support its format")
	}
	if single.SupportsFormat("openai") {
		t.Error("single-format endpoint should not support other formats")
	}

	// Multi-format endpoint (like openrouter)
	multi := EndpointConfig{
		AnthropicURL: "https://openrouter.ai/api/v1",
		OpenAIURL:    "https://openrouter.ai/api/v1",
	}
	if !multi.SupportsFormat("anthropic") {
		t.Error("multi-format endpoint should support anthropic")
	}
	if !multi.SupportsFormat("openai") {
		t.Error("multi-format endpoint should support openai")
	}
	if multi.SupportsFormat("gemini") {
		t.Error("multi-format endpoint without gemini_url should not support gemini")
	}
}

func TestEndpointConfig_URLForFormat(t *testing.T) {
	ep := EndpointConfig{
		URL:          "https://default.example.com",
		AnthropicURL: "https://anthropic.example.com",
	}

	if got := ep.URLForFormat("anthropic"); got != "https://anthropic.example.com" {
		t.Errorf("URLForFormat(anthropic) = %q, want anthropic URL", got)
	}
	if got := ep.URLForFormat("openai"); got != "https://default.example.com" {
		t.Errorf("URLForFormat(openai) = %q, want fallback URL", got)
	}

	// No format-specific URL and no default
	empty := EndpointConfig{}
	if got := empty.URLForFormat("anthropic"); got != "" {
		t.Errorf("URLForFormat on empty = %q, want empty", got)
	}
}

func TestEndpointDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Built-in endpoints must be populated
	for _, name := range []string{"anthropic", "gemini", "openai", "openrouter"} {
		if _, ok := cfg.Endpoints[name]; !ok {
			t.Errorf("missing default endpoint %q", name)
		}
	}

	// Check format fields
	if cfg.Endpoints["anthropic"].Format != "anthropic" {
		t.Errorf("anthropic endpoint format = %q", cfg.Endpoints["anthropic"].Format)
	}
	if cfg.Endpoints["gemini"].Format != "gemini" {
		t.Errorf("gemini endpoint format = %q", cfg.Endpoints["gemini"].Format)
	}
	if cfg.Endpoints["openai"].Format != "openai" {
		t.Errorf("openai endpoint format = %q", cfg.Endpoints["openai"].Format)
	}

	// OpenRouter should have multi-format URLs
	or := cfg.Endpoints["openrouter"]
	if or.AnthropicURL == "" {
		t.Error("openrouter missing anthropic_url")
	}
	if or.OpenAIURL == "" {
		t.Error("openrouter missing openai_url")
	}
}

func TestEndpointUserOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"

[endpoints.local]
format = "openai"
url = "http://localhost:8080/v1"
api_key = "local.api_key"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	local, ok := cfg.Endpoints["local"]
	if !ok {
		t.Fatal("missing user-defined endpoint 'local'")
	}
	if local.Format != "openai" {
		t.Errorf("local format = %q, want openai", local.Format)
	}
	if local.URL != "http://localhost:8080/v1" {
		t.Errorf("local url = %q", local.URL)
	}
	if local.APIKey != "local.api_key" {
		t.Errorf("local api_key = %q", local.APIKey)
	}

	// Built-in defaults should still exist
	if _, ok := cfg.Endpoints["anthropic"]; !ok {
		t.Error("built-in anthropic endpoint missing after user override")
	}
}

func TestModelMigrationAddsEndpointPrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	// Bare model name (no endpoint prefix) should be auto-migrated
	toml := `
[agent]
id = "test"
model = "anthropic/claude-opus-4-6"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agent.Model != "anthropic/claude-opus-4-6" {
		t.Errorf("Agent.Model = %q, want %q (should be migrated)", cfg.Agent.Model, "anthropic/claude-opus-4-6")
	}
}

func TestModelValidationRejectsColonFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"
model = "anthropic:claude-haiku-4-5"
`
	os.WriteFile(path, []byte(toml), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for colon format, got nil")
	}
	if !strings.Contains(err.Error(), "developer/model_id") {
		t.Errorf("error = %q, want to contain 'developer/model_id'", err.Error())
	}
	// Should suggest the corrected format
	if !strings.Contains(err.Error(), "anthropic/claude-haiku-4-5") {
		t.Errorf("error = %q, want to contain suggested format 'anthropic/claude-haiku-4-5'", err.Error())
	}
}

func TestModelValidationRejectsInvalidFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"

[endpoints.bad]
format = "grpc"
`
	os.WriteFile(path, []byte(toml), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid format, got nil")
	}
	if !strings.Contains(err.Error(), "format") {
		t.Errorf("error = %q, want to contain 'format'", err.Error())
	}
}

func TestAliasDefaultsIncludeEndpointPrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Default aliases should include endpoint prefixes
	tests := []struct {
		alias string
		want  string
	}{
		{"opus", "anthropic/claude-opus-4-6"},
		{"sonnet", "anthropic/claude-sonnet-4-6"},
		{"haiku", "anthropic/claude-haiku-4-5-20251001"},
		{"flash", "google/gemini-2.5-flash"},
		{"pro", "google/gemini-2.5-pro"},
	}
	for _, tt := range tests {
		got := cfg.Models.Aliases[tt.alias]
		if got != tt.want {
			t.Errorf("alias %q = %q, want %q", tt.alias, got, tt.want)
		}
	}
}

func TestHasBackend(t *testing.T) {
	tests := []struct {
		name     string
		backends []string
		search   string
		want     bool
	}{
		{"found", []string{"milvus", "sqlite"}, "milvus", true},
		{"not found", []string{"milvus", "sqlite"}, "pgvector", false},
		{"empty list", []string{}, "milvus", false},
		{"case sensitive", []string{"Milvus"}, "milvus", false},
		{"multiple matches", []string{"a", "b", "c"}, "b", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := MemoryConfig{SearchBackends: tt.backends}
			got := cfg.HasBackend(tt.search)
			if got != tt.want {
				t.Errorf("HasBackend(%q) = %v, want %v", tt.search, got, tt.want)
			}
		})
	}
}

func TestURLForFormat(t *testing.T) {
	tests := []struct {
		name     string
		endpoint EndpointConfig
		format   string
		want     string
	}{
		{
			name: "anthropic url set",
			endpoint: EndpointConfig{
				URL:           "https://default.com",
				AnthropicURL:  "https://anthropic.com",
				OpenAIURL:     "https://openai.com",
			},
			format: "anthropic",
			want:   "https://anthropic.com",
		},
		{
			name: "anthropic no specific url fallback",
			endpoint: EndpointConfig{
				URL: "https://default.com",
			},
			format: "anthropic",
			want:   "https://default.com",
		},
		{
			name: "openai url set",
			endpoint: EndpointConfig{
				URL:       "https://default.com",
				OpenAIURL: "https://openai.com",
			},
			format: "openai",
			want:   "https://openai.com",
		},
		{
			name: "gemini url set",
			endpoint: EndpointConfig{
				URL:       "https://default.com",
				GeminiURL: "https://gemini.com",
			},
			format: "gemini",
			want:   "https://gemini.com",
		},
		{
			name: "unknown format returns default",
			endpoint: EndpointConfig{
				URL: "https://default.com",
			},
			format: "unknown",
			want:   "https://default.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.endpoint.URLForFormat(tt.format)
			if got != tt.want {
				t.Errorf("URLForFormat(%q) = %q, want %q", tt.format, got, tt.want)
			}
		})
	}
}

func TestSupportsFormat(t *testing.T) {
	tests := []struct {
		name     string
		endpoint EndpointConfig
		format   string
		want     bool
	}{
		{
			name: "anthropic via explicit url",
			endpoint: EndpointConfig{
				AnthropicURL: "https://anthropic.com",
			},
			format: "anthropic",
			want:   true,
		},
		{
			name: "anthropic via format field",
			endpoint: EndpointConfig{
				Format: "anthropic",
			},
			format: "anthropic",
			want:   true,
		},
		{
			name: "openai via explicit url",
			endpoint: EndpointConfig{
				OpenAIURL: "https://openai.com",
			},
			format: "openai",
			want:   true,
		},
		{
			name: "openai via format field",
			endpoint: EndpointConfig{
				Format: "openai",
			},
			format: "openai",
			want:   true,
		},
		{
			name: "gemini via explicit url",
			endpoint: EndpointConfig{
				GeminiURL: "https://gemini.com",
			},
			format: "gemini",
			want:   true,
		},
		{
			name: "gemini via format field",
			endpoint: EndpointConfig{
				Format: "gemini",
			},
			format: "gemini",
			want:   true,
		},
		{
			name: "format not supported",
			endpoint: EndpointConfig{
				Format: "anthropic",
			},
			format: "openai",
			want:   false,
		},
		{
			name: "unknown format",
			endpoint: EndpointConfig{
				Format: "anthropic",
			},
			format: "unknown",
			want:   false,
		},
		{
			name: "empty endpoint",
			endpoint: EndpointConfig{},
			format:   "anthropic",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.endpoint.SupportsFormat(tt.format)
			if got != tt.want {
				t.Errorf("SupportsFormat(%q) = %v, want %v", tt.format, got, tt.want)
			}
		})
	}
}
