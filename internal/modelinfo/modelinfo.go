// Package modelinfo provides a single registry of model attributes (context
// window, capabilities, pricing). Other packages delegate to this leaf
// package instead of maintaining their own copies.
package modelinfo

import (
	_ "embed"
	"encoding/json"
	"fmt"
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
	Provider        string  // provider qualifier (e.g. "openrouter", "zai-coding-plan")
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

// registry maps bare model IDs to provider→Model maps. The "" provider key is
// the providerless/default entry; all built-in entries use "openrouter" (the
// sole-provider fallback means any lookup matches regardless of provider).
// Populated from models.jsonl at init. Guarded by registryMu.
var registry = map[string]map[string]Model{}

// builtInData is the raw embedded model pricing data, parsed at init.
//
//go:embed models.jsonl
var builtInData []byte

// jsonlEntry is the JSON representation of a model entry in models.jsonl.
// It maps directly to the Model struct; the Comment field is informational
// only and not stored in the registry.
type jsonlEntry struct {
	ID              string  `json:"id"`
	Provider        string  `json:"provider"`
	ContextWindow   int     `json:"context_window,omitempty"`
	Effort          bool    `json:"effort,omitempty"`
	Thinking        bool    `json:"thinking,omitempty"`
	Speed           bool    `json:"speed,omitempty"`
	Caching         bool    `json:"caching,omitempty"`
	InputPer1M      float64 `json:"input_per_1m,omitempty"`
	OutputPer1M     float64 `json:"output_per_1m,omitempty"`
	CacheReadPer1M  float64 `json:"cache_read_per_1m,omitempty"`
	CacheWritePer1M float64 `json:"cache_write_per_1m,omitempty"`
	Comment         string  `json:"comment,omitempty"`
}

// registryMu guards registry. RLock for reads (accessors), Lock for writes
// (Register, ResetToBuiltIn via live-apply).
var registryMu sync.RWMutex

// builtIn is a deep snapshot of the registry taken at init from models.jsonl,
// so live-apply can ResetToBuiltIn and re-apply config overrides from scratch.
var builtIn = map[string]map[string]Model{}

func init() {
	// Parse embedded JSONL into registry.
	for _, line := range strings.Split(string(builtInData), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e jsonlEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			panic(fmt.Sprintf("modelinfo: parse models.jsonl line %q: %v", line, err))
		}
		if e.ID == "" {
			panic(fmt.Sprintf("modelinfo: models.jsonl entry missing id: %q", line))
		}
		provider := strings.ToLower(e.Provider)
		id := strings.ToLower(e.ID)
		m := Model{
			Provider:        provider,
			ContextWindow:   e.ContextWindow,
			Effort:          e.Effort,
			Thinking:        e.Thinking,
			Speed:           e.Speed,
			Caching:         e.Caching,
			InputPer1M:      e.InputPer1M,
			OutputPer1M:     e.OutputPer1M,
			CacheReadPer1M:  e.CacheReadPer1M,
			CacheWritePer1M: e.CacheWritePer1M,
		}
		if registry[id] == nil {
			registry[id] = map[string]Model{}
		}
		registry[id][provider] = m
	}

	// Snapshot for ResetToBuiltIn.
	for k, v := range registry {
		builtIn[k] = map[string]Model{}
		for pk, pv := range v {
			builtIn[k][pk] = pv
		}
	}
}

// Register adds or overrides a registry entry. Called at startup from
// config-loaded [[modelinfo]] sections, and at runtime from live-apply.
// provider may be "" for a providerless (default) entry.
func Register(provider, modelID string, m Model) {
	provider = strings.ToLower(provider)
	modelID = strings.ToLower(stripDateSuffix(modelID))
	m.Provider = provider
	registryMu.Lock()
	defer registryMu.Unlock()
	if registry[modelID] == nil {
		registry[modelID] = map[string]Model{}
	}
	registry[modelID][provider] = m
}

// Lookup returns the model attributes for the given provider and model ID and
// whether it exists. Tries a provider-specific entry first, then falls back to
// the providerless ("") entry.
func Lookup(provider, modelID string) (Model, bool) {
	provider = strings.ToLower(provider)
	modelID = strings.ToLower(stripDateSuffix(modelID))
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registryLookup(provider, modelID)
}

// registryLookup tries a provider-specific entry first, then falls back to the
// providerless ("") entry, then to a sole remaining provider entry. Provider
// only distinguishes the preferred entry when the model already matches — a
// model registered under a single provider matches regardless of which
// provider the lookup carries (or none). Caller must hold registryMu.
func registryLookup(provider, bare string) (Model, bool) {
	providers := registry[bare]
	if providers == nil {
		return Model{}, false
	}
	if provider != "" {
		if m, ok := providers[provider]; ok {
			return m, true
		}
	}
	if m, ok := providers[""]; ok {
		return m, true
	}
	// No exact provider or providerless match. If only one provider entry
	// exists, use it — the model matches, the provider is just not the one
	// the lookup asked for.
	if len(providers) == 1 {
		for _, m := range providers {
			return m, true
		}
	}
	return Model{}, false
}

// ResetToBuiltIn restores the registry to its built-in defaults (from
// models.jsonl), discarding all config overrides. Called by live-apply.
func ResetToBuiltIn() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = make(map[string]map[string]Model, len(builtIn))
	for k, v := range builtIn {
		inner := make(map[string]Model, len(v))
		for pk, pv := range v {
			inner[pk] = pv
		}
		registry[k] = inner
	}
}

// StripPrefix removes a "developer/" prefix from a model string.
// Exported so CC backends (ccstream, cctmux) can strip the provider
// prefix before passing the model to Claude's --model flag, which
// expects a bare model name (e.g. "claude-sonnet-5"), not a
// provider-qualified one (e.g. "claude/claude-sonnet-5").
func StripPrefix(model string) string {
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
	return stripDateSuffix(StripPrefix(model))
}

// normalizeParts splits a model string into provider and bare model ID,
// both lowercased, with date suffix stripped from the bare ID.
func normalizeParts(model string) (provider, bare string) {
	if i := strings.IndexByte(model, '/'); i > 0 {
		return strings.ToLower(model[:i]), strings.ToLower(stripDateSuffix(model[i+1:]))
	}
	return "", strings.ToLower(stripDateSuffix(model))
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
	provider, bare := normalizeParts(model)
	registryMu.RLock()
	m, ok := registryLookup(provider, bare)
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
	provider, bare := normalizeParts(model)
	registryMu.RLock()
	m, ok := registryLookup(provider, bare)
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
	provider, bare := normalizeParts(model)
	registryMu.RLock()
	m, ok := registryLookup(provider, bare)
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
	provider, bare := normalizeParts(model)
	registryMu.RLock()
	defer registryMu.RUnlock()
	m, ok := registryLookup(provider, bare)
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
			m, _ = registryLookup("", "claude-haiku-4-5")
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
// built-in members of each family. Caller must hold registryMu.
func familyPricing(bare string) (Model, bool) {
	switch {
	case strings.Contains(bare, "fable"), strings.Contains(bare, "mythos"):
		return registryLookup("", "claude-fable-5")
	case strings.Contains(bare, "opus"):
		return registryLookup("", "claude-opus-4-6")
	case strings.Contains(bare, "sonnet"):
		return registryLookup("", "claude-sonnet-4-5")
	case strings.Contains(bare, "haiku"):
		return registryLookup("", "claude-haiku-4-5")
	case strings.Contains(bare, "gemini"):
		return registryLookup("", "gemini-2.5-flash")
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
	bare := StripPrefix(model)
	for _, p := range []string{"gpt-", "o1", "o3", "o4", "chatgpt-"} {
		if strings.HasPrefix(bare, p) {
			return true
		}
	}
	return false
}
