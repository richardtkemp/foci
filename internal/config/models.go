package config

import (
	"fmt"
	"strings"

	"foci/internal/modelinfo"
)

// ResolvedModel holds the canonical resolution of a model string.
type ResolvedModel struct {
	Developer       string // "anthropic", "google", "openai", "deepseek", etc.
	ModelID         string // "claude-opus-4-6", "gemini-2.5-flash", etc.
	Format          string // "anthropic", "gemini", "openai" (wire format, derived from developer)
	Endpoint        string // "anthropic", "openrouter", "google", etc.
	EnableKeepalive *bool  // nil=auto-detect, true/false=explicit
	PromptCacheTTL  string // Go duration, empty=auto-detect
}

// ResolveModel takes a model string in developer/model_id format
// and returns the canonical resolution with wire format and endpoint.
// endpoint param is optional - if empty, auto-selects based on developer.
//
// Resolution logic:
// 1. Parse developer/model: split on "/" (error if no slash found)
// 2. Infer wire format from developer
// 3. Determine endpoint (explicit override, or auto-select based on developer)
func ResolveModel(input string, endpoint string) (*ResolvedModel, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, fmt.Errorf("model string is empty")
	}

	// Step 1: Parse developer/model_id
	parts := strings.SplitN(input, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("model must be in developer/model_id syntax (e.g., 'google/gemini-2.5-flash'), got: %q", input)
	}

	developer := strings.ToLower(strings.TrimSpace(parts[0]))
	modelID := strings.TrimSpace(parts[1])

	// Step 2: Infer wire format from developer
	format := InferWireFormat(developer)

	// Step 3: Determine endpoint
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
	effort, thinking, speed := modelinfo.Capabilities(model)
	return ModelCaps{Effort: effort, Thinking: thinking, Speed: speed}
}
