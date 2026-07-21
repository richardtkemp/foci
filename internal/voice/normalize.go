package voice

import (
	"regexp"
	"strings"
)

// Precompiled regexes for NormalizeForSpeech. Package-level so the cost of
// compiling them is paid once, not per synthesis call.
var (
	// mdLinkRE matches a markdown link "[label](url)" and keeps only the
	// label — a TTS voice reading the raw URL out loud is worse than useless.
	mdLinkRE = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	// mdSymbolRE strips the markdown syntax characters that either mean
	// nothing spoken aloud (heading '#', emphasis '*'/'_', code backticks) or
	// actively mangle a TTS model's tail when glued to a token, e.g. a ticket
	// reference like "#1443" (#1444: this is what garbled Orpheus's output
	// into "laish"). Removing the bare character is enough here — headings,
	// emphasis markers, and ticket-ref hashes all read fine as plain text
	// once the symbol itself is gone ("#1443" -> "1443", "**bold**" -> "bold").
	mdSymbolRE = regexp.MustCompile("[#*_`]")
	// dashRE matches an em or en dash, optionally surrounded by spaces, and
	// normalizes it to a spoken pause (a comma) rather than leaving a glyph
	// most TTS models either skip oddly or mangle.
	dashRE = regexp.MustCompile(`\s*[—–]\s*`)
	// slashRE turns a path/alternative-style slash ("foci-client/voice-mode")
	// into a plain word break — reading "slash" aloud for every occurrence is
	// more distracting than just treating it as a separator.
	slashRE = regexp.MustCompile(`\s*/\s*`)
	// digitRunRE matches a run of three or more consecutive digits. A cardinal
	// reading ("one thousand five") is ambiguous/awkward for anything that
	// isn't a genuine quantity — ticket refs, codes, years-as-IDs, etc. — so
	// runs at or above this length are voiced digit-by-digit instead (#1447:
	// 1005 -> "1 0 0 5"). Each run in the text (e.g. within a decimal or
	// version-like token such as "3.141" or "1.2.3") is split independently;
	// this doesn't parse the surrounding token's semantics, it just applies
	// the length rule to every maximal digit run. Two-digit-or-shorter runs
	// (e.g. "42") are left as ordinary cardinals.
	digitRunRE = regexp.MustCompile(`\d{3,}`)
	// multiSpaceRE collapses whitespace left behind by the substitutions above.
	multiSpaceRE = regexp.MustCompile(`[ \t]{2,}`)
)

// splitDigits spaces out each digit in a matched run so a TTS model voices
// them individually rather than as one large cardinal.
func splitDigits(digits string) string {
	return strings.Join(strings.Split(digits, ""), " ")
}

// NormalizeForSpeech converts markdown/symbol-laden assistant text into
// something a TTS model speaks cleanly (#1444). It targets the specific
// classes of symbol observed to mangle synthesis — not a full markdown
// parser: markdown links collapse to their label, heading/emphasis/code
// markers ('#', '*', '_', backtick) are dropped, em/en dashes become a
// spoken comma pause, slashes become a plain word break, and any run of
// three-or-more digits (a ticket ref, code, or other non-quantity number) is
// split into space-separated digits so a TTS model voices them individually
// instead of as one long cardinal. Call this AFTER any sentinel stripping
// (platform.StripSilencingSuffix / StripSpuriousPrefix) and right before
// TTS.Synthesize — it is a speech rendering transform, not a delivery/silence
// gate.
func NormalizeForSpeech(text string) string {
	if text == "" {
		return text
	}
	out := mdLinkRE.ReplaceAllString(text, "$1")
	out = dashRE.ReplaceAllString(out, ", ")
	out = slashRE.ReplaceAllString(out, " ")
	out = mdSymbolRE.ReplaceAllString(out, "")
	// Run after mdSymbolRE so a ticket ref's leading '#' is already gone
	// (e.g. "#1443" -> "1443" -> "1 4 4 3"), and before multiSpaceRE so the
	// single spaces this inserts aren't disturbed by later cleanup.
	out = digitRunRE.ReplaceAllStringFunc(out, splitDigits)
	out = multiSpaceRE.ReplaceAllString(out, " ")
	return strings.TrimSpace(out)
}
