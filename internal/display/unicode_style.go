package display

import (
	"regexp"
	"strings"
)

// Unicode Mathematical Sans-Serif character ranges for styled text in
// contexts where rich formatting isn't available (e.g. <pre> blocks).
// Only A-Z, a-z, 0-9 have styled variants; punctuation/symbols stay as-is.

// ApplyBold converts ASCII letters and digits to Mathematical Sans-Serif Bold.
func ApplyBold(s string) string { return mapRunes(s, boldRune) }

// ApplyItalic converts ASCII letters to Mathematical Sans-Serif Italic.
// Digits are unchanged (no italic digits exist in Unicode).
func ApplyItalic(s string) string { return mapRunes(s, italicRune) }

// ApplyBoldItalic converts ASCII letters to Mathematical Sans-Serif Bold Italic.
// Digits use bold (no bold-italic digits exist).
func ApplyBoldItalic(s string) string { return mapRunes(s, boldItalicRune) }

// Mathematical Sans-Serif Bold: U+1D5D4 (A) .. U+1D607 (z), U+1D7EC (0) .. U+1D7F5 (9)
func boldRune(r rune) rune {
	switch {
	case r >= 'A' && r <= 'Z':
		return 0x1D5D4 + (r - 'A')
	case r >= 'a' && r <= 'z':
		return 0x1D5EE + (r - 'a')
	case r >= '0' && r <= '9':
		return 0x1D7EC + (r - '0')
	default:
		return r
	}
}

// Mathematical Sans-Serif Italic: U+1D608 (A) .. U+1D63B (z)
func italicRune(r rune) rune {
	switch {
	case r >= 'A' && r <= 'Z':
		return 0x1D608 + (r - 'A')
	case r >= 'a' && r <= 'z':
		return 0x1D622 + (r - 'a')
	default:
		return r
	}
}

// Mathematical Sans-Serif Bold Italic: U+1D63C (A) .. U+1D66F (z)
// Digits fall back to bold (U+1D7EC).
func boldItalicRune(r rune) rune {
	switch {
	case r >= 'A' && r <= 'Z':
		return 0x1D63C + (r - 'A')
	case r >= 'a' && r <= 'z':
		return 0x1D656 + (r - 'a')
	case r >= '0' && r <= '9':
		return 0x1D7EC + (r - '0')
	default:
		return r
	}
}

func mapRunes(s string, fn func(rune) rune) string {
	var b strings.Builder
	b.Grow(len(s) * 4) // SMP chars are 4 bytes each
	for _, r := range s {
		b.WriteRune(fn(r))
	}
	return b.String()
}

// Precompiled regexes for DegradeMarkdown.
var (
	dgBoldItalicRe = regexp.MustCompile(`\*\*\*([^\*]+)\*\*\*`)
	dgBoldRe       = regexp.MustCompile(`\*\*([^\*]+)\*\*`)
	dgUnderlineRe  = regexp.MustCompile(`__([^_]+)__`)
	dgStrikeRe     = regexp.MustCompile(`~~([^~]+)~~`)
	dgItalicStarRe = regexp.MustCompile(`\*([^\*\n]+)\*`)
	dgCodeRe       = regexp.MustCompile("`([^`]+)`")
	dgLinkRe       = regexp.MustCompile(`\[([^\]]+)\]\([^\)]+\)`)
)

// DegradeMarkdown converts markdown formatting to Unicode visual equivalents
// for pre-formatted text where HTML tags don't render.
//
// Conversions:
//   - ***text*** → Sans-Serif Bold Italic
//   - **text**   → Sans-Serif Bold
//   - __text__   → Sans-Serif Bold (no Unicode underline)
//   - ~~text~~   → ~text~
//   - *text*     → Sans-Serif Italic
//   - `code`     → code (strip backticks, already monospace)
//   - [text](url) → text (links don't work in pre)
func DegradeMarkdown(s string) string {
	// Order matters: multi-char delimiters before single-char.
	s = dgBoldItalicRe.ReplaceAllStringFunc(s, func(m string) string {
		return ApplyBoldItalic(m[3 : len(m)-3])
	})
	s = dgBoldRe.ReplaceAllStringFunc(s, func(m string) string {
		return ApplyBold(m[2 : len(m)-2])
	})
	s = dgUnderlineRe.ReplaceAllStringFunc(s, func(m string) string {
		return ApplyBold(m[2 : len(m)-2])
	})
	s = dgStrikeRe.ReplaceAllString(s, "~$1~")
	s = dgItalicStarRe.ReplaceAllStringFunc(s, func(m string) string {
		return ApplyItalic(m[1 : len(m)-1])
	})
	s = dgCodeRe.ReplaceAllString(s, "$1")
	s = dgLinkRe.ReplaceAllString(s, "$1")
	return s
}
