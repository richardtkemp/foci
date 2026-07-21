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
	// multiSpaceRE collapses whitespace left behind by the substitutions above.
	multiSpaceRE = regexp.MustCompile(`[ \t]{2,}`)
)

// NormalizeForSpeech converts markdown/symbol-laden assistant text into
// something a TTS model speaks cleanly (#1444). It targets the specific
// classes of symbol observed to mangle synthesis — not a full markdown
// parser: markdown links collapse to their label, heading/emphasis/code
// markers ('#', '*', '_', backtick) are dropped, em/en dashes become a
// spoken comma pause, and slashes become a plain word break. Call this AFTER
// any sentinel stripping (platform.StripSilencingSuffix /
// StripSpuriousPrefix) and right before TTS.Synthesize — it is a speech
// rendering transform, not a delivery/silence gate.
func NormalizeForSpeech(text string) string {
	if text == "" {
		return text
	}
	out := mdLinkRE.ReplaceAllString(text, "$1")
	out = dashRE.ReplaceAllString(out, ", ")
	out = slashRE.ReplaceAllString(out, " ")
	out = mdSymbolRE.ReplaceAllString(out, "")
	out = multiSpaceRE.ReplaceAllString(out, " ")
	return strings.TrimSpace(out)
}
