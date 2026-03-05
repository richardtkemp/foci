// Package display provides a shared table formatter with proper Unicode
// display-width handling, plus common formatting utilities.
package display

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Column alignment constants.
const (
	AlignLeft  = 0
	AlignRight = 1
)

// tabWidth is the display width of a tab character (standard terminal width).
const tabWidth = 4

// Column describes a table column.
type Column struct {
	Header string
	Align  int // AlignLeft (default) or AlignRight
}

// Format renders rows as a markdown pipe table. AlignRight columns get
// a right-aligned separator (---:). No padding — destinations handle that.
func Format(cols []Column, rows [][]string) string {
	if len(cols) == 0 {
		return ""
	}

	var b strings.Builder

	// Header line.
	b.WriteByte('|')
	for _, c := range cols {
		b.WriteByte(' ')
		b.WriteString(c.Header)
		b.WriteString(" |")
	}
	b.WriteByte('\n')

	// Separator line.
	b.WriteByte('|')
	for _, c := range cols {
		b.WriteByte(' ')
		if c.Align == AlignRight {
			b.WriteString("---:")
		} else {
			b.WriteString("---")
		}
		b.WriteString(" |")
	}
	b.WriteByte('\n')

	// Data rows.
	for _, row := range rows {
		b.WriteByte('|')
		for i := range cols {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			b.WriteByte(' ')
			b.WriteString(cell)
			b.WriteString(" |")
		}
		b.WriteByte('\n')
	}

	return strings.TrimRight(b.String(), "\n")
}

// DisplayWidth calculates the terminal display width of a string.
// ASCII chars are width 1, East Asian Wide chars are width 2,
// combining marks and zero-width chars are width 0.
func DisplayWidth(s string) int {
	width := 0
	for _, r := range s {
		switch {
		case r == '\t':
			width += tabWidth - (width % tabWidth)
		case unicode.IsControl(r):
		case isWide(r):
			width += 2
		case isZeroWidth(r):
		default:
			width += 1
		}
	}
	return width
}

// PadRight pads s with spaces on the right to reach targetWidth
// (measured in display width). If s is already wider, it is returned as-is.
func PadRight(s string, targetWidth int) string {
	currentWidth := DisplayWidth(s)
	if currentWidth >= targetWidth {
		return s
	}
	return s + strings.Repeat(" ", targetWidth-currentWidth)
}

// Truncate truncates s to fit within maxWidth display columns.
// If s is wider than maxWidth, it is cut and "…" is appended (the
// ellipsis replaces the last character to stay within maxWidth).
// Returns s unchanged if it already fits.
func Truncate(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if DisplayWidth(s) <= maxWidth {
		return s
	}
	// Walk runes, accumulating display width.
	w := 0
	var result []rune
	for _, r := range s {
		rw := 1
		switch {
		case r == '\t':
			rw = tabWidth - (w % tabWidth)
		case isZeroWidth(r):
			rw = 0
		case isWide(r):
			rw = 2
		}
		if w+rw > maxWidth-1 { // leave room for "…"
			break
		}
		result = append(result, r)
		w += rw
	}
	return string(result) + "…"
}

// WrapText word-wraps s to fit within maxWidth display columns.
// Splits at word boundaries (spaces); hard-breaks words wider than maxWidth.
// If result exceeds maxLines, truncates to maxLines with "…" on the last line.
// maxLines <= 0 means unlimited lines.
func WrapText(s string, maxWidth, maxLines int) []string {
	if s == "" {
		return []string{""}
	}
	if maxWidth <= 0 {
		return []string{s}
	}

	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{""}
	}

	var lines []string
	var curLine strings.Builder
	curWidth := 0

	flush := func() {
		lines = append(lines, curLine.String())
		curLine.Reset()
		curWidth = 0
	}

	for _, word := range words {
		ww := DisplayWidth(word)

		// Hard-break words wider than maxWidth
		if ww > maxWidth {
			// Flush current line if non-empty
			if curWidth > 0 {
				flush()
			}
			// Break the word into maxWidth-sized chunks
			var chunk strings.Builder
			cw := 0
			for _, r := range word {
				rw := 1
				switch {
				case r == '\t':
					rw = tabWidth - (cw % tabWidth)
				case isZeroWidth(r):
					rw = 0
				case isWide(r):
					rw = 2
				}
				if cw+rw > maxWidth && cw > 0 {
					lines = append(lines, chunk.String())
					chunk.Reset()
					cw = 0
				}
				chunk.WriteRune(r)
				cw += rw
			}
			if chunk.Len() > 0 {
				curLine.WriteString(chunk.String())
				curWidth = cw
			}
			continue
		}

		// Does the word fit on the current line?
		needed := ww
		if curWidth > 0 {
			needed += 1 // space separator
		}
		if curWidth+needed > maxWidth {
			// Word doesn't fit — start a new line
			if curWidth > 0 {
				flush()
			}
		}
		if curWidth > 0 {
			curLine.WriteByte(' ')
			curWidth++
		}
		curLine.WriteString(word)
		curWidth += ww
	}
	if curWidth > 0 {
		flush()
	}

	// Apply maxLines cap
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[:maxLines]
		// Indicate truncation with "…" on the last line
		last := lines[maxLines-1]
		if DisplayWidth(last)+1 <= maxWidth {
			// Room to append "…"
			lines[maxLines-1] = last + "…"
		} else {
			// Need to truncate to fit "…"
			lines[maxLines-1] = Truncate(last, maxWidth)
		}
	}

	return lines
}

// FormatDuration formats a duration as compact human-readable text.
// Examples: "38s", "3m12s", "3h12m", "2d4h".
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	return fmt.Sprintf("%dd%dh", days, hours)
}

// FormatCommas formats an integer with comma separators (e.g. 32793 → "32,793").
func FormatCommas(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + FormatCommas(-n)
	}
	if len(s) <= 3 {
		return s
	}
	var result strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result.WriteByte(',')
		}
		result.WriteRune(c)
	}
	return result.String()
}

// FormatBytes formats a byte count as human-readable (e.g. "1.5 KB").
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// RelativeTime formats a timestamp as a relative time string (e.g. "3h ago").
func RelativeTime(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		m := int(d.Minutes())
		if m == 1 {
			return "1m ago"
		}
		return fmt.Sprintf("%dm ago", m)
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		if h == 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", h)
	}
	days := int(d.Hours() / 24)
	if days == 1 {
		return "1d ago"
	}
	return fmt.Sprintf("%dd ago", days)
}

// wideRanges defines Unicode ranges for characters that take two display columns.
var wideRanges = &unicode.RangeTable{
	R16: []unicode.Range16{
		{0x1100, 0x115F, 1},  // Hangul Jamo
		{0x2329, 0x232A, 1},  // Angle brackets
		{0x2600, 0x26FF, 1},  // Miscellaneous Symbols
		{0x2700, 0x27BF, 1},  // Dingbats
		{0x2E80, 0x303E, 1},  // CJK Radicals, Kangxi, Ideographic
		{0x3040, 0xA4CF, 1},  // CJK Unified + Hiragana/Katakana/Bopomofo/Hangul/Yi
		{0xAC00, 0xD7A3, 1},  // Hangul Syllables
		{0xF900, 0xFAFF, 1},  // CJK Compatibility Ideographs
		{0xFE10, 0xFE1F, 1},  // Vertical forms
		{0xFE30, 0xFE6F, 1},  // CJK Compatibility Forms
		{0xFF00, 0xFF60, 1},  // Fullwidth Forms
		{0xFFE0, 0xFFE6, 1},  // Fullwidth Signs
	},
	R32: []unicode.Range32{
		{0x1F000, 0x1F02F, 1},  // Mahjong/Domino Tiles
		{0x1F100, 0x1F1FF, 1},  // Enclosed Alphanumeric Supplement
		{0x1F300, 0x1FAD6, 1},  // Misc Symbols & Pictographs, Emoticons, etc.
		{0x1F600, 0x1F64F, 1},  // Emoticons
		{0x1F680, 0x1F6FF, 1},  // Transport & Map Symbols
		{0x1F900, 0x1F9FF, 1},  // Supplemental Symbols
		{0x1FA00, 0x1FA6F, 1},  // Chess Symbols
		{0x1FA70, 0x1FAFF, 1},  // Symbols Extended-A
		{0x20000, 0x2FFFD, 1},  // CJK Unified Ideographs Extension B+
		{0x30000, 0x3FFFD, 1},  // CJK Unified Ideographs Extension G+
	},
}

func isWide(r rune) bool { return unicode.Is(wideRanges, r) }

// zeroWidthRanges defines Unicode ranges for characters that take zero display columns.
var zeroWidthRanges = &unicode.RangeTable{
	R16: []unicode.Range16{
		{0x0300, 0x036F, 1},  // Combining Diacritical Marks
		{0x1AB0, 0x1AFF, 1},  // Combining Diacritical Marks Extended
		{0x1DC0, 0x1DFF, 1},  // Combining Diacritical Marks Supplement
		{0x200B, 0x200D, 1},  // Zero Width Space/Joiner/Non-Joiner
		{0x202A, 0x202E, 1},  // Bidi controls
		{0x2060, 0x2063, 1},  // Invisible operators
		{0x2066, 0x2069, 1},  // Bidi isolates
		{0x20D0, 0x20FF, 1},  // Combining Marks for Symbols
		{0xFE00, 0xFE0F, 1},  // Variation Selectors
		{0xFE20, 0xFE2F, 1},  // Combining Half Marks
		{0xFEFF, 0xFEFF, 1},  // BOM / Zero Width No-Break Space
	},
	R32: []unicode.Range32{
		{0xE0100, 0xE01EF, 1}, // Variation Selectors Supplement
	},
}

func isZeroWidth(r rune) bool { return unicode.Is(zeroWidthRanges, r) }
