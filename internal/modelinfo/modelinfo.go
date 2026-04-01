// Package modelinfo provides a single registry of model attributes (context
// window, capabilities, pricing). Other packages delegate to this leaf
// package instead of maintaining their own copies.
package modelinfo

import "strings"

// Model holds the static attributes of a model.
type Model struct {
	ContextWindow   int     // tokens
	Effort          bool    // supports output_config.effort
	Thinking        bool    // supports thinking (adaptive/enabled)
	Speed           bool    // supports fast mode (speed: "fast")
	InputPer1M      float64 // cost per 1M input tokens
	OutputPer1M     float64 // cost per 1M output tokens
	CacheReadPer1M  float64 // cost per 1M cache-read tokens
	CacheWritePer1M float64 // cost per 1M cache-write tokens
}

// registry maps bare model IDs to their attributes.
var registry = map[string]Model{
	// Anthropic
	"claude-haiku-4-5": {
		ContextWindow: 200_000,
		InputPer1M:    1.00, OutputPer1M: 5.00,
		CacheReadPer1M: 0.10, CacheWritePer1M: 1.25,
	},
	"claude-sonnet-4-5": {
		ContextWindow: 200_000,
		Effort: true, Thinking: true,
		InputPer1M: 3.00, OutputPer1M: 15.00,
		CacheReadPer1M: 0.30, CacheWritePer1M: 3.75,
	},
	"claude-opus-4-6": {
		ContextWindow: 1_000_000, // 1M with Claude Max subscription
		Effort: true, Thinking: true, Speed: true,
		InputPer1M: 15.00, OutputPer1M: 75.00,
		CacheReadPer1M: 1.50, CacheWritePer1M: 18.75,
	},

	// Gemini
	"gemini-2.5-pro": {
		ContextWindow: 1_000_000,
		InputPer1M:    1.25, OutputPer1M: 10.00,
		CacheReadPer1M: 0.315,
	},
	"gemini-2.5-flash": {
		ContextWindow: 1_000_000,
		InputPer1M:    0.15, OutputPer1M: 0.60,
		CacheReadPer1M: 0.0375,
	},
	"gemini-2.0-flash": {
		ContextWindow: 1_000_000,
		InputPer1M:    0.10, OutputPer1M: 0.40,
		CacheReadPer1M: 0.025,
	},
}

// stripPrefix removes a "developer/" prefix from a model string.
func stripPrefix(model string) string {
	if i := strings.IndexByte(model, '/'); i > 0 {
		return model[i+1:]
	}
	return model
}

// ContextWindow returns the context window for a model.
// Falls back to family defaults: gemini-1.5-* → 2M, gemini-* → 1M,
// everything else (including claude) → 200k.
func ContextWindow(model string) int {
	bare := stripPrefix(model)
	if m, ok := registry[bare]; ok {
		return m.ContextWindow
	}
	// Family fallbacks
	switch {
	case strings.Contains(bare, "gemini-1.5"):
		return 2_000_000
	case strings.Contains(bare, "gemini-"):
		return 1_000_000
	default:
		return 200_000
	}
}

// Capabilities returns whether a model supports effort, thinking, and speed.
// Falls back to family defaults: claude-sonnet → effort+thinking,
// claude-opus → effort+thinking+speed, everything else → none.
func Capabilities(model string) (effort, thinking, speed bool) {
	bare := strings.ToLower(stripPrefix(model))
	if m, ok := registry[bare]; ok {
		return m.Effort, m.Thinking, m.Speed
	}
	// Family fallbacks for unregistered claude variants
	if strings.Contains(bare, "claude") {
		if strings.Contains(bare, "haiku") {
			return false, false, false
		}
		if strings.Contains(bare, "opus") {
			return true, true, true
		}
		// sonnet or unknown claude
		return true, true, false
	}
	return false, false, false
}

// Cost returns the estimated cost in USD for an API request.
// Falls back to family defaults: gemini → flash pricing,
// OpenAI → $5/$15 approximation, everything else → haiku pricing.
func Cost(model string, input, output, cacheRead, cacheWrite int) float64 {
	bare := stripPrefix(model)
	m, ok := registry[bare]
	if !ok {
		switch {
		case strings.HasPrefix(bare, "gemini-"):
			m = registry["gemini-2.5-flash"]
		case IsOpenAI(bare):
			m = Model{InputPer1M: 5.00, OutputPer1M: 15.00}
		default:
			m = registry["claude-haiku-4-5"]
		}
	}

	mtok := 1_000_000.0
	return float64(input)/mtok*m.InputPer1M +
		float64(output)/mtok*m.OutputPer1M +
		float64(cacheRead)/mtok*m.CacheReadPer1M +
		float64(cacheWrite)/mtok*m.CacheWritePer1M
}

// ModelMeta holds structural metadata about a model from [models.*] config.
// Used at runtime to override registry defaults (e.g. when config defines
// a custom context window for a third-party model).
type ModelMeta struct {
	ContextWindow int // 0 = unknown, fall back to registry
}

// IsOpenAI returns true if the model name looks like an OpenAI model.
func IsOpenAI(model string) bool {
	bare := stripPrefix(model)
	for _, p := range []string{"gpt-", "o1", "o3", "o4", "chatgpt-"} {
		if strings.HasPrefix(bare, p) {
			return true
		}
	}
	return false
}
