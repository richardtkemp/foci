// Package modelinfo provides a single registry of model attributes (context
// window, capabilities, pricing). Other packages delegate to this leaf
// package instead of maintaining their own copies.
package modelinfo

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
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

// AmbiguousModelHook, if set, is invoked once per distinct leaf id whose lookup
// had to fall back to a deterministic pick among a genuine collision (two
// entries under the same leaf that the input couldn't disambiguate by dev or
// provider). Wired at startup to a log warning so a real collision surfaces.
var AmbiguousModelHook func(bare string)

var (
	ambiguousMu   sync.Mutex
	ambiguousSeen = map[string]bool{}
)

func noteAmbiguous(bare string) {
	if AmbiguousModelHook == nil {
		return
	}
	ambiguousMu.Lock()
	first := !ambiguousSeen[bare]
	ambiguousSeen[bare] = true
	ambiguousMu.Unlock()
	if first {
		AmbiguousModelHook(bare)
	}
}

// Model holds the static attributes of a model.
type Model struct {
	Provider        string  // provider qualifier / API host (e.g. "openrouter", "zai-coding-plan")
	Dev             string  // model author/vendor slug (e.g. "moonshotai", "anthropic"); the segment OpenRouter puts before the model id. Distinct from Provider (the API host).
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
	Dev             string  `json:"dev,omitempty"`
	ContextWindow   int     `json:"context_window,omitempty"`
	Effort          bool    `json:"effort,omitempty"`
	Thinking        bool    `json:"thinking,omitempty"`
	Speed           bool    `json:"speed,omitempty"`
	Caching         bool    `json:"caching,omitempty"`
	InputPer1M      float64 `json:"input_per_1m,omitempty"`
	OutputPer1M     float64 `json:"output_per_1m,omitempty"`
	CacheReadPer1M  float64 `json:"cache_read_per_1m,omitempty"`
	CacheWritePer1M float64 `json:"cache_write_per_1m,omitempty"`
	// Extended pricing + quality captured by sync-modelinfo. Parsed but NOT yet
	// used at runtime (see TODO #1407 — cost calc still uses only the flat base
	// rates above). Kept here so the parser documents the full schema.
	CacheWrite1hPer1M      float64          `json:"cache_write_1h_per_1m,omitempty"`
	InternalReasoningPer1M float64          `json:"internal_reasoning_per_1m,omitempty"`
	WebSearchPerCall       float64          `json:"web_search_per_call,omitempty"`
	ImagePrice             float64          `json:"image_price,omitempty"`
	AudioPrice             float64          `json:"audio_price,omitempty"`
	PriceTiers             []jsonlPriceTier `json:"price_tiers,omitempty"`
	IntelligenceIndex      float64          `json:"intelligence_index,omitempty"`
	// Fetched (UTC date the pricing was last confirmed against OpenRouter) and
	// Comment are informational provenance only — not stored in the registry.
	Fetched string `json:"fetched,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// jsonlPriceTier mirrors a usage-dependent price schedule in models.jsonl
// (OpenRouter overrides). Parsed for schema-completeness; not yet used.
type jsonlPriceTier struct {
	MinPromptTokens   int     `json:"min_prompt_tokens"`
	InputPer1M        float64 `json:"input_per_1m,omitempty"`
	OutputPer1M       float64 `json:"output_per_1m,omitempty"`
	CacheReadPer1M    float64 `json:"cache_read_per_1m,omitempty"`
	CacheWritePer1M   float64 `json:"cache_write_per_1m,omitempty"`
	CacheWrite1hPer1M float64 `json:"cache_write_1h_per_1m,omitempty"`
}

// registryMu guards registry. RLock for reads (accessors), Lock for writes
// (Register, ResetToBuiltIn via live-apply).
var registryMu sync.RWMutex

// builtIn is a deep snapshot of the registry taken at init from models.jsonl,
// so live-apply can ResetToBuiltIn and re-apply config overrides from scratch.
var builtIn = map[string]map[string]Model{}

// historyRow is one models.jsonl row for a given (id, provider) key, kept for
// as-of-time price lookups (see `history` below). fetched="" (a pre-history
// baseline row) sorts before every real date and is treated as "in effect
// since before any recorded history".
type historyRow struct {
	fetched string
	model   Model
}

// history maps bare model ID → provider → that (id,provider)'s rows in
// ASCENDING fetched order (ties broken by original file/append order — see
// parseModelsJSONL). Kept alongside `registry` (which only retains the LATEST
// row) so LookupAsOf/CostAsOf can reconstruct the price that was actually in
// effect at an arbitrary past timestamp — e.g. re-deriving the live-estimated
// cost of a session logged days ago after models.jsonl has since recorded a
// newer price for that model (foci_todo #1407, point 4: price the call using
// the rate effective AT THE REQUEST'S TIME, not today's latest rate).
//
// GRANULARITY CAVEAT (flagged deliberately, not hidden — see notes-1407.md):
// `fetched` is a DATE (YYYY-MM-DD), not a timestamp, and it records when
// sync-modelinfo OBSERVED a price, not when the price actually changed. Two
// price changes on the same calendar day cannot be told apart, and a request
// that landed inside the gap between two sync runs is priced at the nearest
// PRECEDING observation — the best available approximation from the data
// that exists today, not a guarantee of the exact historical price.
var history = map[string]map[string][]historyRow{}
var historyMu sync.RWMutex

// builtInHistory is a deep snapshot of `history` taken at init, mirroring
// `builtIn` for `registry` — so ResetToBuiltIn restores both together.
var builtInHistory = map[string]map[string][]historyRow{}

func init() {
	reg, hist, err := parseModelsJSONL(builtInData)
	if err != nil {
		panic(err.Error())
	}
	registry = reg
	history = hist

	// Snapshot for ResetToBuiltIn.
	for k, v := range registry {
		builtIn[k] = map[string]Model{}
		for pk, pv := range v {
			builtIn[k][pk] = pv
		}
	}
	for k, v := range history {
		builtInHistory[k] = map[string][]historyRow{}
		for pk, pv := range v {
			rows := make([]historyRow, len(pv))
			copy(rows, pv)
			builtInHistory[k][pk] = rows
		}
	}
}

// parseModelsJSONL parses the append-only models.jsonl into BOTH the
// latest-only `registry` (used by Lookup/Cost) and the full `history` per
// (id, provider) (used by LookupAsOf/CostAsOf) — one pass, one source of
// truth, rather than parsing the file twice. models.jsonl is a HISTORY: a
// model may have several rows over time, each stamped with the `fetched` date
// it was observed. Only the LATEST row per (id, provider) populates
// `registry` — max `fetched`, with later file position breaking ties (append
// order), and an empty `fetched` (pre-history baseline rows) treated as
// oldest. `history` retains every row (sorted ascending by `fetched`) for the
// as-of lookups. Factored out of init for testability.
func parseModelsJSONL(data []byte) (registry map[string]map[string]Model, history map[string]map[string][]historyRow, err error) {
	registry = map[string]map[string]Model{}
	history = map[string]map[string][]historyRow{}
	// fetchedAt[id][provider] = the `fetched` of the row currently stored in
	// `registry`, so we only overwrite with a same-or-newer one.
	fetchedAt := map[string]map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e jsonlEntry
		if uerr := json.Unmarshal([]byte(line), &e); uerr != nil {
			return nil, nil, fmt.Errorf("modelinfo: parse models.jsonl line %q: %v", line, uerr)
		}
		if e.ID == "" {
			return nil, nil, fmt.Errorf("modelinfo: models.jsonl entry missing id: %q", line)
		}
		provider := strings.ToLower(e.Provider)
		id := strings.ToLower(e.ID)
		dev := strings.ToLower(e.Dev)
		key := provKey(provider, dev)
		m := Model{
			Provider:        provider,
			Dev:             dev,
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

		if history[id] == nil {
			history[id] = map[string][]historyRow{}
		}
		history[id][key] = append(history[id][key], historyRow{fetched: e.Fetched, model: m})

		if registry[id] == nil {
			registry[id] = map[string]Model{}
			fetchedAt[id] = map[string]string{}
		}
		// `fetched` is a YYYY-MM-DD date, so lexical compare is chronological;
		// "" (baseline) precedes any real date. >= lets a later line win ties.
		// Keyed by (provider, dev): two models can share a leaf id under the
		// same provider with different devs (a genuine collision), so the
		// latest-row dedup must not collapse them onto each other.
		if _, has := registry[id][key]; has && e.Fetched < fetchedAt[id][key] {
			continue // an older historical row — keep the newer one already stored
		}
		registry[id][key] = m
		fetchedAt[id][key] = e.Fetched
	}

	// history is appended in FILE order above, which is normally also
	// ascending-by-fetched (sync-modelinfo's writeJSONL sorts the file that
	// way) — but nothing enforces that invariant on a hand-edited or
	// hand-constructed models.jsonl, and historyLookupAsOf's scan assumes
	// ascending order. Sort explicitly (stable, so same-date rows keep their
	// file-order tie-break) rather than trust the input's order.
	for _, byProvider := range history {
		for provider, rows := range byProvider {
			sort.SliceStable(rows, func(i, j int) bool { return rows[i].fetched < rows[j].fetched })
			byProvider[provider] = rows
		}
	}
	return registry, history, nil
}

// Register adds or overrides a registry entry. Called at startup from
// config-loaded [[modelinfo]] sections, and at runtime from live-apply.
// provider may be "" for a providerless (default) entry.
func Register(provider, modelID string, m Model) {
	provider = strings.ToLower(provider)
	modelID = strings.ToLower(stripDateSuffix(modelID))
	m.Provider = provider
	m.Dev = strings.ToLower(m.Dev)
	key := provKey(provider, m.Dev)
	registryMu.Lock()
	if registry[modelID] == nil {
		registry[modelID] = map[string]Model{}
	}
	registry[modelID][key] = m
	registryMu.Unlock()

	// Also append to `history` so an as-of lookup made after this call sees
	// the override — stamped with today's date ("in effect from now on").
	// Config overrides/live-apply have no natural historical `fetched` of
	// their own, so "the day it was registered" is the best available anchor.
	historyMu.Lock()
	if history[modelID] == nil {
		history[modelID] = map[string][]historyRow{}
	}
	today := time.Now().UTC().Format("2006-01-02")
	history[modelID][key] = append(history[modelID][key], historyRow{fetched: today, model: m})
	historyMu.Unlock()
}

// Lookup returns the model attributes for the given provider and model ID and
// whether it exists. Tries a provider-specific entry first, then falls back to
// the providerless ("") entry.
func Lookup(provider, modelID string) (Model, bool) {
	segs, bare := splitSegs(modelID)
	if p := strings.ToLower(provider); p != "" {
		segs[p] = true
	}
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registryLookupSegs(segs, bare)
}

// provKey composes the registry/history inner-map key from a provider (the API
// host, e.g. "openrouter") and a dev (the model author/vendor, e.g.
// "moonshotai"). BOTH dimensions matter: two models can share a leaf id under
// the same provider but different devs — a genuine collision the old
// provider-only key silently overwrote at load time.
func provKey(provider, dev string) string { return provider + "\x00" + dev }

// candidates returns the distinct Models registered under a leaf id (one per
// (provider, dev)). Caller must hold registryMu.
func candidates(bare string) []Model {
	byKey := registry[bare]
	if len(byKey) == 0 {
		return nil
	}
	out := make([]Model, 0, len(byKey))
	for _, m := range byKey {
		out = append(out, m)
	}
	return out
}

// pickIndex chooses which candidate a lookup resolves to, given the caller's
// known segments — the provider/dev tokens parsed from the model string (plus
// any explicit provider hint). It is the single disambiguation authority shared
// by the registry and history lookups (which pass parallel candidate slices).
//
// Rules (confirmed design):
//   - 0 candidates → miss.
//   - 1 candidate  → that one (fast path): a sole entry matches regardless of a
//     mismatched dev/provider — dev DISAMBIGUATES a collision, it never rejects
//     an otherwise-unique model.
//   - >1 → prefer an entry matching BOTH provider and dev in segs; else a
//     unique dev match; else a unique provider match; else a providerless ("")
//     default; else a deterministic sorted pick, reported via AmbiguousModelHook
//     so a real collision surfaces instead of silently mis-resolving.
func pickIndex(cands []Model, segs map[string]bool, bare string) (int, bool) {
	switch len(cands) {
	case 0:
		return 0, false
	case 1:
		return 0, true
	}
	filter := func(pool []int, keep func(Model) bool) []int {
		var out []int
		for _, i := range pool {
			if keep(cands[i]) {
				out = append(out, i)
			}
		}
		return out
	}
	all := make([]int, len(cands))
	for i := range cands {
		all[i] = i
	}

	// Exact provider+dev match.
	if ex := filter(all, func(m Model) bool { return segs[m.Provider] && segs[m.Dev] }); len(ex) == 1 {
		return ex[0], true
	} else if len(ex) > 1 {
		all = ex
	}
	// Unique dev match.
	if dm := filter(all, func(m Model) bool { return m.Dev != "" && segs[m.Dev] }); len(dm) == 1 {
		return dm[0], true
	} else if len(dm) > 1 {
		all = dm
	}
	// Unique provider match.
	if pm := filter(all, func(m Model) bool { return m.Provider != "" && segs[m.Provider] }); len(pm) == 1 {
		return pm[0], true
	} else if len(pm) > 1 {
		all = pm
	}
	// Providerless ("") default entry.
	if pl := filter(all, func(m Model) bool { return m.Provider == "" }); len(pl) >= 1 {
		return pl[0], true
	}
	// Genuinely ambiguous: deterministic sorted pick, and log it.
	noteAmbiguous(bare)
	best := all[0]
	for _, i := range all[1:] {
		if cands[i].Provider < cands[best].Provider ||
			(cands[i].Provider == cands[best].Provider && cands[i].Dev < cands[best].Dev) {
			best = i
		}
	}
	return best, true
}

// registryLookupSegs resolves a leaf id against the registry using the caller's
// segment set. Caller must hold registryMu.
func registryLookupSegs(segs map[string]bool, bare string) (Model, bool) {
	cands := candidates(bare)
	i, ok := pickIndex(cands, segs, bare)
	if !ok {
		return Model{}, false
	}
	return cands[i], true
}

// registryLookup resolves a leaf id with no provider/dev hint — the family and
// claude-haiku fallbacks target single-candidate leaves, so this is the
// segment-less entry point. Caller must hold registryMu.
func registryLookup(bare string) (Model, bool) {
	return registryLookupSegs(map[string]bool{}, bare)
}

// ResetToBuiltIn restores the registry to its built-in defaults (from
// models.jsonl), discarding all config overrides. Called by live-apply.
func ResetToBuiltIn() {
	registryMu.Lock()
	registry = make(map[string]map[string]Model, len(builtIn))
	for k, v := range builtIn {
		inner := make(map[string]Model, len(v))
		for pk, pv := range v {
			inner[pk] = pv
		}
		registry[k] = inner
	}
	registryMu.Unlock()

	historyMu.Lock()
	history = make(map[string]map[string][]historyRow, len(builtInHistory))
	for k, v := range builtInHistory {
		inner := make(map[string][]historyRow, len(v))
		for pk, pv := range v {
			rows := make([]historyRow, len(pv))
			copy(rows, pv)
			inner[pk] = rows
		}
		history[k] = inner
	}
	historyMu.Unlock()
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

// normalize reduces a model string to its bare leaf id — the segment after the
// LAST '/' (OpenRouter ids are host/dev/model or dev/model, so the leaf is the
// registry key), with the date suffix stripped. Casing is preserved (matching
// the prior behaviour that modelcaps relies on for its cache keys).
func normalize(model string) string {
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		model = model[i+1:]
	}
	return stripDateSuffix(model)
}

// splitSegs splits a possibly-prefixed model string into its lowercased prefix
// segments (a set, for dev/provider disambiguation) and its bare leaf id (the
// last '/'-segment, lowercased, date-stripped). A leading '~' (OpenRouter's
// shadow/variant listing marker) is stripped from each segment.
// e.g. "openrouter/moonshotai/kimi-k3-20260101" → ({openrouter, moonshotai}, "kimi-k3").
func splitSegs(model string) (segs map[string]bool, bare string) {
	segs = map[string]bool{}
	parts := strings.Split(model, "/")
	for _, p := range parts[:len(parts)-1] {
		if p = strings.ToLower(strings.TrimPrefix(p, "~")); p != "" {
			segs[p] = true
		}
	}
	return segs, strings.ToLower(stripDateSuffix(parts[len(parts)-1]))
}

// Normalize reduces a model string to its bare leaf registry key
// (e.g. "openrouter/moonshotai/kimi-k3-20260101" → "kimi-k3"). Exported so
// other packages (e.g. modelcaps) key their caches the same way the registry does.
func Normalize(model string) string {
	return normalize(model)
}

// ContextWindow returns the context window for a model.
// Falls back to family defaults: gemini-1.5-* → 2M, gemini-* → 1M,
// everything else (including claude) → 200k.
func ContextWindow(model string) int {
	segs, bare := splitSegs(model)
	registryMu.RLock()
	m, ok := registryLookupSegs(segs, bare)
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
	segs, bare := splitSegs(model)
	registryMu.RLock()
	m, ok := registryLookupSegs(segs, bare)
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
	segs, bare := splitSegs(model)
	registryMu.RLock()
	m, ok := registryLookupSegs(segs, bare)
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
	segs, bare := splitSegs(model)
	// CC's synthetic sentinel is a zero-cost no-op / session-limit turn: there is
	// nothing to price, and pricing it would spuriously trip the unpriced warning.
	// Check the BARE key (not just the exact string) so a provider-prefixed
	// sentinel — e.g. "openrouter/<synthetic>" from a non-ccstream caller — is
	// caught by the same guard rather than slipping through to noteUnpriced.
	if IsSynthetic(model) || IsSynthetic(bare) {
		return 0
	}
	registryMu.RLock()
	defer registryMu.RUnlock()
	m, ok := registryLookupSegs(segs, bare)
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
			m, _ = registryLookup("claude-haiku-4-5")
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
		return registryLookup("claude-fable-5")
	case strings.Contains(bare, "opus"):
		return registryLookup("claude-opus-4-6")
	case strings.Contains(bare, "sonnet"):
		return registryLookup("claude-sonnet-4-5")
	case strings.Contains(bare, "haiku"):
		return registryLookup("claude-haiku-4-5")
	case strings.Contains(bare, "gemini"):
		return registryLookup("gemini-2.5-flash")
	}
	return Model{}, false
}

// LookupAsOf returns the model attributes effective AT THE GIVEN TIME `at` —
// the latest models.jsonl row for (provider, modelID) whose `fetched` date is
// on or before at's UTC date — rather than Lookup's always-latest-known
// price. Falls back to the earliest available row if `at` predates every
// dated row (baseline/no-fetched rows always qualify, per the `history` var
// doc). ok is false if there is no history at all under this (provider,
// bare) key. See the `history` var doc for the day-granularity/
// observation-date caveats: this is a best-effort reconstruction from the
// data models.jsonl actually records, not an exact historical price.
func LookupAsOf(provider, modelID string, at time.Time) (Model, bool) {
	segs, bare := splitSegs(modelID)
	if p := strings.ToLower(provider); p != "" {
		segs[p] = true
	}
	historyMu.RLock()
	defer historyMu.RUnlock()
	return historyLookupAsOfSegs(segs, bare, at)
}

// historyLookupAsOf is LookupAsOf's body, factored out so CostAsOf can reuse
// it while already holding historyMu (mirrors registryLookup/Lookup's split).
// Provider resolution mirrors registryLookup: provider-specific row set first,
// then providerless, then a sole remaining provider.
func historyLookupAsOfSegs(segs map[string]bool, bare string, at time.Time) (Model, bool) {
	byKey := history[bare]
	if len(byKey) == 0 {
		return Model{}, false
	}
	atDate := at.UTC().Format("2006-01-02")
	pick := func(rows []historyRow) (Model, bool) {
		if len(rows) == 0 {
			return Model{}, false
		}
		// rows is ascending by fetched (parseModelsJSONL/Register append
		// order); pick the latest row whose fetched <= atDate, falling back to
		// the earliest row if `at` predates all of them.
		best := rows[0]
		for _, r := range rows {
			if r.fetched > atDate {
				break
			}
			best = r
		}
		return best.model, true
	}
	// Parallel candidate slices: a representative (latest) model per
	// (provider, dev) group carries the fields pickIndex matches on; groups[i]
	// holds that group's full ascending row history for the as-of pick.
	var reps []Model
	var groups [][]historyRow
	for _, rows := range byKey {
		if len(rows) == 0 {
			continue
		}
		reps = append(reps, rows[len(rows)-1].model)
		groups = append(groups, rows)
	}
	i, ok := pickIndex(reps, segs, bare)
	if !ok {
		return Model{}, false
	}
	return pick(groups[i])
}

// historyLookupAsOf resolves a leaf id as-of `at` with no provider/dev hint
// (familyPricingAsOf targets single-candidate leaves). Caller must hold historyMu.
func historyLookupAsOf(bare string, at time.Time) (Model, bool) {
	return historyLookupAsOfSegs(map[string]bool{}, bare, at)
}

// familyPricingAsOf mirrors familyPricing but resolves the canonical family
// entry's price as of `at` rather than the latest known. Caller must hold
// historyMu.
func familyPricingAsOf(bare string, at time.Time) (Model, bool) {
	switch {
	case strings.Contains(bare, "fable"), strings.Contains(bare, "mythos"):
		return historyLookupAsOf("claude-fable-5", at)
	case strings.Contains(bare, "opus"):
		return historyLookupAsOf("claude-opus-4-6", at)
	case strings.Contains(bare, "sonnet"):
		return historyLookupAsOf("claude-sonnet-4-5", at)
	case strings.Contains(bare, "haiku"):
		return historyLookupAsOf("claude-haiku-4-5", at)
	case strings.Contains(bare, "gemini"):
		return historyLookupAsOf("gemini-2.5-flash", at)
	}
	return Model{}, false
}

// CostAsOf is Cost, but priced using the model's flat per-1M rates AS OF THE
// GIVEN TIME `at` (see LookupAsOf's caveats) instead of the latest known
// price. Used to compute a live estimate for a stored call that has no
// provider-reported ("golden") cost — e.g. by /cost when rendering an old
// api.db row — since the rate recorded in models.jsonl can have moved on
// since that call was actually made. Never persisted: callers recompute
// fresh on every read (foci_todo #1407).
func CostAsOf(model string, at time.Time, input, output, cacheRead, cacheWrite int) float64 {
	segs, bare := splitSegs(model)
	if IsSynthetic(model) || IsSynthetic(bare) {
		return 0
	}
	historyMu.RLock()
	m, ok := historyLookupAsOfSegs(segs, bare, at)
	if !ok {
		m, ok = familyPricingAsOf(bare, at) // caller-holds-lock: mirrors Cost/familyPricing
	}
	historyMu.RUnlock()
	if !ok {
		// noteUnpriced uses its own mutex (unpricedMu), not historyMu.
		noteUnpriced(bare)
		switch {
		case IsOpenAI(bare):
			m = Model{InputPer1M: 5.00, OutputPer1M: 15.00}
		default:
			m, _ = LookupAsOf("", "claude-haiku-4-5", at)
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
	bare := StripPrefix(model)
	for _, p := range []string{"gpt-", "o1", "o3", "o4", "chatgpt-"} {
		if strings.HasPrefix(bare, p) {
			return true
		}
	}
	return false
}
