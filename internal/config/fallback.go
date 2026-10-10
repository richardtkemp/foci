package config

import (
	"sort"
	"strings"
)

// MaxFallbackDepth is the maximum number of fallback hops allowed per request.
const MaxFallbackDepth = 3

// sortedKeys returns the map's keys in sorted order, for deterministic
// iteration over config maps (stable validation errors, deterministic
// merges).
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// FallbackResolver resolves model fallbacks for automatic failover.
// Keys are case-folded canonical "developer/model_id" match keys (ModelKey);
// values are canonical "developer/model_id" strings with the case written in
// config. Returns nil from constructor when both maps are empty (no-op fast
// path).
type FallbackResolver struct {
	fallbacks map[string]string // folded match key → canonical fallback (config case)
}

// NewFallbackResolver creates a FallbackResolver by merging global and per-agent
// fallback maps (per-agent wins). All keys and values are normalized through
// named model configs to canonical "developer/model_id" format — keys are then
// case-folded (ModelKey) so matching ignores case, while values keep the case
// written in config (the spelling the provider receives). Cycles are detected
// and broken. Returns nil if both maps are empty.
func NewFallbackResolver(global, perAgent map[string]string, models map[string]ModelConfig) *FallbackResolver {
	if len(global) == 0 && len(perAgent) == 0 {
		return nil
	}

	// Global entries first, then per-agent overrides. Each level folds
	// deterministically (foldFallbacks).
	merged := foldFallbacks(global, models)
	for k, v := range foldFallbacks(perAgent, models) {
		merged[k] = v
	}

	if len(merged) == 0 {
		return nil
	}

	// Break cycles: walk each chain and remove the edge that creates a cycle.
	breakCycles(merged)

	if len(merged) == 0 {
		return nil
	}

	return &FallbackResolver{fallbacks: merged}
}

// foldFallbacks folds ONE fallback table to its match-key map: keys through
// ModelKey (matching ignores case), values canonicalized with the case
// written in config. Keys are inserted in sorted order so two keys that fold
// to the same match key (differing only by case) keep a deterministic winner
// — the first in sorted order. Validate rejects such tables; this only keeps
// resolvers built outside validation (tests, programmatic use) from flipping
// on map iteration order.
func foldFallbacks(m map[string]string, models map[string]ModelConfig) map[string]string {
	out := make(map[string]string, len(m))
	for _, k := range sortedKeys(m) {
		ck := ModelKey(canonicalize(k, models))
		cv := canonicalize(m[k], models)
		if ck == "" || cv == "" {
			continue
		}
		if _, dup := out[ck]; dup {
			continue
		}
		out[ck] = cv
	}
	return out
}

// Resolve returns the fallback model for the given model, or nil if no
// fallback is configured. The input is matched case-insensitively through
// the same key used at construction time; the returned ModelID keeps the
// value's config spelling.
func (fr *FallbackResolver) Resolve(model string) *ResolvedModel {
	if fr == nil {
		return nil
	}
	// Canonicalize using a nil alias map since keys are already canonical.
	// We need to handle bare model IDs though — try direct lookup first,
	// then try with just splitting.
	key := ModelKey(model)
	fb, ok := fr.fallbacks[key]
	if !ok {
		return nil
	}

	// Parse the canonical fallback into a ResolvedModel
	resolved, err := ResolveModel(fb, "", nil)
	if err != nil {
		return nil
	}
	return resolved
}

// canonicalize resolves a model string (named model or developer/model_id) to
// canonical "developer/model_id" format, keeping the model id's config case.
// Returns "" if unresolvable.
func canonicalize(model string, models map[string]ModelConfig) string {
	resolved, err := ResolveModel(model, "", models)
	if err != nil {
		return ""
	}
	return resolved.Developer + "/" + resolved.ModelID
}

// ModelKey returns the case-insensitive match key for a model string: the
// canonical developer/model_id lowercased in full (the raw string lowercased
// when it does not resolve). Matching model config ([models.*] per-model
// settings, [groups.fallbacks] keys) uses this key; the model id sent to a
// provider keeps the case written in config.
func ModelKey(model string) string {
	// If it already has a slash, parse directly
	resolved, err := ResolveModel(model, "", nil)
	if err != nil {
		return strings.ToLower(model)
	}
	return strings.ToLower(resolved.Developer + "/" + resolved.ModelID)
}

// breakCycles detects and removes edges that would create cycles in the
// fallback chain. For each key, walks the chain; if it revisits a node,
// deletes the edge from that node. Keys are folded match keys while values
// keep config case, so the walk follows an edge through ModelKey — the
// visited set and the next-step lookup use the same normalisation as the
// map's keys, never the raw value.
func breakCycles(m map[string]string) {
	for start := range m {
		visited := map[string]bool{start: true}
		cur := start
		for {
			next, ok := m[cur]
			if !ok {
				break
			}
			folded := ModelKey(next)
			if visited[folded] {
				// Cycle detected — break it by removing this edge
				delete(m, cur)
				break
			}
			visited[folded] = true
			cur = folded
		}
	}
}
