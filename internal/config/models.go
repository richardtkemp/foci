package config

import (
	"fmt"
	"strings"

	"foci/internal/modelinfo"
)

// ModelDefaults holds per-model settings resolved at request time from [models.*] config.
// Used by ModelDefaultsFn callbacks in the agent and compaction packages.
type ModelDefaults struct {
	Thinking      string
	Effort        string
	Speed         string
	CacheStrategy string
	CacheTTL      string
}

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
	CacheTTL        string // cache TTL: Go duration, empty=auto-detect
	CacheStrategy   string // "auto" or "explicit" (Anthropic only)
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

	// Use endpoint from named model config if caller didn't specify one
	if mc != nil && mc.Endpoint != "" && endpoint == "" {
		endpoint = mc.Endpoint
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
		rm.CacheTTL = mc.CacheTTL
		rm.CacheStrategy = mc.CacheStrategy
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

// toModel converts a ModelInfoEntry to a modelinfo.Model, applying the two-mode
// validation: overrides of existing entries merge over built-in values (only
// specified fields change); new entries require context_window, input_per_1m,
// and output_per_1m, with capability flags defaulting to false and cache
// pricing to 0.0.
func (e ModelInfoEntry) toModel() (modelinfo.Model, error) {
	if e.ID == "" {
		return modelinfo.Model{}, fmt.Errorf("missing required field: id")
	}

	provider, modelID := SplitDeveloperModel(e.ID)
	existing, isOverride := modelinfo.Lookup(provider, modelID)

	if !isOverride {
		var missing []string
		if e.ContextWindow == nil {
			missing = append(missing, "context_window")
		}
		if e.InputPer1M == nil {
			missing = append(missing, "input_per_1m")
		}
		if e.OutputPer1M == nil {
			missing = append(missing, "output_per_1m")
		}
		if len(missing) > 0 {
			return modelinfo.Model{}, fmt.Errorf("missing required fields for new model %q: %s", e.ID, strings.Join(missing, ", "))
		}
	}

	// Start from existing entry (override) or zero value (new model).
	m := existing

	if e.ContextWindow != nil {
		m.ContextWindow = *e.ContextWindow
	}
	if e.CanEffort != nil {
		m.Effort = *e.CanEffort
	}
	if e.CanThinking != nil {
		m.Thinking = *e.CanThinking
	}
	if e.CanSpeed != nil {
		m.Speed = *e.CanSpeed
	}
	if e.CanCaching != nil {
		m.Caching = *e.CanCaching
	}
	if e.InputPer1M != nil {
		m.InputPer1M = *e.InputPer1M
	}
	if e.OutputPer1M != nil {
		m.OutputPer1M = *e.OutputPer1M
	}
	if e.CacheReadPer1M != nil {
		m.CacheReadPer1M = *e.CacheReadPer1M
	}
	if e.CacheWritePer1M != nil {
		m.CacheWritePer1M = *e.CacheWritePer1M
	}

	return m, nil
}

// ApplyModelInfo merges [[modelinfo]] config entries into the modelinfo
// registry. Called from Load() at startup and from the live-apply applier.
func ApplyModelInfo(entries []ModelInfoEntry) {
	for _, entry := range entries {
		m, err := entry.toModel()
		if err != nil {
			configLog.Warnf("[modelinfo] id=%q %s — skipping", entry.ID, err)
			continue
		}
		provider, modelID := SplitDeveloperModel(entry.ID)

		// When a provider-prefixed ID overrides a built-in (providerless)
		// model, register under "" so bare runtime lookups see the override.
		// The tell: no provider-specific entry exists (LookupExact misses)
		// but a providerless one does (the built-in). Without this, the
		// override would be stored under the provider and invisible to
		// runtime calls that pass the bare model ID without a prefix.
		if provider != "" {
			if _, hasExact := modelinfo.LookupExact(provider, modelID); !hasExact {
				if _, hasProviderless := modelinfo.LookupExact("", modelID); hasProviderless {
					provider = ""
				}
			}
		}

		modelinfo.Register(provider, modelID, m)
	}
}
