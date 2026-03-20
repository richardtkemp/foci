package config

// MaxFallbackDepth is the maximum number of fallback hops allowed per request.
const MaxFallbackDepth = 3

// FallbackResolver resolves model fallbacks for automatic failover.
// Keys and values are canonical "developer/model_id" format.
// Returns nil from constructor when both maps are empty (no-op fast path).
type FallbackResolver struct {
	fallbacks map[string]string // canonical model → canonical fallback
}

// NewFallbackResolver creates a FallbackResolver by merging global and per-agent
// fallback maps (per-agent wins). All keys and values are normalized to
// canonical "developer/model_id" format. Cycles are detected and broken.
// Returns nil if both maps are empty.
func NewFallbackResolver(global, perAgent map[string]string) *FallbackResolver {
	if len(global) == 0 && len(perAgent) == 0 {
		return nil
	}

	merged := make(map[string]string, len(global)+len(perAgent))

	// Start with global entries
	for k, v := range global {
		ck := canonicalize(k)
		cv := canonicalize(v)
		if ck != "" && cv != "" {
			merged[ck] = cv
		}
	}

	// Per-agent overrides
	for k, v := range perAgent {
		ck := canonicalize(k)
		cv := canonicalize(v)
		if ck != "" && cv != "" {
			merged[ck] = cv
		}
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

// Resolve returns the fallback model for the given model, or nil if no
// fallback is configured. The input is normalized through the same
// canonicalization used at construction time.
func (fr *FallbackResolver) Resolve(model string) *ResolvedModel {
	if fr == nil {
		return nil
	}
	// Canonicalize using a nil alias map since keys are already canonical.
	// We need to handle bare model IDs though — try direct lookup first,
	// then try with just splitting.
	key := normalizeModelKey(model)
	fb, ok := fr.fallbacks[key]
	if !ok {
		return nil
	}

	// Parse the canonical fallback into a ResolvedModel
	resolved, err := ResolveModel(fb, "")
	if err != nil {
		return nil
	}
	return resolved
}

// canonicalize resolves a model string to canonical "developer/model_id" format.
// Returns "" if unresolvable.
func canonicalize(model string) string {
	resolved, err := ResolveModel(model, "")
	if err != nil {
		return ""
	}
	return resolved.Developer + "/" + resolved.ModelID
}

// normalizeModelKey normalizes a model string to "developer/model_id" for
// lookup in the fallback map. Handles both "developer/model_id" and bare
// model IDs by attempting ResolveModel without aliases.
func normalizeModelKey(model string) string {
	// If it already has a slash, parse directly
	resolved, err := ResolveModel(model, "")
	if err != nil {
		return model
	}
	return resolved.Developer + "/" + resolved.ModelID
}

// breakCycles detects and removes edges that would create cycles in the
// fallback chain. For each key, walks the chain; if it revisits a node,
// deletes the edge from that node.
func breakCycles(m map[string]string) {
	for start := range m {
		visited := map[string]bool{start: true}
		cur := start
		for {
			next, ok := m[cur]
			if !ok {
				break
			}
			if visited[next] {
				// Cycle detected — break it by removing this edge
				delete(m, cur)
				break
			}
			visited[next] = true
			cur = next
		}
	}
}
