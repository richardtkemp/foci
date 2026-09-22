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

// FamilyPricedModelHook, if set, is invoked once per distinct bare id that
// resolves to pricing via familyPricing/familyPricingAsOf — i.e. no exact
// registry row for this version, but it inherited its family canonical's
// rates. This is silent-by-design the rest of the time (a new version
// usually DOES match its family), but a version whose true rates diverge from
// the family's — a cheaper cache-read tier, a repriced tier — goes unnoticed
// until the #1674 divergence warning happens to fire on real backend-costed
// traffic, or (if the model is never exercised, or is API-mode with no
// backend-reported cost) not at all. Wired at startup to a log warning so the
// inheritance is visible on the FIRST priced call, not eventually.
var FamilyPricedModelHook func(model string)

var (
	familyPricedMu   sync.Mutex
	familyPricedSeen = map[string]bool{}
)

func noteFamilyPriced(bare string) {
	if FamilyPricedModelHook == nil {
		return
	}
	familyPricedMu.Lock()
	first := !familyPricedSeen[bare]
	familyPricedSeen[bare] = true
	familyPricedMu.Unlock()
	if first {
		FamilyPricedModelHook(bare)
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
	CacheWritePer1M float64 // cost per 1M cache-write tokens (5-minute TTL)

	// CacheWrite1hPer1M is the 1-HOUR cache-write rate (Anthropic bills it at
	// 2x base input). PREFERRED over CacheWritePer1M wherever it is set — see
	// cacheWriteRate. Zero means the registry has no 1h figure for this model,
	// not that 1h caching is free.
	CacheWrite1hPer1M float64
}

// cacheWriteRate returns the rate to price cache-WRITE tokens at.
//
// It prefers the 1-hour rate, reversing the earlier standing ruling that the
// TTL split was immaterial and a single rate would do. Measured 2026-08-06 on
// helen's live session: 273,094 cache-write tokens, of which ephemeral_1h was
// 273,094 and ephemeral_5m was ZERO. Claude Code caches at 1h exclusively, so
// "assume the 5m rate" was wrong for 100% of writes and understated the bill
// by $3.75/M on opus-5 — $1.02 on that session alone, 11.4% of it, and 16.6%
// on the single turn that triggered the divergence warning.
//
// We do not model both rates: nothing in the delegated stream's ModelUsage
// carries the split (it reports a single cacheCreationInputTokens), so there is
// no per-token TTL to branch on even if we wanted one. The 1h rate is simply
// the truthful single rate for the traffic foci actually has.
//
// Safe for the API path too, which requests the 5m default (cache_ttl unset):
// it logged ZERO cache-write tokens in the month to 2026-08-06, so there is
// nothing there to misprice. If that changes, this is the function to split.
//
// Falls back to the 5m rate when the registry carries no 1h figure (only 26 of
// 477 rows do) rather than inventing one from the 2x-input rule, which holds
// for Anthropic but is not a general truth.
func (m Model) cacheWriteRate() float64 {
	if m.CacheWrite1hPer1M > 0 {
		return m.CacheWrite1hPer1M
	}
	return m.CacheWritePer1M
}

// registry maps bare model IDs to provider→Model maps. The "" provider key is
// the providerless/default entry; all built-in entries use "openrouter" (the
// sole-provider fallback means any lookup matches regardless of provider).
// Populated from models.jsonl at init. Guarded by registryMu.
var registry = map[string]map[string]Model{}

// modelFieldsKnown records, per registry row, which of the optional
// capability/context fields the source JSONL row actually SET explicitly —
// as opposed to a Go zero value (false / 0) that really means "the JSON key
// was absent". This distinction matters only for the punctuation-fold retry
// (see punctuationPick / punctuationPickAsOf, #1966/#1969): OpenRouter-synced
// rows routinely omit effort/thinking/speed/caching (and sometimes
// context_window) entirely, while the hand-added rows that share the same
// version under a different spelling are exactly the ones that carry those
// fields. Folding punctuation to find a PRICE must not let a field-sparse
// row's absent-therefore-false values silently overwrite what the family
// fallback would otherwise have supplied for the CAPABILITY fields.
//
// A plain bool can't tell "explicitly false" from "absent" once parsed — see
// jsonlEntry, which uses *bool for these four fields specifically so
// parseModelsJSONL can tell nil (absent) from a real false apart. models.jsonl
// has never actually written an explicit "false" for any of these (checked:
// zero occurrences of "effort":false/"thinking":false/"speed":false/
// "caching":false in the file today) but the pointer distinction is kept
// so a future row that DOES write an explicit false is honoured rather than
// silently misread as "unknown, use the family default".
type modelFieldsKnown struct {
	ContextWindow bool
	Effort        bool
	Thinking      bool
	Speed         bool
	Caching       bool
}

// fullyKnown marks every field as explicitly set — used for Register()
// entries (config overrides / live-apply), which are always caller-supplied
// in full and must never be back-filled with a family fallback via the
// punctuation-fold merge.
var fullyKnown = modelFieldsKnown{ContextWindow: true, Effort: true, Thinking: true, Speed: true, Caching: true}

// knownFields is registry's parallel "which fields are real" map, keyed the
// same way (bare id → provKey). Guarded by registryMu, same as registry.
var knownFields = map[string]map[string]modelFieldsKnown{}

// builtInKnownFields mirrors builtIn for knownFields, so ResetToBuiltIn
// restores both together.
var builtInKnownFields = map[string]map[string]modelFieldsKnown{}

// builtInData is the raw embedded model pricing data, parsed at init.
//
//go:embed models.jsonl
var builtInData []byte

// jsonlEntry is the JSON representation of a model entry in models.jsonl.
// It maps directly to the Model struct; the Comment field is informational
// only and not stored in the registry.
type jsonlEntry struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	Dev           string `json:"dev,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	// Effort/Thinking/Speed/Caching are *bool (not bool) so parseModelsJSONL can
	// tell "the JSON key was absent" (nil) apart from "explicitly false" — see
	// modelFieldsKnown for why that distinction matters.
	Effort          *bool   `json:"effort,omitempty"`
	Thinking        *bool   `json:"thinking,omitempty"`
	Speed           *bool   `json:"speed,omitempty"`
	Caching         *bool   `json:"caching,omitempty"`
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
	// known is model's modelFieldsKnown — see that type's doc. Carried per-row
	// (not just per latest-registry-row) so an as-of lookup that lands on an
	// older row still knows which of ITS fields were real vs absent.
	known modelFieldsKnown
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
	reg, hist, known, err := parseModelsJSONL(builtInData)
	if err != nil {
		panic(err.Error())
	}
	registry = reg
	history = hist
	knownFields = known

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
	for k, v := range knownFields {
		builtInKnownFields[k] = map[string]modelFieldsKnown{}
		for pk, pv := range v {
			builtInKnownFields[k][pk] = pv
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
func parseModelsJSONL(data []byte) (registry map[string]map[string]Model, history map[string]map[string][]historyRow, known map[string]map[string]modelFieldsKnown, err error) {
	registry = map[string]map[string]Model{}
	history = map[string]map[string][]historyRow{}
	known = map[string]map[string]modelFieldsKnown{}
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
			return nil, nil, nil, fmt.Errorf("modelinfo: parse models.jsonl line %q: %v", line, uerr)
		}
		if e.ID == "" {
			return nil, nil, nil, fmt.Errorf("modelinfo: models.jsonl entry missing id: %q", line)
		}
		provider := strings.ToLower(e.Provider)
		id := strings.ToLower(e.ID)
		dev := strings.ToLower(e.Dev)
		key := provKey(provider, dev)
		m := Model{
			Provider:          provider,
			Dev:               dev,
			ContextWindow:     e.ContextWindow,
			Effort:            e.Effort != nil && *e.Effort,
			Thinking:          e.Thinking != nil && *e.Thinking,
			Speed:             e.Speed != nil && *e.Speed,
			Caching:           e.Caching != nil && *e.Caching,
			InputPer1M:        e.InputPer1M,
			OutputPer1M:       e.OutputPer1M,
			CacheReadPer1M:    e.CacheReadPer1M,
			CacheWritePer1M:   e.CacheWritePer1M,
			CacheWrite1hPer1M: e.CacheWrite1hPer1M,
		}
		fieldsKnown := modelFieldsKnown{
			ContextWindow: e.ContextWindow != 0,
			Effort:        e.Effort != nil,
			Thinking:      e.Thinking != nil,
			Speed:         e.Speed != nil,
			Caching:       e.Caching != nil,
		}

		if history[id] == nil {
			history[id] = map[string][]historyRow{}
		}
		history[id][key] = append(history[id][key], historyRow{fetched: e.Fetched, model: m, known: fieldsKnown})

		if registry[id] == nil {
			registry[id] = map[string]Model{}
			known[id] = map[string]modelFieldsKnown{}
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
		known[id][key] = fieldsKnown
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
	return registry, history, known, nil
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
	// A Register call is always caller-supplied in full (config override /
	// live-apply) — mark every field known so a later punctuation-fold match
	// (#1966/#1969) never back-fills it with a family default it doesn't need.
	if knownFields[modelID] == nil {
		knownFields[modelID] = map[string]modelFieldsKnown{}
	}
	knownFields[modelID][key] = fullyKnown
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
	history[modelID][key] = append(history[modelID][key], historyRow{fetched: today, model: m, known: fullyKnown})
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
// segment set. A routing-variant leaf (":x") is tried EXACT first, then falls
// back to its base — so a distinct variant entry keeps its own attributes while
// an unlisted variant inherits the base's. If neither the exact leaf nor its
// variant-stripped base has a registry row, retry both treating '.' and '-' as
// interchangeable (see punctuationPick) before giving up — this is still an
// EXACT hit by another spelling, so it is tried before the caller falls
// through to family pricing, not after. Caller must hold registryMu.
func registryLookupSegs(segs map[string]bool, bare string) (Model, bool) {
	if m, ok := registryPick(segs, bare); ok {
		return m, true
	}
	base := stripVariantSuffix(bare)
	if base != bare {
		if m, ok := registryPick(segs, base); ok {
			return m, true
		}
	}
	if m, ok := punctuationPick(segs, bare); ok {
		return m, true
	}
	if base != bare {
		return punctuationPick(segs, base)
	}
	return Model{}, false
}

// punctFold canonicalizes '.' to '-' so a dotted and a hyphenated spelling of
// the same id compare equal. It folds ONLY the punctuation characters — every
// other rune, and its position, is preserved — so it is a precise
// punctuation-only equivalence, never a fuzzy or substring match:
// "claude-opus-4-8" and "claude-opus-4.8" fold to the same string;
// "claude-opus-48" does not (different length/position).
func punctFold(s string) string {
	return strings.ReplaceAll(s, ".", "-")
}

// punctuationPick retries a registry leaf lookup with '.' and '-' folded
// together — OpenRouter's models.jsonl catalogue spells versions with dots
// (claude-opus-4.8) while Claude Code reports the same model with hyphens
// (claude-opus-4-8); an exact lookup misses on that one character even though
// the price is already in the registry under the other spelling. Only called
// after the exact leaf has already missed (registryLookupSegs tries that
// first), and `bare` itself is excluded from the scan since it's already been
// tried and failed.
//
// AMBIGUITY RULE: more than one OTHER registry key can fold to the same form.
// If every Model reachable under those keys is identical, that's the expected
// dot/hyphen duplicate of a single model and resolves to it (also running the
// usual provider/dev disambiguation within that one key, via registryPick).
// If they genuinely differ, that is a real collision, not a match — refuse
// (return false) rather than guess, so a punctuation-folded lookup can never
// produce a silently wrong price. A refusal here is not a dead end: the
// caller (registryLookupSegs) returns ok=false and its own caller (Cost, …)
// falls through to familyPricing exactly as it would for any other miss.
//
// FIELD-LEVEL MERGE (#1969): before returning, the matched row's
// capability/context fields are passed through fillUnknownFields, which
// back-fills anything the SOURCE row left unset with the ordinary family
// default. This is necessary because the two spellings are not always full
// duplicates of each other in practice: OpenRouter-synced dot-form rows
// routinely omit effort/thinking/speed/caching, while a hand-added
// hyphen-form row of the same version is exactly where those fields live
// (e.g. claude-opus-4.6 has none of them set; claude-opus-4-6 has all three
// true). Pricing fields are exempt — a punctuation match IS authoritative for
// price (that's this ticket's whole purpose), and a zero rate can be a real
// free-tier price, so there is no "unknown, use a default" case for them the
// way there is for a bool/int capability field.
func punctuationPick(segs map[string]bool, bare string) (Model, bool) {
	folded := punctFold(bare)
	var matchKeys []string
	for key := range registry {
		if key != bare && punctFold(key) == folded {
			matchKeys = append(matchKeys, key)
		}
	}
	switch len(matchKeys) {
	case 0:
		return Model{}, false
	case 1:
		m, ok := registryPick(segs, matchKeys[0])
		if !ok {
			return Model{}, false
		}
		return fillUnknownFields(m, knownFields[matchKeys[0]][provKey(m.Provider, m.Dev)], bare), true
	}
	var all []Model
	var allKnown []modelFieldsKnown
	for _, key := range matchKeys {
		for _, m := range candidates(key) {
			all = append(all, m)
			allKnown = append(allKnown, knownFields[key][provKey(m.Provider, m.Dev)])
		}
	}
	if len(all) == 0 {
		return Model{}, false
	}
	for _, m := range all[1:] {
		if m != all[0] {
			return Model{}, false // genuine collision, not a duplicate — refuse
		}
	}
	return fillUnknownFields(all[0], allKnown[0], bare), true
}

// fillUnknownFields back-fills any capability/context field a punctuation-fold
// match left UNKNOWN (its source row never set it — see modelFieldsKnown) with
// the same family default the caller would have used on a total miss. See
// punctuationPick's FIELD-LEVEL MERGE section for why this exists. Pricing
// fields are untouched — see the same section for why they're exempt.
func fillUnknownFields(m Model, known modelFieldsKnown, bare string) Model {
	if !known.ContextWindow {
		m.ContextWindow = contextWindowFallback(bare)
	}
	if !known.Effort || !known.Thinking || !known.Speed {
		effort, thinking, speed := capabilitiesFallback(bare)
		if !known.Effort {
			m.Effort = effort
		}
		if !known.Thinking {
			m.Thinking = thinking
		}
		if !known.Speed {
			m.Speed = speed
		}
	}
	if !known.Caching {
		m.Caching = cachingFallback(bare)
	}
	return m
}

// registryPick resolves exactly the given leaf (no variant fallback).
// Caller must hold registryMu.
func registryPick(segs map[string]bool, bare string) (Model, bool) {
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
	knownFields = make(map[string]map[string]modelFieldsKnown, len(builtInKnownFields))
	for k, v := range builtInKnownFields {
		inner := make(map[string]modelFieldsKnown, len(v))
		for pk, pv := range v {
			inner[pk] = pv
		}
		knownFields[k] = inner
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

// stripVariantSuffix removes a trailing OpenRouter routing-variant suffix
// (":floor", ":nitro", ":free", ":thinking", …) from a model leaf. These are
// provider-ROUTING modifiers, not distinct models — they carry the base
// model's pricing and caps — so a variant leaf that is NOT its own registry
// entry resolves to the base. e.g. "deepseek-v4-pro:floor" → "deepseek-v4-pro".
// A model id never contains a ':' except as this separator, so splitting on the
// first ':' is safe. Applied as a lookup FALLBACK (exact leaf first), so a
// variant that IS a distinct registry entry keeps its own attributes.
func stripVariantSuffix(model string) string {
	if i := strings.IndexByte(model, ':'); i >= 0 {
		return model[:i]
	}
	return model
}

// normalize reduces a model string to its bare leaf id — the segment after the
// LAST '/' (OpenRouter ids are host/dev/model or dev/model, so the leaf is the
// registry key), with the date suffix stripped. The routing-variant (":x")
// suffix is NOT stripped here: the lookup tries the exact variant leaf first
// and only falls back to the base, so a distinct variant entry keeps its own
// attributes. Casing is preserved (modelcaps relies on it for cache keys).
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
//
// An unregistered anthropic family member (opus/sonnet/fable/haiku) is priced
// off the newest plain member of that family, same as familyPricing — unlike
// Capabilities below, models.jsonl's OpenRouter-synced rows DO carry
// context_window on every row inspected (foci_todo #1967), so following
// "newest" doesn't land on a field-sparse row here. Falls back to a literal
// only where the registry has no anthropic family match at all: gemini-1.5-*
// → 2M, gemini-* → 1M, everything else → 200k.
func ContextWindow(model string) int {
	segs, bare := splitSegs(model)
	registryMu.RLock()
	defer registryMu.RUnlock()
	if m, ok := registryLookupSegs(segs, bare); ok {
		return m.ContextWindow
	}
	var anthropicFamily string
	switch {
	case strings.Contains(bare, "fable"), strings.Contains(bare, "mythos"):
		anthropicFamily = "fable"
	case strings.Contains(bare, "opus"):
		anthropicFamily = "opus"
	case strings.Contains(bare, "sonnet"):
		anthropicFamily = "sonnet"
	case strings.Contains(bare, "haiku"):
		anthropicFamily = "haiku"
	}
	if anthropicFamily != "" {
		if fam, ok := familyCanonicalPrice(anthropicFamily); ok && fam.ContextWindow > 0 {
			return fam.ContextWindow
		}
	}
	return contextWindowFallback(bare)
}

// contextWindowFallback is ContextWindow's LITERAL family-default table,
// factored out so punctuationPick's field-level merge (#1969) can apply the
// same defaults to just the fields a folded row left unset, not only to a
// total miss.
//
// Deliberately does NOT do the newest-in-family resolution ContextWindow does
// above it (#1967), and this must not be "simplified" into it: the as-of path
// reaches here via punctuationPickAsOf -> fillUnknownFields holding only
// historyMu, while newestInFamilyLocked reads `registry` and requires
// registryMu. Folding the family lookup in here would make that an unlocked
// registry read — a data race the tests would not reliably catch.
func contextWindowFallback(bare string) int {
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
//
// DELIBERATELY NOT routed through familyCanonicalPrice/newest-in-family, unlike
// familyPricing and ContextWindow (foci_todo #1967) — this is the opposite
// choice, on purpose. The newest rows in models.jsonl are OpenRouter-synced
// and leave effort/thinking/speed UNSET (nil), so "follow newest" would land
// on a field-sparse row and answer false/false/false for a model that plainly
// has these capabilities — precisely the regression 9eabc7e7 (foci_todo #1966)
// just fixed. Nothing else in the catalogue carries capability data at all, so
// this hand-written family table is the ONLY authority for it and must stay a
// literal. A later reader may be tempted to "tidy" this into consistency with
// familyPricing/ContextWindow below — don't; that reintroduces the bug.
func Capabilities(model string) (effort, thinking, speed bool) {
	segs, bare := splitSegs(model)
	registryMu.RLock()
	m, ok := registryLookupSegs(segs, bare)
	registryMu.RUnlock()
	if ok {
		return m.Effort, m.Thinking, m.Speed
	}
	return capabilitiesFallback(bare)
}

// capabilitiesFallback is Capabilities' family-default table, factored out for
// the same reason as contextWindowFallback — see its doc comment.
func capabilitiesFallback(bare string) (effort, thinking, speed bool) {
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
// Unlike ContextWindow/familyPricing, this fallback carries no per-version
// literal to go stale (foci_todo #1967) — it is already family-agnostic
// ("any claude id" → true), so there was nothing here for newest-in-family to
// fix. Checked, not assumed: models.jsonl's `caching` field IS populated on
// the newest row of every family inspected, so this would be safe to route
// through familyCanonicalPrice too if a per-family literal ever gets added
// here — it just isn't needed today.
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
	return cachingFallback(bare)
}

// cachingFallback is Caching's family-default rule, factored out for the same
// reason as contextWindowFallback — see its doc comment.
func cachingFallback(bare string) bool {
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
		if m, ok = familyPricing(bare); ok { // caller-holds-lock: Cost holds RLock
			// noteFamilyPriced uses its own mutex (familyPricedMu), not registryMu.
			noteFamilyPriced(bare)
		}
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
		float64(cacheWrite)/mtok*m.cacheWriteRate()
}

// familyPricing maps a bare model name to a canonical per-family price entry by
// family keyword, so pricing tracks the family ("opus costs this much") rather
// than an exact version string. The canonical entry is the NEWEST plain
// (non-variant) member of the family currently in the registry — resolved via
// newestInFamilyLocked, not a hand-picked literal, so it stops going stale
// every time a new version ships (foci_todo #1967: a claude-opus-4-6 literal
// fetched 2026-02-04 was still being used to price claude-opus-4-8 seven
// months later, by luck rather than design). Caller must hold registryMu.
//
// gemini is the one exception, kept as a literal: its ids carry a VARIANT name
// ("flash"/"pro"), not a trailing version number, so familyVersion's
// all-numeric-after-the-family-token rule (see NewestInFamily) never matches
// any gemini id and newestInFamilyLocked would always report ok=false.
func familyPricing(bare string) (Model, bool) {
	switch {
	case strings.Contains(bare, "fable"), strings.Contains(bare, "mythos"):
		return familyCanonicalPrice("fable")
	case strings.Contains(bare, "opus"):
		return familyCanonicalPrice("opus")
	case strings.Contains(bare, "sonnet"):
		return familyCanonicalPrice("sonnet")
	case strings.Contains(bare, "haiku"):
		return familyCanonicalPrice("haiku")
	case strings.Contains(bare, "gemini"):
		return registryLookup("gemini-2.5-flash")
	}
	return Model{}, false
}

// familyCanonicalPrice resolves the newest plain member of the given
// anthropic model family in the registry and returns its price row. Every
// caller passes "anthropic" — the only dev in the registry whose ids carry a
// trailing numeric version (see familyPricing's gemini comment) — so that
// argument to newestInFamilyLocked is fixed here rather than threaded through
// as a parameter unparam would flag as always-constant. Caller must hold
// registryMu.
func familyCanonicalPrice(family string) (Model, bool) {
	id, ok := newestInFamilyLocked("anthropic", family)
	if !ok {
		return Model{}, false
	}
	return registryLookup(id)
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
// historyLookupAsOfSegs tries the exact variant leaf first, then falls back to
// the base leaf, then the punctuation-folded retry on each (mirrors
// registryLookupSegs — see punctuationPick/punctuationPickAsOf for the
// dot/hyphen equivalence and its ambiguity rule). Caller must hold historyMu.
func historyLookupAsOfSegs(segs map[string]bool, bare string, at time.Time) (Model, bool) {
	if m, ok := historyPickAsOf(segs, bare, at); ok {
		return m, true
	}
	base := stripVariantSuffix(bare)
	if base != bare {
		if m, ok := historyPickAsOf(segs, base, at); ok {
			return m, true
		}
	}
	if m, ok := punctuationPickAsOf(segs, bare, at); ok {
		return m, true
	}
	if base != bare {
		return punctuationPickAsOf(segs, base, at)
	}
	return Model{}, false
}

// punctuationPickAsOf mirrors punctuationPick for the as-of/history path: it
// retries with '.' and '-' folded together, resolving each candidate key's
// price AS OF `at` (via historyRowAsOf, which applies the usual provider/dev
// disambiguation). Same ambiguity rule as punctuationPick — a single other
// folded-matching key resolves directly; several that resolve (as of `at`) to
// identical Models resolve to that shared value; several that diverge refuse,
// so the caller falls through to familyPricingAsOf. Also mirrors
// punctuationPick's FIELD-LEVEL MERGE: the resolved row's capability/context
// fields are back-filled via fillUnknownFields using THAT ROW's own known
// bits (not the latest row's — an as-of match can land on an older row with a
// different known-set). Caller must hold historyMu.
func punctuationPickAsOf(segs map[string]bool, bare string, at time.Time) (Model, bool) {
	folded := punctFold(bare)
	var matchKeys []string
	for key := range history {
		if key != bare && punctFold(key) == folded {
			matchKeys = append(matchKeys, key)
		}
	}
	switch len(matchKeys) {
	case 0:
		return Model{}, false
	case 1:
		row, ok := historyRowAsOf(segs, matchKeys[0], at)
		if !ok {
			return Model{}, false
		}
		return fillUnknownFields(row.model, row.known, bare), true
	}
	var all []historyRow
	for _, key := range matchKeys {
		if row, ok := historyRowAsOf(segs, key, at); ok {
			all = append(all, row)
		}
	}
	if len(all) == 0 {
		return Model{}, false
	}
	for _, row := range all[1:] {
		if row.model != all[0].model {
			return Model{}, false // genuine collision, not a duplicate — refuse
		}
	}
	return fillUnknownFields(all[0].model, all[0].known, bare), true
}

// historyPickAsOf resolves exactly the given leaf as-of `at` (no variant
// fallback). Caller must hold historyMu.
func historyPickAsOf(segs map[string]bool, bare string, at time.Time) (Model, bool) {
	row, ok := historyRowAsOf(segs, bare, at)
	if !ok {
		return Model{}, false
	}
	return row.model, true
}

// historyRowAsOf is historyPickAsOf's body, additionally returning the full
// historyRow (not just its Model) so punctuationPickAsOf can read the row's
// own modelFieldsKnown for the field-level merge (#1969) — the ordinary exact
// / variant-stripped callers only need the Model. Caller must hold historyMu.
func historyRowAsOf(segs map[string]bool, bare string, at time.Time) (historyRow, bool) {
	byKey := history[bare]
	if len(byKey) == 0 {
		return historyRow{}, false
	}
	atDate := at.UTC().Format("2006-01-02")
	pick := func(rows []historyRow) (historyRow, bool) {
		if len(rows) == 0 {
			return historyRow{}, false
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
		return best, true
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
		return historyRow{}, false
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
	// A flat cache-write figure carries no TTL, so it maps to Unknown — which
	// prices at cacheWriteRate(), exactly what this function did before the
	// split existed. Behaviour-preserving by construction, and the existing
	// cost tests are the proof.
	return CostAsOfSplit(model, at, input, output, cacheRead, CacheWrites{Unknown: cacheWrite})
}

// CacheWrites is cache-write tokens separated by the TTL they were written at.
//
// Unknown is its own class rather than being folded into either rate. CC
// reporting cache-write tokens with no breakdown means the TTL was NOT
// OBSERVED, which is a different fact from "they were 5m" — and assuming
// either one is the mistake that caused #1866. It prices at the 1h rate: the
// higher of the two, so an unobserved TTL errs toward over-charging, and the
// pre-split behaviour is preserved exactly.
type CacheWrites struct {
	Ephemeral5m int
	Ephemeral1h int
	Unknown     int
}

// CostAsOfSplit is CostAsOf with cache writes separated by TTL.
//
// Ephemeral5m prices at Model.CacheWritePer1M (the 5-minute rate);
// Ephemeral1h and Unknown price at Model.cacheWriteRate(), which prefers the
// registry's 1h figure and falls back to the 5m one where no 1h rate exists.
//
// This exists because Claude Code's MAIN THREAD caches at 1h while its
// SUBAGENTS cache at 5m, and foci priced every write at the 1h rate — a 60%
// overcharge on subagent writes, reconciled to six decimals on four separate
// production turns (#1866). The split is observable only on per-message usage;
// the result's ModelUsage merges it into one figure, so a turn summarised from
// the result alone can no longer be priced correctly.
func CostAsOfSplit(model string, at time.Time, input, output, cacheRead int, w CacheWrites) float64 {
	segs, bare := splitSegs(model)
	if IsSynthetic(model) || IsSynthetic(bare) {
		return 0
	}
	historyMu.RLock()
	m, ok := historyLookupAsOfSegs(segs, bare, at)
	if !ok {
		if m, ok = familyPricingAsOf(bare, at); ok { // caller-holds-lock: mirrors Cost/familyPricing
			// noteFamilyPriced uses its own mutex (familyPricedMu), not historyMu.
			noteFamilyPriced(bare)
		}
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
		float64(w.Ephemeral5m)/mtok*m.CacheWritePer1M +
		float64(w.Ephemeral1h+w.Unknown)/mtok*m.cacheWriteRate()
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

// NewestInFamily returns the newest registered model id for one developer's
// model family — NewestInFamily("anthropic", "opus") → "claude-opus-5".
//
// It exists so callers that mean "the current opus" stop hand-maintaining a
// literal that goes stale on every release. provision.ResolveModelAlias was a
// fixed map and had drifted three of its four rows behind the models actually
// in use (opus→4-6 while every real turn ran opus-5) — a map keyed by a name
// whose whole meaning is "the newest one" cannot help drifting.
//
// Newest is decided by the VERSION SEGMENTS THAT FOLLOW the family token in the
// id, compared as a numeric tuple: claude-fable-5-1 [5 1] beats claude-fable-5
// [5]; claude-opus-5 [5] beats claude-opus-4-6 [4 6]. Both spellings the
// registry carries are accepted ("4-6" and "4.6" both parse to [4 6]); on a tie
// the dash spelling wins, because that is the form Anthropic's own API uses.
//
// A candidate must be ALL-NUMERIC after the family token, which is what keeps
// the aliases pointing at plain, current models: it rejects claude-opus-latest
// (a moving pointer), claude-opus-5-fast and claude-opus-5[1m] (priced variants
// of a model, not newer models), and claude-3-haiku (whose version precedes the
// family token, so it offers no version at all and cannot outrank haiku-4-5).
//
// Returns ok=false when nothing in the registry matches, so callers keep a
// literal fallback rather than propagating an empty model id.
//
// Deliberately NOT codex's compareModelVersions (delegator/codex/model_resolver.go),
// which ranks by every numeric run anywhere in an id and ignores non-numeric
// text. That is right for its input — a curated app-server catalogue where each
// entry is a selectable model — and wrong for this one: on the registry it reads
// claude-opus-5[1m] as [5 1] and ranks it ABOVE claude-opus-5. The registry
// mixes real models with price/context variants and moving pointers, so the
// non-numeric text is exactly what has to be honoured here.
func NewestInFamily(dev, family string) (string, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return newestInFamilyLocked(dev, family)
}

// newestInFamilyLocked is NewestInFamily's body, factored out so familyPricing
// (and other registryMu-holding callers, mirroring registryLookup) can reuse
// the exact same ranking without a recursive RLock — sync.RWMutex's RLock is
// NOT safe to call twice on the same goroutine, because a Lock() request
// queued in between the two RLocks blocks the second one, deadlocking against
// itself (foci_todo #1967). Caller must hold registryMu.
func newestInFamilyLocked(dev, family string) (string, bool) {
	dev, family = strings.ToLower(dev), strings.ToLower(family)
	var bestID string
	var bestVer []int
	for id, byKey := range registry {
		matchesDev := false
		for _, m := range byKey {
			if m.Dev == dev {
				matchesDev = true
				break
			}
		}
		if !matchesDev {
			continue
		}
		ver, ok := familyVersion(id, family)
		if !ok {
			continue
		}
		if bestID == "" || versionLess(bestVer, ver) ||
			(!versionLess(ver, bestVer) && preferredSpelling(id, bestID)) {
			bestID, bestVer = id, ver
		}
	}
	return bestID, bestID != ""
}

// familyVersion extracts the numeric version following the family token in a
// model id — ("claude-fable-5-1", "fable") → [5 1]. It reports false unless
// `family` appears as a whole dash-separated segment AND every segment after it
// is purely numeric (dots allowed, so the registry's "4.6" spelling parses like
// its "4-6" one). See NewestInFamily for why that strictness is the point.
func familyVersion(id, family string) ([]int, bool) {
	segs := strings.Split(id, "-")
	at := -1
	for i, s := range segs {
		if s == family {
			at = i
			break
		}
	}
	if at < 0 || at == len(segs)-1 {
		return nil, false
	}
	var ver []int
	for _, s := range segs[at+1:] {
		for _, part := range strings.Split(s, ".") {
			if part == "" {
				return nil, false
			}
			n := 0
			for _, r := range part {
				if r < '0' || r > '9' {
					return nil, false
				}
				n = n*10 + int(r-'0')
			}
			ver = append(ver, n)
		}
	}
	return ver, true
}

// versionLess orders version tuples element-wise, treating a missing element as
// lower, so [5] < [5 1] and [4 6] < [5].
func versionLess(a, b []int) bool {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// preferredSpelling breaks a version tie between two ids for the same model
// (the registry carries both "claude-haiku-4-5" and "claude-haiku-4.5"). Prefer
// the dash form — Anthropic's own API ids use dashes, and the alias result is
// sent to that API. Falls back to lexical order so the pick is deterministic
// whatever the map iteration order.
func preferredSpelling(candidate, current string) bool {
	cDot, curDot := strings.Contains(candidate, "."), strings.Contains(current, ".")
	if cDot != curDot {
		return !cDot
	}
	return candidate < current
}
