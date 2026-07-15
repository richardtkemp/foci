// Package modelinfo provides a single registry of model attributes (context
// window, capabilities, pricing). Other packages delegate to this leaf
// package instead of maintaining their own copies.
package modelinfo

import (
	"strings"
	"sync"
)

// syntheticModel is CC's sentinel model name for a zero-cost no-op /
// session-limit turn. It carries no real pricing, so it is priced at $0 and must
// never trip the unpriced-model warning. Kept as a local literal (rather than
// importing the ccstream constant) because modelinfo is a leaf package.
const syntheticModel = "<synthetic>"

// IsSynthetic reports whether model is CC's zero-cost synthetic sentinel.
func IsSynthetic(model string) bool { return model == syntheticModel }

// UnpricedModelHook, if set, is invoked once per distinct model that resolves
// to a fallback rate (no exact registry hit and no family match). Wired at
// startup to a log warning. A hook rather than a direct log call because
// modelinfo is a leaf package and internal/log imports it.
var UnpricedModelHook func(model string)

var (
	unpricedMu   sync.Mutex
	unpricedSeen = map[string]bool{}
)

func noteUnpriced(bare string) {
	if UnpricedModelHook == nil {
		return
	}
	unpricedMu.Lock()
	first := !unpricedSeen[bare]
	unpricedSeen[bare] = true
	unpricedMu.Unlock()
	if first {
		UnpricedModelHook(bare)
	}
}

// Model holds the static attributes of a model.
type Model struct {
	ContextWindow   int     // tokens
	Effort          bool    // supports output_config.effort
	Thinking        bool    // supports thinking (adaptive/enabled)
	Speed           bool    // supports fast mode (speed: "fast")
	Caching         bool    // supports explicit, TTL-bounded prompt caching that keepalive pings warm
	InputPer1M      float64 // cost per 1M input tokens
	OutputPer1M     float64 // cost per 1M output tokens
	CacheReadPer1M  float64 // cost per 1M cache-read tokens
	CacheWritePer1M float64 // cost per 1M cache-write tokens
}

// registry maps bare model IDs to their attributes. Guarded by registryMu:
// reads from the exported accessors (ContextWindow, Cost, etc.) take RLock;
// Register and ResetToBuiltIn (live-apply) take Lock. familyPricing is called
// only from Cost which already holds RLock, so it assumes the caller holds it.
var registry = map[string]Model{
	// Anthropic
	"claude-haiku-4-5": {
		ContextWindow: 200_000,
		Caching:       true,
		InputPer1M:    1.00, OutputPer1M: 5.00,
		CacheReadPer1M: 0.10, CacheWritePer1M: 1.25,
	},
	"claude-sonnet-4-5": {
		ContextWindow: 200_000,
		Effort:        true, Thinking: true, Caching: true,
		InputPer1M: 3.00, OutputPer1M: 15.00,
		CacheReadPer1M: 0.30, CacheWritePer1M: 3.75,
	},
	"claude-opus-4-6": {
		ContextWindow: 1_000_000, // 1M with Claude Max subscription
		Effort:        true, Thinking: true, Speed: true, Caching: true,
		InputPer1M: 15.00, OutputPer1M: 75.00,
		CacheReadPer1M: 1.50, CacheWritePer1M: 18.75,
	},
	"claude-opus-4-6[1m]": { // CC reports model with [1m] suffix for Max subscription
		ContextWindow: 1_000_000,
		Effort:        true, Thinking: true, Speed: true, Caching: true,
		InputPer1M: 15.00, OutputPer1M: 75.00,
		CacheReadPer1M: 1.50, CacheWritePer1M: 18.75,
	},
	"claude-fable-5": { // Mythos-class, GA 2026-06-09; tier above Opus
		ContextWindow: 1_000_000, // full 1M at standard pricing
		Effort:        true, Thinking: true, Caching: true,
		InputPer1M: 10.00, OutputPer1M: 50.00,
		CacheReadPer1M: 1.00, CacheWritePer1M: 12.50,
	},

	// Claude Code backends — default to largest available context window so
	// we don't trigger spurious compaction before learning the true model.
	// FinalModel feedback in UpdateSessionMeta corrects this downward if needed.
	"claude-code-tmux": {
		ContextWindow: 1_000_000,
		Caching:       true,
	},
	"claude-code": {
		ContextWindow: 1_000_000,
		Caching:       true,
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

// registryMu guards registry. RLock for reads (accessors), Lock for writes
// (Register, ResetToBuiltIn via live-apply).
var registryMu sync.RWMutex

// builtIn is a snapshot of the hardcoded registry taken at init, so live-apply
// can ResetToBuiltIn and re-apply config overrides from scratch.
var builtIn = map[string]Model{}

func init() {
	for k, v := range registry {
		builtIn[k] = v
	}
}

// Register adds or overrides a registry entry. Called at startup from
// config-loaded [[modelinfo]] sections, and at runtime from live-apply.
// The key is normalized (prefix stripped, lowercased) to match the form used
// by all registry lookups, so users can write the ID with or without a
// provider prefix (e.g. "zai-coding-plan/glm-5.2" → "glm-5.2").
func Register(key string, m Model) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[normalizeKey(key)] = m
}

// Lookup returns the model attributes for key and whether it exists.
func Lookup(key string) (Model, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	m, ok := registry[normalizeKey(key)]
	return m, ok
}

// normalizeKey lowercases and normalizes (strips prefix + date suffix) a
// registry key, matching the lookup path used by all exported accessors.
func normalizeKey(key string) string {
	return strings.ToLower(normalize(key))
}

// ResetToBuiltIn restores the registry to its hardcoded defaults, discarding
// all config overrides. Called by live-apply before re-applying.
func ResetToBuiltIn() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = make(map[string]Model, len(builtIn))
	for k, v := range builtIn {
		registry[k] = v
	}
}

// stripPrefix removes a "developer/" prefix from a model string.
func stripPrefix(model string) string {
	if i := strings.IndexByte(model, '/'); i > 0 {
		return model[i+1:]
	}
	return model
}

// stripDateSuffix removes a trailing "-YYYYMMDD" date suffix from a model
// name. CC sometimes reports dated model variants (e.g.
// "claude-haiku-4-5-20251001") that don't match our registry keys.
func stripDateSuffix(model string) string {
	// Need at least "-" + 8 digits.
	if len(model) < 9 {
		return model
	}
	tail := model[len(model)-9:]
	if tail[0] != '-' {
		return model
	}
	for i := 1; i < 9; i++ {
		if tail[i] < '0' || tail[i] > '9' {
			return model
		}
	}
	return model[:len(model)-9]
}

// normalize strips provider prefixes and date suffixes from a model string.
func normalize(model string) string {
	return stripDateSuffix(stripPrefix(model))
}

// Normalize strips provider prefixes and date suffixes from a model string,
// yielding the bare registry key (e.g. "anthropic/claude-opus-4-8-20260528" →
// "claude-opus-4-8"). Exported so other packages (e.g. modelcaps) key their
// caches the same way the registry does.
func Normalize(model string) string {
	return normalize(model)
}

// ContextWindow returns the context window for a model.
// Falls back to family defaults: gemini-1.5-* → 2M, gemini-* → 1M,
// everything else (including claude) → 200k.
func ContextWindow(model string) int {
	bare := normalize(model)
	registryMu.RLock()
	m, ok := registry[bare]
	registryMu.RUnlock()
	if ok {
		return m.ContextWindow
	}
	// Family fallbacks
	switch {
	case strings.Contains(bare, "gemini-1.5"):
		return 2_000_000
	case strings.Contains(bare, "gemini-"):
		return 1_000_000
	case strings.Contains(bare, "opus"), strings.Contains(bare, "fable"), strings.Contains(bare, "mythos"):
		return 1_000_000
	default:
		return 200_000
	}
}

// Capabilities returns whether a model supports effort, thinking, and speed.
// Falls back to family defaults: claude-sonnet → effort+thinking,
// claude-opus → effort+thinking+speed, everything else → none.
func Capabilities(model string) (effort, thinking, speed bool) {
	bare := strings.ToLower(normalize(model))
	registryMu.RLock()
	m, ok := registry[bare]
	registryMu.RUnlock()
	if ok {
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

// Caching reports whether a model supports the explicit, TTL-bounded prompt
// cache that foci's keepalive pings warm. Only Anthropic (claude) models do:
// Gemini caching is implicit/automatic (no ping warms it) and OpenAI's is
// automatic too. Falls back to the claude family so unregistered/dated claude
// variants still resolve true.
//
// This answers a STATIC capability question for API agents (resolved.ModelID).
// Delegated/claude-code agents have no resolved model and are handled at the
// call site (they keep keepalive — their backend has its own prompt cache).
func Caching(model string) bool {
	bare := strings.ToLower(normalize(model))
	registryMu.RLock()
	m, ok := registry[bare]
	registryMu.RUnlock()
	if ok {
		return m.Caching
	}
	return strings.Contains(bare, "claude")
}

// Cost returns the estimated cost in USD for an API request.
// An exact registry hit wins; otherwise pricing is by model FAMILY (opus,
// fable, sonnet, haiku, gemini) so a new version — opus-4-8, sonnet-4-6, … —
// inherits its family's rates without needing a per-version registry entry.
// Final fallbacks: OpenAI → $5/$15 approximation, everything else → haiku.
func Cost(model string, input, output, cacheRead, cacheWrite int) float64 {
	// CC's synthetic sentinel is a zero-cost no-op / session-limit turn: there is
	// nothing to price, and pricing it would spuriously trip the unpriced warning.
	if IsSynthetic(model) {
		return 0
	}
	bare := normalize(model)
	registryMu.RLock()
	defer registryMu.RUnlock()
	m, ok := registry[bare]
	if !ok {
		m, ok = familyPricing(bare) // caller-holds-lock: Cost holds RLock
	}
	if !ok {
		// noteUnpriced uses its own mutex (unpricedMu), not registryMu.
		noteUnpriced(bare)
		switch {
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

// familyPricing maps a bare model name to a canonical per-family price entry by
// family keyword, so pricing tracks the family ("opus costs this much") rather
// than an exact version string. The canonical entries are the registry's
// current members of each family.
func familyPricing(bare string) (Model, bool) {
	switch {
	case strings.Contains(bare, "fable"), strings.Contains(bare, "mythos"):
		return registry["claude-fable-5"], true
	case strings.Contains(bare, "opus"):
		return registry["claude-opus-4-6"], true
	case strings.Contains(bare, "sonnet"):
		return registry["claude-sonnet-4-5"], true
	case strings.Contains(bare, "haiku"):
		return registry["claude-haiku-4-5"], true
	case strings.Contains(bare, "gemini"):
		return registry["gemini-2.5-flash"], true
	}
	return Model{}, false
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
