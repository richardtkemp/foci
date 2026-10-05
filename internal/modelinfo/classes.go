package modelinfo

import (
	"maps"
	"slices"
)

// Class is one billable token class: a kind of unit a provider bills at its own
// rate. Classes are DATA, not columns (#2111 R1): a call is a (class -> count)
// map, a model is a (class -> rate) map, and adding a class needs one entry in
// the vocabulary below and a rate — no schema change anywhere.
//
// Classes are DISJOINT and ADDITIVE: every billed unit is counted in exactly one
// class. Each backend adapter normalises its provider's overlapping fields into
// disjoint classes before booking (codex's cachedInputTokens is taken out of
// inputTokens, for example).
type Class string

// The vocabulary. docs/WIRING.md "Cost ledger" has the per-backend mapping.
const (
	ClassInput     Class = "input"
	ClassOutput    Class = "output"
	ClassCacheRead Class = "cache_read"
	// ClassCacheWrite5m and ClassCacheWrite1h are cache writes whose TTL was
	// OBSERVED (CC per-message usage) or fixed by a backend rule (the direct
	// API, always 1h per Dick).
	ClassCacheWrite5m Class = "cache_write_5m"
	ClassCacheWrite1h Class = "cache_write_1h"
	// ClassCacheWrite is a cache write whose TTL was not reported (codex,
	// opencode, legacy rows). It prices at the higher of the two write rates,
	// which is the 1h rate for Anthropic models and the provider's single rate
	// otherwise, so an unknown TTL errs toward over-charging.
	ClassCacheWrite Class = "cache_write"
	// ClassReasoning is reasoning output billed ON TOP of output (opencode,
	// #2112 P0-d). codex does not use it: its reasoning tokens are inside
	// outputTokens already.
	ClassReasoning Class = "reasoning"
	// ClassWebSearch and ClassWebFetch are billed per REQUEST, not per token
	// (#1913).
	ClassWebSearch Class = "web_search"
	ClassWebFetch  Class = "web_fetch"
)

// Unit is how a class's count is priced.
type Unit string

const (
	// UnitMTok: the rate is USD per million units.
	UnitMTok Unit = "mtok"
	// UnitEach: the rate is USD per unit.
	UnitEach Unit = "each"
)

// PerUnits is how many counted units one rate figure covers.
func (u Unit) PerUnits() float64 {
	if u == UnitMTok {
		return 1_000_000
	}
	return 1
}

// classInfo is one vocabulary entry.
type classInfo struct {
	unit Unit
	// context marks a class that fills the model's context window, so a call's
	// context fill is the sum of these classes (the turn_costs view reads it).
	context bool
}

var vocabulary = map[Class]classInfo{
	ClassInput:        {unit: UnitMTok, context: true},
	ClassOutput:       {unit: UnitMTok},
	ClassCacheRead:    {unit: UnitMTok, context: true},
	ClassCacheWrite5m: {unit: UnitMTok, context: true},
	ClassCacheWrite1h: {unit: UnitMTok, context: true},
	ClassCacheWrite:   {unit: UnitMTok, context: true},
	ClassReasoning:    {unit: UnitMTok},
	ClassWebSearch:    {unit: UnitEach},
	ClassWebFetch:     {unit: UnitEach},
}

// Classes returns the vocabulary, sorted.
func Classes() []Class {
	return slices.Sorted(maps.Keys(vocabulary))
}

// ClassUnit returns c's unit. ok is false for a class outside the vocabulary.
func ClassUnit(c Class) (Unit, bool) {
	info, ok := vocabulary[c]
	return info.unit, ok
}

// InContext reports whether c counts toward a call's context fill.
func InContext(c Class) bool { return vocabulary[c].context }

// Tokens is one call's billed counts, by class. A zero or absent class means
// nothing was billed in it.
type Tokens map[Class]int

// Prices is a model's rates in the NAMED-FIGURE shape the sources carry
// (models.jsonl rows and [[modelinfo]] config sections). It exists only to be
// turned into a class-rate map by Rates, which is the one place the sources'
// fields are normalised into classes.
type Prices struct {
	Input             float64 // per 1M input tokens
	Output            float64 // per 1M output tokens
	CacheRead         float64 // per 1M cache-read tokens
	CacheWrite5m      float64 // per 1M cache-write tokens, 5-minute TTL
	CacheWrite1h      float64 // per 1M cache-write tokens, 1-hour TTL
	InternalReasoning float64 // per 1M reasoning tokens, when published separately
	WebSearch         float64 // per search request
	// CacheReadSet / CacheWriteSet mark CacheRead / CacheWrite5m as a real
	// price even at zero: a [[modelinfo]] override that writes 0.0 (a
	// flat-rate plan, #2172) means $0, unlike a source that left it out.
	CacheReadSet  bool
	CacheWriteSet bool
}

// Rates normalises p into class rates. A zero figure in the source means "not
// published", so the class is left out and a call billed in it is UNPRICED
// (never a silent $0) — except input and output, which every priced row
// carries and where zero is a real free-tier price, and a cache figure marked
// Set, which is a real price at zero.
//
//   - cache_write_1h and cache_write price at the higher write figure. When
//     the source has no 1h figure (most non-Anthropic rows), the provider's
//     single write rate stands in; a 1h figure below the 5m one is a data
//     error and is ignored. This is exactly how the pre-class code priced a
//     cache write of unknown or 1h TTL, so no price moves.
//   - reasoning prices at the published reasoning figure, else at the output
//     rate (#2112 P0-d: opencode bills reasoning on top of output, at the
//     output rate).
//   - web_fetch is always present at zero: it is not billed separately today,
//     but its count is kept (#2111 §2.1).
func (p Prices) Rates() map[Class]float64 {
	r := map[Class]float64{
		ClassInput:    p.Input,
		ClassOutput:   p.Output,
		ClassWebFetch: 0,
	}
	if p.CacheRead > 0 || p.CacheReadSet {
		r[ClassCacheRead] = p.CacheRead
	}
	if p.CacheWrite5m > 0 || p.CacheWriteSet {
		r[ClassCacheWrite5m] = p.CacheWrite5m
	}
	if w := max(p.CacheWrite5m, p.CacheWrite1h); w > 0 || p.CacheWriteSet {
		r[ClassCacheWrite1h] = w
		r[ClassCacheWrite] = w
	}
	r[ClassReasoning] = p.Output
	if p.InternalReasoning > 0 {
		r[ClassReasoning] = p.InternalReasoning
	}
	if p.WebSearch > 0 {
		r[ClassWebSearch] = p.WebSearch
	}
	return r
}

// Prices recovers the named figures m's rates were built from, so a caller can
// change one figure and rebuild the rates through the same normalisation
// (config overrides merge this way). Lossless for any rate map Prices.Rates
// produced.
func (m Model) Prices() Prices {
	_, readSet := m.Rates[ClassCacheRead]
	_, writeSet := m.Rates[ClassCacheWrite5m]
	p := Prices{
		Input:         m.Rates[ClassInput],
		Output:        m.Rates[ClassOutput],
		CacheRead:     m.Rates[ClassCacheRead],
		CacheWrite5m:  m.Rates[ClassCacheWrite5m],
		WebSearch:     m.Rates[ClassWebSearch],
		CacheReadSet:  readSet,
		CacheWriteSet: writeSet,
	}
	if w := m.Rates[ClassCacheWrite1h]; w > p.CacheWrite5m {
		p.CacheWrite1h = w
	}
	if r := m.Rates[ClassReasoning]; r != p.Output {
		p.InternalReasoning = r
	}
	return p
}
