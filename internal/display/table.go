// Package display provides a shared table formatter with proper Unicode
// display-width handling, plus common formatting utilities.
package display

import (
	"regexp"
	"strings"
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

// RenderOpts controls table rendering.
type RenderOpts struct {
	MaxWidth      int                 // max display columns (0 = no constraint)
	WrapLines     int                 // max wrapped lines per cell (0 = truncate)
	Style         string              // "pretty" (default) or "markdown"
	CellTransform func(string) string // optional transform applied to each cell (e.g. DegradeMarkdown)
}

// TableBlock represents a detected table in markdown text.
type TableBlock struct {
	StartLine int      // first line index (inclusive)
	EndLine   int      // last line index (exclusive)
	Lines     []string // the raw table lines
}

// tableRow holds parsed cells and whether it's a separator row.
type tableRow struct {
	cells []string
	isSep bool
}

// wrappedRow holds wrapped cell content for a single table row.
type wrappedRow struct {
	lines [][]string // lines[col] = []wrapped_lines
	max   int        // max wrapped lines across all cols
}

// sepRe matches a markdown table separator line (e.g. |---|---|).
var sepRe = regexp.MustCompile(`^\|?\s*[-:]+[-|\s:]*$`)

// MarkdownTable renders rows as a markdown pipe table. AlignRight columns get
// a right-aligned separator (---:). No padding — destinations handle that.
func MarkdownTable(cols []Column, rows [][]string) string {
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

// DetectTables scans markdown text for pipe-table blocks. A table is
// consecutive lines containing | where at least one matches the separator
// pattern (|---|---|). Returns blocks in order of appearance.
func DetectTables(text string) []TableBlock {
	lines := strings.Split(text, "\n")
	var blocks []TableBlock

	i := 0
	for i < len(lines) {
		if strings.Contains(lines[i], "|") {
			tableStart := i
			tableEnd := i
			hasSep := false

			for j := i; j < len(lines); j++ {
				line := strings.TrimSpace(lines[j])
				if !strings.Contains(line, "|") && line != "" {
					break
				}
				if line == "" {
					break
				}
				if sepRe.MatchString(line) {
					hasSep = true
				}
				tableEnd = j + 1
			}

			if hasSep && tableEnd-tableStart >= 2 {
				blocks = append(blocks, TableBlock{
					StartLine: tableStart,
					EndLine:   tableEnd,
					Lines:     lines[tableStart:tableEnd],
				})
				i = tableEnd
				continue
			}
		}
		i++
	}
	return blocks
}

// ParseCells splits a pipe-table row by | and trims whitespace from each cell.
// Leading/trailing empty cells from outer pipes are removed.
func ParseCells(line string) []string {
	parts := strings.Split(line, "|")
	if len(parts) > 0 && strings.TrimSpace(parts[0]) == "" {
		parts = parts[1:]
	}
	if len(parts) > 0 && strings.TrimSpace(parts[len(parts)-1]) == "" {
		parts = parts[:len(parts)-1]
	}
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

// RenderTable takes raw markdown pipe-table lines and renders a fixed-width
// string. Style "pretty" (default): 2-space column gaps, ─ separator, no |
// borders. Style "markdown": pipe-delimited.
func RenderTable(lines []string, opts RenderOpts) string {
	var rows []tableRow
	maxCols := 0
	for _, line := range lines {
		isSep := sepRe.MatchString(strings.TrimSpace(line))
		cells := ParseCells(line)
		if !isSep && opts.CellTransform != nil {
			for j, cell := range cells {
				cells[j] = opts.CellTransform(cell)
			}
		}
		if len(cells) > maxCols {
			maxCols = len(cells)
		}
		rows = append(rows, tableRow{cells: cells, isSep: isSep})
	}

	// Find max width per column (from non-separator rows)
	colWidths := make([]int, maxCols)
	for _, r := range rows {
		if r.isSep {
			continue
		}
		for j, cell := range r.cells {
			if j < maxCols {
				w := DisplayWidth(cell)
				if w > colWidths[j] {
					colWidths[j] = w
				}
			}
		}
	}
	for j := range colWidths {
		if colWidths[j] < 3 {
			colWidths[j] = 3
		}
	}

	// Shrink columns to fit MaxWidth if set.
	if opts.MaxWidth > 0 && maxCols > 0 {
		const minCol = 3
		// Row overhead differs by style.
		// markdown: "| " + join(" | ") + " |" = 3*ncols + 1
		// pretty:   join("  ") = 2*(ncols-1)
		overhead := 2 * (maxCols - 1)
		if opts.Style == "markdown" {
			overhead = 3*maxCols + 1
		}
		for {
			total := overhead
			for _, w := range colWidths {
				total += w
			}
			if total <= opts.MaxWidth {
				break
			}
			widest := 0
			for i, w := range colWidths {
				if w > colWidths[widest] {
					widest = i
				}
			}
			if colWidths[widest] <= minCol {
				break
			}
			colWidths[widest]--
		}
	}

	// Wrap/truncate cells into per-row blocks
	var wrapped []wrappedRow
	for _, r := range rows {
		if r.isSep {
			wrapped = append(wrapped, wrappedRow{})
			continue
		}
		cl := make([][]string, maxCols)
		maxLines := 1
		for j := 0; j < maxCols; j++ {
			cell := ""
			if j < len(r.cells) {
				cell = r.cells[j]
			}
			w := colWidths[j]
			if DisplayWidth(cell) <= w {
				cl[j] = []string{cell}
			} else if opts.WrapLines > 0 {
				cl[j] = WrapText(cell, w, opts.WrapLines)
			} else {
				cl[j] = []string{Truncate(cell, w)}
			}
			if len(cl[j]) > maxLines {
				maxLines = len(cl[j])
			}
		}
		wrapped = append(wrapped, wrappedRow{lines: cl, max: maxLines})
	}

	if opts.Style == "markdown" {
		return renderMarkdownStyle(rows, wrapped, colWidths, maxCols)
	}
	return renderPrettyStyle(rows, wrapped, colWidths, maxCols)
}

// renderMarkdownStyle outputs pipe-delimited table rows.
func renderMarkdownStyle(rows []tableRow, wrapped []wrappedRow, colWidths []int, maxCols int) string {
	var out []string
	for i, r := range rows {
		if r.isSep {
			var parts []string
			for j := 0; j < maxCols; j++ {
				parts = append(parts, strings.Repeat("-", colWidths[j]))
			}
			out = append(out, "| "+strings.Join(parts, " | ")+" |")
			continue
		}
		cb := wrapped[i]
		for line := 0; line < cb.max; line++ {
			var parts []string
			for j := 0; j < maxCols; j++ {
				cell := ""
				if line < len(cb.lines[j]) {
					cell = cb.lines[j][line]
				}
				parts = append(parts, PadRight(cell, colWidths[j]))
			}
			out = append(out, "| "+strings.Join(parts, " | ")+" |")
		}
	}
	return strings.Join(out, "\n")
}

// renderPrettyStyle outputs tables with 2-space column gaps and ─ separators.
func renderPrettyStyle(rows []tableRow, wrapped []wrappedRow, colWidths []int, maxCols int) string {
	totalWidth := 0
	for _, w := range colWidths {
		totalWidth += w
	}
	totalWidth += 2 * (maxCols - 1) // 2-space gaps

	var out []string
	for i, r := range rows {
		if r.isSep {
			out = append(out, strings.Repeat("─", totalWidth))
			continue
		}
		cb := wrapped[i]
		for line := 0; line < cb.max; line++ {
			var parts []string
			for j := 0; j < maxCols; j++ {
				cell := ""
				if line < len(cb.lines[j]) {
					cell = cb.lines[j][line]
				}
				parts = append(parts, PadRight(cell, colWidths[j]))
			}
			out = append(out, strings.Join(parts, "  "))
		}
	}
	return strings.Join(out, "\n")
}
