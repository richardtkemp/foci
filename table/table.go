// Package table provides a shared column-aligned table formatter with
// proper Unicode display-width handling.
package table

import (
	"strings"
	"unicode"
)

// Column alignment constants.
const (
	AlignLeft  = 0
	AlignRight = 1
)

// Column describes a table column.
type Column struct {
	Header string
	Align  int // AlignLeft (default) or AlignRight
}

// Format renders rows as a column-aligned table with a header line and
// a separator. Columns auto-size to the widest value using display width
// (East Asian Wide characters count as 2, combining marks as 0).
func Format(cols []Column, rows [][]string) string {
	if len(cols) == 0 {
		return ""
	}

	// Measure max display width per column (header vs all row values).
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = DisplayWidth(c.Header)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i >= len(cols) {
				break
			}
			if w := DisplayWidth(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}

	// Total width including 2-space gaps between columns.
	totalWidth := 0
	for i, w := range widths {
		totalWidth += w
		if i < len(widths)-1 {
			totalWidth += 2
		}
	}

	var b strings.Builder

	// Header line.
	for i, c := range cols {
		if i > 0 {
			b.WriteString("  ")
		}
		if c.Align == AlignRight {
			padLeft(&b, c.Header, widths[i])
		} else {
			b.WriteString(PadRight(c.Header, widths[i]))
		}
	}
	b.WriteByte('\n')

	// Separator.
	b.WriteString(strings.Repeat("─", totalWidth))
	b.WriteByte('\n')

	// Data rows.
	for _, row := range rows {
		for i, c := range cols {
			if i > 0 {
				b.WriteString("  ")
			}
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			if c.Align == AlignRight {
				padLeft(&b, cell, widths[i])
			} else {
				b.WriteString(PadRight(cell, widths[i]))
			}
		}
		b.WriteByte('\n')
	}

	return strings.TrimRight(b.String(), "\n")
}

// padLeft writes s right-aligned within targetWidth.
func padLeft(b *strings.Builder, s string, targetWidth int) {
	w := DisplayWidth(s)
	if w < targetWidth {
		b.WriteString(strings.Repeat(" ", targetWidth-w))
	}
	b.WriteString(s)
}

// DisplayWidth calculates the terminal display width of a string.
// ASCII chars are width 1, East Asian Wide chars are width 2,
// combining marks and zero-width chars are width 0.
func DisplayWidth(s string) int {
	width := 0
	for _, r := range s {
		switch {
		case r == '\t':
			width += 4 - (width % 4)
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

// FormatWidth renders a table like Format but constrains it to maxWidth
// display columns. If maxWidth <= 0, it delegates to Format (no constraint).
// When the natural table is wider than maxWidth, the widest columns are
// iteratively shrunk and their cell values truncated with "…".
func FormatWidth(cols []Column, rows [][]string, maxWidth int) string {
	if maxWidth <= 0 {
		return Format(cols, rows)
	}
	if len(cols) == 0 {
		return ""
	}

	const minCol = 4

	// Measure natural widths.
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = DisplayWidth(c.Header)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i >= len(cols) {
				break
			}
			if w := DisplayWidth(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}

	// Gaps: 2 spaces between columns.
	gaps := 2 * (len(cols) - 1)
	if gaps < 0 {
		gaps = 0
	}

	// Shrink widest columns iteratively until total fits.
	for {
		total := gaps
		for _, w := range widths {
			total += w
		}
		if total <= maxWidth {
			break
		}
		// Find widest column.
		widest := 0
		for i, w := range widths {
			if w > widths[widest] {
				widest = i
			}
			_ = w
		}
		if widths[widest] <= minCol {
			break // can't shrink further
		}
		widths[widest]--
	}

	// Render with truncation.
	totalWidth := 0
	for i, w := range widths {
		totalWidth += w
		if i < len(widths)-1 {
			totalWidth += 2
		}
	}
	if totalWidth > maxWidth {
		totalWidth = maxWidth
	}

	var b strings.Builder
	writeCell := func(s string, colWidth int, align int) {
		truncated := Truncate(s, colWidth)
		if align == AlignRight {
			padLeft(&b, truncated, colWidth)
		} else {
			b.WriteString(PadRight(truncated, colWidth))
		}
	}

	// Header.
	for i, c := range cols {
		if i > 0 {
			b.WriteString("  ")
		}
		writeCell(c.Header, widths[i], c.Align)
	}
	b.WriteByte('\n')

	// Separator.
	b.WriteString(strings.Repeat("─", totalWidth))
	b.WriteByte('\n')

	// Data rows.
	for _, row := range rows {
		for i, c := range cols {
			if i > 0 {
				b.WriteString("  ")
			}
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			writeCell(cell, widths[i], c.Align)
		}
		b.WriteByte('\n')
	}

	return strings.TrimRight(b.String(), "\n")
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
			rw = 4 - (w % 4)
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
		{0xFE20, 0xFE2F, 1},  // Combining Half Marks
		{0xFEFF, 0xFEFF, 1},  // BOM / Zero Width No-Break Space
	},
	R32: []unicode.Range32{
		{0xE0100, 0xE01EF, 1}, // Variation Selectors Supplement
	},
}

func isZeroWidth(r rune) bool { return unicode.Is(zeroWidthRanges, r) }
