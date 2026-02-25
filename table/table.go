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

func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F:
		return true
	case r >= 0x2329 && r <= 0x232A:
		return true
	case r >= 0x2E80 && r <= 0x303E:
		return true
	case r >= 0x3040 && r <= 0xA4CF:
		return true
	case r >= 0xAC00 && r <= 0xD7A3:
		return true
	case r >= 0xF900 && r <= 0xFAFF:
		return true
	case r >= 0xFE10 && r <= 0xFE1F:
		return true
	case r >= 0xFE30 && r <= 0xFE6F:
		return true
	case r >= 0xFF00 && r <= 0xFF60:
		return true
	case r >= 0xFFE0 && r <= 0xFFE6:
		return true
	case r >= 0x20000 && r <= 0x2FFFD:
		return true
	case r >= 0x30000 && r <= 0x3FFFD:
		return true
	case r >= 0x2600 && r <= 0x26FF:
		return true
	case r >= 0x2700 && r <= 0x27BF:
		return true
	case r >= 0x1F000 && r <= 0x1F02F:
		return true
	case r >= 0x1F100 && r <= 0x1F1FF:
		return true
	case r >= 0x1F300 && r <= 0x1FAD6:
		return true
	case r >= 0x1F600 && r <= 0x1F64F:
		return true
	case r >= 0x1F680 && r <= 0x1F6FF:
		return true
	case r >= 0x1F900 && r <= 0x1F9FF:
		return true
	case r >= 0x1FA00 && r <= 0x1FA6F:
		return true
	case r >= 0x1FA70 && r <= 0x1FAFF:
		return true
	}
	return false
}

func isZeroWidth(r rune) bool {
	switch {
	case r == 0x200B:
		return true
	case r >= 0x200C && r <= 0x200D:
		return true
	case r >= 0x202A && r <= 0x202E:
		return true
	case r >= 0x2060 && r <= 0x2063:
		return true
	case r == 0x2066 || r == 0x2067 || r == 0x2068 || r == 0x2069:
		return true
	case r == 0xFEFF:
		return true
	case r >= 0x0300 && r <= 0x036F:
		return true
	case r >= 0x1AB0 && r <= 0x1AFF:
		return true
	case r >= 0x1DC0 && r <= 0x1DFF:
		return true
	case r >= 0x20D0 && r <= 0x20FF:
		return true
	case r >= 0xFE20 && r <= 0xFE2F:
		return true
	case r >= 0xE0100 && r <= 0xE01EF:
		return true
	}
	return false
}
