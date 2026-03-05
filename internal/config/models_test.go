package config

import (
	"testing"
)

func TestResolveModel(t *testing.T) {
	aliases := map[string]string{
		"opus":     "anthropic/claude-opus-4-6",
		"sonnet":   "anthropic/claude-sonnet-4-6",
		"haiku":    "anthropic/claude-haiku-4-5",
		"flash":    "google/gemini-2.5-flash",
		"pro":      "google/gemini-2.5-pro",
		"gpt4o":    "openai/gpt-4o",
		"deepseek": "deepseek/deepseek-chat",
	}

	tests := []struct {
		name            string
		input           string
		endpoint        string
		aliases         map[string]string
		wantDeveloper   string
		wantModelID     string
		wantFormat      string
		wantEndpoint    string
		wantErr         bool
		wantErrContains string
	}{
		// Alias resolution
		{
			name:          "alias opus",
			input:         "opus",
			endpoint:      "",
			aliases:       aliases,
			wantDeveloper: "anthropic",
			wantModelID:   "claude-opus-4-6",
			wantFormat:    "anthropic",
			wantEndpoint:  "anthropic",
		},
		{
			name:          "alias flash",
			input:         "flash",
			endpoint:      "",
			aliases:       aliases,
			wantDeveloper: "google",
			wantModelID:   "gemini-2.5-flash",
			wantFormat:    "gemini",
			wantEndpoint:  "gemini",
		},
		{
			name:          "alias deepseek",
			input:         "deepseek",
			endpoint:      "",
			aliases:       aliases,
			wantDeveloper: "deepseek",
			wantModelID:   "deepseek-chat",
			wantFormat:    "openai",
			wantEndpoint:  "openrouter",
		},

		// Direct syntax
		{
			name:          "direct anthropic",
			input:         "anthropic/claude-haiku-4-5",
			endpoint:      "",
			aliases:       aliases,
			wantDeveloper: "anthropic",
			wantModelID:   "claude-haiku-4-5",
			wantFormat:    "anthropic",
			wantEndpoint:  "anthropic",
		},
		{
			name:          "direct google",
			input:         "google/gemini-2.5-flash",
			endpoint:      "",
			aliases:       aliases,
			wantDeveloper: "google",
			wantModelID:   "gemini-2.5-flash",
			wantFormat:    "gemini",
			wantEndpoint:  "gemini",
		},
		{
			name:          "direct gemini developer",
			input:         "gemini/gemini-2.5-pro",
			endpoint:      "",
			aliases:       aliases,
			wantDeveloper: "gemini",
			wantModelID:   "gemini-2.5-pro",
			wantFormat:    "gemini",
			wantEndpoint:  "gemini",
		},
		{
			name:          "direct openai",
			input:         "openai/gpt-4o",
			endpoint:      "",
			aliases:       aliases,
			wantDeveloper: "openai",
			wantModelID:   "gpt-4o",
			wantFormat:    "openai",
			wantEndpoint:  "openai",
		},
		{
			name:          "third-party model defaults to openrouter",
			input:         "deepseek/deepseek-chat",
			endpoint:      "",
			aliases:       aliases,
			wantDeveloper: "deepseek",
			wantModelID:   "deepseek-chat",
			wantFormat:    "openai",
			wantEndpoint:  "openrouter",
		},

		// Explicit endpoint override
		{
			name:          "override to openrouter",
			input:         "anthropic/claude-opus-4-6",
			endpoint:      "openrouter",
			aliases:       aliases,
			wantDeveloper: "anthropic",
			wantModelID:   "claude-opus-4-6",
			wantFormat:    "anthropic",
			wantEndpoint:  "openrouter",
		},
		{
			name:          "override to custom endpoint",
			input:         "google/gemini-2.5-flash",
			endpoint:      "mycustom",
			aliases:       aliases,
			wantDeveloper: "google",
			wantModelID:   "gemini-2.5-flash",
			wantFormat:    "gemini",
			wantEndpoint:  "mycustom",
		},

		// Error cases
		{
			name:            "empty input",
			input:           "",
			endpoint:        "",
			aliases:         aliases,
			wantErr:         true,
			wantErrContains: "empty",
		},
		{
			name:            "no slash",
			input:           "claude-haiku-4-5",
			endpoint:        "",
			aliases:         aliases,
			wantErr:         true,
			wantErrContains: "developer/model_id syntax",
		},
		{
			name:            "trailing slash",
			input:           "anthropic/",
			endpoint:        "",
			aliases:         aliases,
			wantErr:         true,
			wantErrContains: "developer/model_id syntax",
		},
		{
			name:            "leading slash",
			input:           "/claude-haiku-4-5",
			endpoint:        "",
			aliases:         aliases,
			wantErr:         true,
			wantErrContains: "developer/model_id syntax",
		},

		// Case normalization
		{
			name:          "uppercase developer",
			input:         "ANTHROPIC/claude-haiku-4-5",
			endpoint:      "",
			aliases:       aliases,
			wantDeveloper: "anthropic",
			wantModelID:   "claude-haiku-4-5",
			wantFormat:    "anthropic",
			wantEndpoint:  "anthropic",
		},
		{
			name:          "mixed case alias",
			input:         "OPUS",
			endpoint:      "",
			aliases:       aliases,
			wantDeveloper: "anthropic",
			wantModelID:   "claude-opus-4-6",
			wantFormat:    "anthropic",
			wantEndpoint:  "anthropic",
		},

		// No aliases map
		{
			name:          "no aliases direct syntax",
			input:         "anthropic/claude-haiku-4-5",
			endpoint:      "",
			aliases:       nil,
			wantDeveloper: "anthropic",
			wantModelID:   "claude-haiku-4-5",
			wantFormat:    "anthropic",
			wantEndpoint:  "anthropic",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveModel(tt.input, tt.endpoint, tt.aliases)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ResolveModel() expected error, got nil")
					return
				}
				if tt.wantErrContains != "" && !contains(err.Error(), tt.wantErrContains) {
					t.Errorf("ResolveModel() error = %v, want substring %q", err, tt.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Errorf("ResolveModel() unexpected error: %v", err)
				return
			}
			if got.Developer != tt.wantDeveloper {
				t.Errorf("ResolveModel() Developer = %v, want %v", got.Developer, tt.wantDeveloper)
			}
			if got.ModelID != tt.wantModelID {
				t.Errorf("ResolveModel() ModelID = %v, want %v", got.ModelID, tt.wantModelID)
			}
			if got.Format != tt.wantFormat {
				t.Errorf("ResolveModel() Format = %v, want %v", got.Format, tt.wantFormat)
			}
			if got.Endpoint != tt.wantEndpoint {
				t.Errorf("ResolveModel() Endpoint = %v, want %v", got.Endpoint, tt.wantEndpoint)
			}
		})
	}
}

func TestInferWireFormat(t *testing.T) {
	tests := []struct {
		developer string
		want      string
	}{
		{"anthropic", "anthropic"},
		{"ANTHROPIC", "anthropic"},
		{"google", "gemini"},
		{"GOOGLE", "gemini"},
		{"gemini", "gemini"},
		{"openai", "openai"},
		{"OpenAI", "openai"},
		{"deepseek", "openai"},
		{"mistral", "openai"},
		{"unknown", "openai"},
		{"", "openai"},
	}

	for _, tt := range tests {
		t.Run(tt.developer, func(t *testing.T) {
			got := InferWireFormat(tt.developer)
			if got != tt.want {
				t.Errorf("InferWireFormat(%q) = %v, want %v", tt.developer, got, tt.want)
			}
		})
	}
}

func TestSplitDeveloperModel(t *testing.T) {
	tests := []struct {
		input           string
		wantDeveloper   string
		wantModelID     string
	}{
		{"anthropic/claude-opus-4-6", "anthropic", "claude-opus-4-6"},
		{"google/gemini-2.5-flash", "google", "gemini-2.5-flash"},
		{"openai/gpt-4o", "openai", "gpt-4o"},
		{"deepseek/deepseek-chat", "deepseek", "deepseek-chat"},
		{"claude-haiku-4-5", "", "claude-haiku-4-5"},
		{"gpt-4o", "", "gpt-4o"},
		{"", "", ""},
		{"/model", "", "/model"},
		{"developer/", "developer", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			gotDeveloper, gotModelID := SplitDeveloperModel(tt.input)
			if gotDeveloper != tt.wantDeveloper {
				t.Errorf("SplitDeveloperModel(%q) developer = %v, want %v", tt.input, gotDeveloper, tt.wantDeveloper)
			}
			if gotModelID != tt.wantModelID {
				t.Errorf("SplitDeveloperModel(%q) modelID = %v, want %v", tt.input, gotModelID, tt.wantModelID)
			}
		})
	}
}

func TestStripDeveloperPrefix(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"anthropic/claude-opus-4-6", "claude-opus-4-6"},
		{"anthropic/claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001"},
		{"google/gemini-2.5-flash", "gemini-2.5-flash"},
		{"google/gemini-2.5-pro", "gemini-2.5-pro"},
		{"openai/gpt-4o", "gpt-4o"},
		{"openai/o3", "o3"},
		{"deepseek/deepseek-chat", "deepseek-chat"},
		{"claude-opus-4-6", "claude-opus-4-6"},       // no prefix
		{"gpt-4o", "gpt-4o"},                         // no prefix
		{"gemini-2.5-flash", "gemini-2.5-flash"},     // no prefix
		{"", ""},                                     // empty
		{"no-slash-here", "no-slash-here"},           // no slash
		{"/model", "/model"},                         // leading slash (no developer part)
		{"foo/bar/baz", "bar/baz"},                   // slash in middle (should strip first part only)
		{"developer/", ""},                           // trailing slash
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := StripDeveloperPrefix(tt.input)
			if got != tt.expected {
				t.Errorf("StripDeveloperPrefix(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || (len(s) > 0 && len(substr) > 0 && hasSubstring(s, substr)))
}

func hasSubstring(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
