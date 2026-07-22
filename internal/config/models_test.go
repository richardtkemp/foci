package config

import (
	"testing"

	tomlParser "github.com/BurntSushi/toml"
)

func TestResolveModel(t *testing.T) {
	// Proves that ResolveModel correctly handles developer/model_id syntax,
	// endpoint overrides, case normalization, and error cases for malformed
	// or empty input.
	tests := []struct {
		name            string
		input           string
		endpoint        string
		wantDeveloper   string
		wantModelID     string
		wantFormat      string
		wantEndpoint    string
		wantErr         bool
		wantErrContains string
	}{
		// Direct syntax
		{
			name:          "direct anthropic",
			input:         "anthropic/claude-haiku-4-5",
			endpoint:      "",
			wantDeveloper: "anthropic",
			wantModelID:   "claude-haiku-4-5",
			wantFormat:    "anthropic",
			wantEndpoint:  "anthropic",
		},
		{
			name:          "direct google",
			input:         "google/gemini-2.5-flash",
			endpoint:      "",
			wantDeveloper: "google",
			wantModelID:   "gemini-2.5-flash",
			wantFormat:    "gemini",
			wantEndpoint:  "gemini",
		},
		{
			name:          "direct gemini developer",
			input:         "gemini/gemini-2.5-pro",
			endpoint:      "",
			wantDeveloper: "gemini",
			wantModelID:   "gemini-2.5-pro",
			wantFormat:    "gemini",
			wantEndpoint:  "gemini",
		},
		{
			name:          "direct openai",
			input:         "openai/gpt-4o",
			endpoint:      "",
			wantDeveloper: "openai",
			wantModelID:   "gpt-4o",
			wantFormat:    "openai",
			wantEndpoint:  "openai",
		},
		{
			name:          "third-party model defaults to openrouter",
			input:         "deepseek/deepseek-chat",
			endpoint:      "",
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
			wantDeveloper: "anthropic",
			wantModelID:   "claude-opus-4-6",
			wantFormat:    "anthropic",
			wantEndpoint:  "openrouter",
		},
		{
			name:          "override to custom endpoint",
			input:         "google/gemini-2.5-flash",
			endpoint:      "mycustom",
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
			wantErr:         true,
			wantErrContains: "empty",
		},
		{
			name:            "no slash",
			input:           "claude-haiku-4-5",
			endpoint:        "",
			wantErr:         true,
			wantErrContains: "developer/model_id syntax",
		},
		{
			name:            "trailing slash",
			input:           "anthropic/",
			endpoint:        "",
			wantErr:         true,
			wantErrContains: "developer/model_id syntax",
		},
		{
			name:            "leading slash",
			input:           "/claude-haiku-4-5",
			endpoint:        "",
			wantErr:         true,
			wantErrContains: "developer/model_id syntax",
		},

		// Case normalization
		{
			name:          "uppercase developer",
			input:         "ANTHROPIC/claude-haiku-4-5",
			endpoint:      "",
			wantDeveloper: "anthropic",
			wantModelID:   "claude-haiku-4-5",
			wantFormat:    "anthropic",
			wantEndpoint:  "anthropic",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveModel(tt.input, tt.endpoint, nil)
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
	// Proves that InferWireFormat maps anthropic->anthropic, google/gemini->gemini,
	// and everything else (including openai, deepseek, unknown) to openai format,
	// case-insensitively.
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
	// Proves that SplitDeveloperModel splits on the first slash and handles edge
	// cases: no slash returns empty developer, leading slash returns empty developer,
	// and empty string returns both empty.
	tests := []struct {
		input         string
		wantDeveloper string
		wantModelID   string
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
	// Proves that StripDeveloperPrefix removes the "developer/" prefix from model
	// strings, handles no-slash and leading-slash edge cases, and strips only the
	// first component when multiple slashes are present.
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
		{"claude-opus-4-6", "claude-opus-4-6"},   // no prefix
		{"gpt-4o", "gpt-4o"},                     // no prefix
		{"gemini-2.5-flash", "gemini-2.5-flash"}, // no prefix
		{"", ""},                                 // empty
		{"no-slash-here", "no-slash-here"},       // no slash
		{"/model", "/model"},                     // leading slash (no developer part)
		{"foo/bar/baz", "bar/baz"},               // slash in middle (should strip first part only)
		{"developer/", ""},                       // trailing slash
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

func TestModelCapabilities(t *testing.T) {
	// Proves that ModelCapabilities correctly reports effort and thinking support per
	// model family: sonnet/opus support both, haiku supports neither, and non-Anthropic
	// models support neither.
	t.Parallel()
	tests := []struct {
		model        string
		wantEffort   bool
		wantThinking bool
	}{
		{"claude-sonnet-4-6", true, true},
		{"claude-opus-4-6", true, true},
		{"anthropic/claude-sonnet-4-6", true, true},
		{"anthropic/claude-opus-4-6", true, true},
		{"claude-haiku-4-5", false, false},
		{"claude-haiku-4-5-20251001", false, false},
		{"anthropic/claude-haiku-4-5-20251001", false, false},
		{"ANTHROPIC/CLAUDE-HAIKU-4-5", false, false},
		{"gemini-2.5-flash", false, false},
		{"gpt-4o", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			caps := ModelCapabilities(tt.model)
			if caps.Effort != tt.wantEffort {
				t.Errorf("ModelCapabilities(%q).Effort = %v, want %v", tt.model, caps.Effort, tt.wantEffort)
			}
			if caps.Thinking != tt.wantThinking {
				t.Errorf("ModelCapabilities(%q).Thinking = %v, want %v", tt.model, caps.Thinking, tt.wantThinking)
			}
		})
	}
}

func TestModelStringConfigToWire(t *testing.T) {
	// Proves end-to-end that a model string from config produces the correct
	// wire model ID after ResolveModel + reconstitution + StripDeveloperPrefix.
	// This is the chain that all provider clients use before sending to the API.
	// The critical case is 3-segment OpenRouter models like
	// "openrouter/stepfun/step-3.5-flash" where the developer prefix is "openrouter"
	// and the remaining "stepfun/step-3.5-flash" is the OpenRouter model ID.
	t.Parallel()
	tests := []struct {
		name     string
		config   string // model string from TOML config
		wantWire string // expected model in API request body
	}{
		{"anthropic native", "anthropic/claude-opus-4-6", "claude-opus-4-6"},
		{"openai native", "openai/gpt-4o", "gpt-4o"},
		{"google native", "google/gemini-2.5-flash", "gemini-2.5-flash"},
		{"third-party auto-routed", "deepseek/deepseek-chat", "deepseek-chat"},
		{"openrouter 3-segment", "openrouter/stepfun/step-3.5-flash", "stepfun/step-3.5-flash"},
		{"openrouter anthropic model", "openrouter/anthropic/claude-opus-4-6", "anthropic/claude-opus-4-6"},
		{"openrouter deepseek model", "openrouter/deepseek/deepseek-r1", "deepseek/deepseek-r1"},
		// ":floor"/":nitro" are OpenRouter's own routing shortcuts (equivalent
		// to provider.sort: "price"/"throughput"). They need NO special
		// parsing here — the suffix rides along as part of the model leaf and
		// is forwarded verbatim (#1478 locks this in as intentional).
		{"openrouter :floor shortcut", "openrouter/deepseek/deepseek-v4-pro:floor", "deepseek/deepseek-v4-pro:floor"},
		{"openrouter :nitro shortcut", "openrouter/moonshotai/kimi-k2.5:nitro", "moonshotai/kimi-k2.5:nitro"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := ResolveModel(tt.config, "", nil)
			if err != nil {
				t.Fatalf("ResolveModel(%q): %v", tt.config, err)
			}

			// Reconstitute the full model string (as agents.go does)
			fullModel := resolved.Developer + "/" + resolved.ModelID

			// Strip prefix (as buildParams/buildSDKParams does)
			wireModel := StripDeveloperPrefix(fullModel)

			if wireModel != tt.wantWire {
				t.Errorf("config %q → resolve → reconstitute %q → strip → %q, want %q",
					tt.config, fullModel, wireModel, tt.wantWire)
			}
		})
	}
}

func TestModelConfigProviderRoutingTOML(t *testing.T) {
	// Proves that a [models.X.provider] sub-table round-trips through TOML
	// into the shared provider.ProviderRouting type (internal/provider),
	// including a nested inline table for sort and a sub-table for max_price.
	tomlData := `
[models.deepseek]
model = "openrouter/deepseek/deepseek-v4-pro:floor"

[models.deepseek.provider]
order = ["deepinfra", "novita"]
allow_fallbacks = false
data_collection = "deny"
quantizations = ["fp8", "fp16"]
sort = {by = "price"}

[models.deepseek.provider.max_price]
prompt = 1.0
completion = 2.0
`
	var cfg Config
	if _, err := tomlParser.Decode(tomlData, &cfg); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	mc, ok := cfg.Models["deepseek"]
	if !ok {
		t.Fatal("expected [models.deepseek] to be present")
	}
	pr := mc.Provider
	if pr == nil {
		t.Fatal("expected Provider to be populated")
	}
	if len(pr.Order) != 2 || pr.Order[0] != "deepinfra" || pr.Order[1] != "novita" {
		t.Errorf("Order = %v, want [deepinfra novita]", pr.Order)
	}
	if pr.AllowFallbacks == nil || *pr.AllowFallbacks != false {
		t.Errorf("AllowFallbacks = %v, want false", pr.AllowFallbacks)
	}
	if pr.DataCollection != "deny" {
		t.Errorf("DataCollection = %q, want deny", pr.DataCollection)
	}
	if len(pr.Quantizations) != 2 || pr.Quantizations[0] != "fp8" {
		t.Errorf("Quantizations = %v, want [fp8 fp16]", pr.Quantizations)
	}
	if pr.Sort == nil || pr.Sort.By != "price" {
		t.Fatalf("Sort = %+v, want {By: price}", pr.Sort)
	}
	if pr.MaxPrice == nil || pr.MaxPrice.Prompt != 1.0 || pr.MaxPrice.Completion != 2.0 {
		t.Fatalf("MaxPrice = %+v, want {Prompt: 1.0, Completion: 2.0}", pr.MaxPrice)
	}
}

func TestModelConfigProviderRoutingAbsentByDefault(t *testing.T) {
	// Proves that a [models.X] entry with no [models.X.provider] sub-table
	// leaves Provider nil — no accidental zero-value provider object gets
	// forwarded to the wire for models that don't configure routing.
	tomlData := `
[models.plain]
model = "openrouter/stepfun/step-3.5-flash:nitro"
`
	var cfg Config
	if _, err := tomlParser.Decode(tomlData, &cfg); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	mc, ok := cfg.Models["plain"]
	if !ok {
		t.Fatal("expected [models.plain] to be present")
	}
	if mc.Provider != nil {
		t.Errorf("Provider = %+v, want nil", mc.Provider)
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
