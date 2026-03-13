package config

import (
	"fmt"
	"strings"
)

// ResolvedModel holds the canonical resolution of a model string.
type ResolvedModel struct {
	Developer string // "anthropic", "google", "openai", "deepseek", etc.
	ModelID   string // "claude-opus-4-6", "gemini-2.5-flash", etc.
	Format    string // "anthropic", "gemini", "openai" (wire format, derived from developer)
	Endpoint  string // "anthropic", "openrouter", "google", etc.
}

// ResolveModel takes a model string (alias or developer/model_id)
// and returns the canonical resolution with wire format and endpoint.
// endpoint param is optional - if empty, auto-selects based on developer.
//
// Resolution logic:
// 1. Resolve alias: check if input exists in aliases map
// 2. Parse developer/model: split on "/" (error if no slash found)
// 3. Infer wire format from developer
// 4. Determine endpoint (explicit override, or auto-select based on developer)
func ResolveModel(input string, endpoint string, aliases map[string]string) (*ResolvedModel, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, fmt.Errorf("model string is empty")
	}

	// Step 1: Resolve alias
	resolved := input
	if len(aliases) > 0 {
		if aliasVal, ok := aliases[strings.ToLower(input)]; ok {
			resolved = aliasVal
		}
	}

	// Step 2: Parse developer/model_id
	parts := strings.SplitN(resolved, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("model must be in developer/model_id syntax (e.g., 'google/gemini-2.5-flash'), got: %q", input)
	}

	developer := strings.ToLower(strings.TrimSpace(parts[0]))
	modelID := strings.TrimSpace(parts[1])

	// Step 3: Infer wire format from developer
	format := InferWireFormat(developer)

	// Step 4: Determine endpoint
	endpointName := endpoint
	if endpointName == "" {
		// Auto-select based on developer
		switch developer {
		case "anthropic", "google", "gemini", "openai":
			endpointName = developer
			// Normalize: "google" → "gemini" endpoint name
			if endpointName == "google" {
				endpointName = "gemini"
			}
		default:
			// Third-party models default to openrouter
			endpointName = "openrouter"
		}
	}

	return &ResolvedModel{
		Developer: developer,
		ModelID:   modelID,
		Format:    format,
		Endpoint:  endpointName,
	}, nil
}

// InferWireFormat returns the wire format for a developer based on naming conventions.
// "anthropic" → "anthropic", "google"/"gemini" → "gemini", "openai" → "openai".
// Returns "openai" as the universal fallback for third-party models.
func InferWireFormat(developer string) string {
	developer = strings.ToLower(strings.TrimSpace(developer))

	switch developer {
	case "anthropic":
		return "anthropic"
	case "google", "gemini":
		return "gemini"
	case "openai":
		return "openai"
	default:
		// Universal fallback: most third-party models use OpenAI wire format
		return "openai"
	}
}

// SplitDeveloperModel splits "developer/model_id" into its parts.
// Returns ("", model) if the input contains no slash.
// This is a lower-level helper; prefer ResolveModel for full resolution.
func SplitDeveloperModel(input string) (developer, modelID string) {
	input = strings.TrimSpace(input)
	if i := strings.IndexByte(input, '/'); i > 0 {
		return input[:i], input[i+1:]
	}
	return "", input
}

// StripDeveloperPrefix removes the developer prefix from a model ID.
// Converts "developer/model_id" to "model_id", or returns the input unchanged if no slash.
func StripDeveloperPrefix(model string) string {
	_, modelID := SplitDeveloperModel(model)
	return modelID
}

// ModelCaps describes which optional API parameters a model supports.
// Used by translate layers to strip unsupported params before sending,
// avoiding 400 errors and wasted round-trips.
type ModelCaps struct {
	Effort   bool // supports output_config.effort
	Thinking bool // supports thinking (adaptive/enabled)
	Speed    bool // supports fast mode (speed: "fast")
}

// ModelCapabilities returns the capabilities of a model based on its ID.
// Accepts both bare model IDs ("claude-haiku-4-5") and developer-prefixed
// ("anthropic/claude-haiku-4-5").
func ModelCapabilities(model string) ModelCaps {
	modelID := strings.ToLower(StripDeveloperPrefix(model))

	// Anthropic models: Haiku supports neither effort nor thinking.
	// Sonnet and Opus support both.
	if strings.Contains(modelID, "claude") {
		if strings.Contains(modelID, "haiku") {
			return ModelCaps{Effort: false, Thinking: false, Speed: false}
		}
		if strings.Contains(modelID, "opus") {
			return ModelCaps{Effort: true, Thinking: true, Speed: true}
		}
		return ModelCaps{Effort: true, Thinking: true, Speed: false}
	}

	// Non-Anthropic models: effort and thinking are Anthropic-specific.
	return ModelCaps{Effort: false, Thinking: false}
}
