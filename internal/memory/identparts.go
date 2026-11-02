package memory

import (
	"strings"
	"unicode"
)

// identifierParts returns the parts of every code identifier in text, so a
// search for one part of an identifier finds the todo that names the whole
// identifier (#2815).
//
// The index's "en" analyzer uses the Unicode word-break rules, which keep
// "FociRepository.seedReliabilityFromDb", "seed_reliability" and "pkg:Name"
// together as ONE token: a search for "seedReliabilityFromDb" or
// "reliability" found nothing. This function splits each such run at '.',
// '_' and ':', then splits every piece at camelCase boundaries
// ("seedReliabilityFromDb" -> seed, Reliability, From, Db; "HTTPServer" ->
// HTTP, Server). It returns the dot/underscore/colon pieces and the camelCase
// parts, deduplicated, in order of first appearance. A plain word ("deploy")
// yields nothing: the analyzer already indexes it whole.
//
// Hyphens and slashes need no work here: the analyzer already splits on them.
// The caller indexes the parts in a separate field; the original token stays
// in the content field, so a search for the whole identifier still matches.
func identifierParts(text string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, run := range strings.FieldsFunc(text, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || isIdentJoiner(r))
	}) {
		pieces := strings.FieldsFunc(run, isIdentJoiner)
		split := len(pieces) > 1
		for _, piece := range pieces {
			camel := camelParts(piece)
			if split || len(camel) > 1 {
				add(piece)
			}
			if len(camel) > 1 {
				for _, c := range camel {
					add(c)
				}
			}
		}
	}
	return out
}

// isIdentJoiner reports the characters that join identifier pieces into one
// analyzer token: '.', '_' and ':' (Unicode word-break MidNumLet, ExtendNumLet
// and MidLetter).
func isIdentJoiner(r rune) bool {
	return r == '.' || r == '_' || r == ':'
}

// camelParts splits s at camelCase boundaries: before an upper-case letter
// that follows a lower-case letter or digit ("fromDb" -> from, Db), and before
// the last upper-case letter of an acronym that a lower-case letter follows
// ("HTTPServer" -> HTTP, Server). Digits stay with the letters before them
// ("sha256Sum" -> sha256, Sum). A word with no boundary returns itself alone.
func camelParts(s string) []string {
	rs := []rune(s)
	var parts []string
	start := 0
	for i := 1; i < len(rs); i++ {
		prev, cur := rs[i-1], rs[i]
		boundary := unicode.IsUpper(cur) && (unicode.IsLower(prev) || unicode.IsDigit(prev))
		if !boundary && unicode.IsUpper(prev) && unicode.IsUpper(cur) &&
			i+1 < len(rs) && unicode.IsLower(rs[i+1]) {
			boundary = true
		}
		if boundary {
			parts = append(parts, string(rs[start:i]))
			start = i
		}
	}
	return append(parts, string(rs[start:]))
}
