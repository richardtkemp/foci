package config

import (
	"fmt"
	"strings"

	"foci/internal/modelinfo"
)

// ResolvedModel holds the canonical resolution of a model string.
type ResolvedModel struct {
	Developer string // "anthropic", "google", "openai", "deepseek", etc.
	ModelID   string // "claude-opus-4-6", "gemini-2.5-flash", etc.
	Format    string // "anthropic", "gemini", "openai" (wire format, derived from developer)
	Endpoint  string // "anthropic", "openrouter", "google", etc.
	// From ModelConfig (populated when resolved via a named model entry)
	Thinking        string // "adaptive", "off", or ""
	Effort          string // "low", "medium", "high", or ""
	Speed           string // "fast" or ""
	Context         int    // context window size in tokens (0 = unknown)
	EnableKeepalive *bool  // nil=auto-detect, true/false=explicit
	PromptCacheTTL  string // Go duration, empty=auto-detect
}

// ResolveModel takes a model string (alias or developer/model_id)
// and returns the canonical resolution with wire format and endpoint.
// endpoint param is optional - if empty, auto-selects based on developer.
// models is the named model config map; entries are checked first by name,
// then their .Model field provides the developer/model_id string.
// Raw "developer/model_id" strings not in the models map still work.
//
// Resolution logic:
// 1. Resolve named model: check if input exists in models map (carries settings)
// 2. Parse developer/model: split on "/" (error if no slash found)
// 3. Infer wire format from developer
// 4. Determine endpoint (explicit override, or auto-select based on developer)
func ResolveModel(input string, endpoint string, models map[string]ModelConfig) (*ResolvedModel, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, fmt.Errorf("model string is empty")
	}

	// Step 1: Resolve named model entry (carries settings through)
	resolved := input
	var mc *ModelConfig
	if len(models) > 0 {
		if cfg, ok := models[strings.ToLower(input)]; ok {
			resolved = cfg.Model
			mc = &cfg
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

	rm := &ResolvedModel{
		Developer: developer,
		ModelID:   modelID,
		Format:    format,
		Endpoint:  endpointName,
	}

	// Carry settings from ModelConfig if resolved via a named entry
	if mc != nil {
		rm.Thinking = string(mc.Thinking)
		rm.Effort = mc.Effort
		rm.Speed = mc.Speed
		rm.Context = int(mc.Context)
		rm.EnableKeepalive = mc.EnableKeepalive
		rm.PromptCacheTTL = mc.PromptCacheTTL
	}

	return rm, nil
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
