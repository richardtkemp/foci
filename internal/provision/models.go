package provision

import (
	"strings"

	"foci/internal/modelinfo"
)

// familyFallback is the id used when the registry somehow answers nothing for a
// family. It is a floor, not the answer: normal resolution reads the newest
// member out of the model registry, so these literals only ever surface if
// models.jsonl loses a family entirely.
var familyFallback = map[string]string{
	"fable":  "claude-fable-5",
	"opus":   "claude-opus-5",
	"sonnet": "claude-sonnet-5",
	"haiku":  "claude-haiku-4-5",
}

// ResolveModelAlias maps short aliases to full model IDs.
// Accepts full model IDs as pass-through. Empty input defaults to sonnet.
//
// An alias names a FAMILY, not a version — "opus" means "the current opus" —
// so it resolves against the model registry rather than a fixed map. The fixed
// map this replaced had drifted three of its four rows behind reality (opus
// still pointed at 4-6 long after every real turn ran opus-5), which is the
// predictable end state for a hand-maintained literal whose meaning is "newest".
func ResolveModelAlias(input string) string {
	alias := strings.ToLower(strings.TrimSpace(input))
	if alias == "" {
		alias = "sonnet"
	}
	if _, known := familyFallback[alias]; !known {
		return input // a full model id, or something we don't alias — pass through
	}
	id, ok := modelinfo.NewestInFamily("anthropic", alias)
	if !ok {
		id = familyFallback[alias]
	}
	return "anthropic/" + id
}
