package telegram

import (
	"regexp"
	"strings"
	"unicode"
)

// displayWidth calculates the terminal display width of a string.
// ASCII chars are width 1, East Asian Wide chars are width 2,
// combining marks and zero-width chars are width 0.
func displayWidth(s string) int {
	width := 0
	for _, r := range s {
		switch {
		case r == '\t':
			width += 4 - (width % 4)
		case unicode.IsControl(r):
		case isInWideRange(r):
			width += 2
		case isZeroWidth(r):
		default:
			width += 1
		}
	}
	return width
}

func isInWideRange(r rune) bool {
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

func padRight(s string, targetWidth int) string {
	currentWidth := displayWidth(s)
	if currentWidth >= targetWidth {
		return s
	}
	return s + strings.Repeat(" ", targetWidth-currentWidth)
}

// ConvertToTelegramHTML converts standard markdown to Telegram's HTML format.
// HTML is simpler and safer than MarkdownV2 (no escaping hell).
//
// Conversions:
// - **text** -> <b>text</b> (bold)
// - *text* or _text_ -> <i>text</i> (italic)
// - __text__ -> <u>text</u> (underline)
// - ~~text~~ -> <s>text</s> (strikethrough)
// - `code` -> <code>code</code> (inline code)
// - ```lang\ncode\n``` -> <pre><code>code</code></pre> (code block)
// - [text](url) -> <a href="url">text</a> (links)
// - > blockquote -> <blockquote>blockquote</blockquote> (multiline)
// - # Heading -> <b>Heading</b> (bold, Telegram has no headings)
// - ||spoiler|| -> <tg-spoiler>spoiler</tg-spoiler> (Telegram spoiler)
// - | tables | -> <pre> block
// - --- / *** / ___ -> em-dash horizontal rule
// - - item / * item -> bullet lists
// - 1. item -> ordered lists
func ConvertToTelegramHTML(text string) string {
	// Convert markdown formatting in order of precedence
	// Code blocks first (preserve everything inside)
	var codeBlocks []string
	codeBlockRe := regexp.MustCompile("(?m)^```(?:[a-z]+)?\n([\\s\\S]*?)\n```")
	text = codeBlockRe.ReplaceAllStringFunc(text, func(match string) string {
		idx := len(codeBlocks)
		// Extract code (everything between ``` markers)
		parts := codeBlockRe.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		inner := htmlEscape(parts[1])
		codeBlocks = append(codeBlocks, "<pre><code>"+inner+"</code></pre>")
		return "[CODEBLOCK" + string(rune('0'+idx)) + "]"
	})

	// Tables: detect blocks of lines containing | with a separator row (---).
	// Extract early to protect | chars from other conversions.
	text = convertTables(text, &codeBlocks)

	// Inline code (extract early to protect content)
	var inlineCodes []string
	inlineCodeRe := regexp.MustCompile("`([^`]+)`")
	text = inlineCodeRe.ReplaceAllStringFunc(text, func(match string) string {
		idx := len(inlineCodes)
		parts := inlineCodeRe.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		code := htmlEscape(parts[1])
		inlineCodes = append(inlineCodes, "<code>"+code+"</code>")
		return "[INLINECODE" + string(rune('0'+idx)) + "]"
	})

	// Links: [text](url)
	text = regexp.MustCompile(`\[([^\]]+)\]\(([^\)]+)\)`).ReplaceAllString(text, "<a href=\"$2\">$1</a>")

	// Spoilers: ||text||
	text = regexp.MustCompile(`\|\|([^\|]+)\|\|`).ReplaceAllString(text, "<tg-spoiler>$1</tg-spoiler>")

	// Bold: **text**
	text = regexp.MustCompile(`\*\*([^\*]+)\*\*`).ReplaceAllString(text, "<b>$1</b>")

	// Strikethrough: ~~text~~
	text = regexp.MustCompile(`~~([^~]+)~~`).ReplaceAllString(text, "<s>$1</s>")

	// Underline: __text__
	text = regexp.MustCompile(`__([^_]+)__`).ReplaceAllString(text, "<u>$1</u>")

	// Italic: *text* (avoid bold markers which are **)
	text = regexp.MustCompile(`\*([^\*\n]+)\*`).ReplaceAllString(text, "<i>$1</i>")

	// Italic: _text_ (but not when part of snake_case identifiers like word_word_word)
	// Only matches single-word content between underscores (no additional underscores inside)
	// Uses capture groups to preserve surrounding characters
	text = regexp.MustCompile(`(^|[^a-z0-9])_([^_\n]+?)_([^a-z0-9]|$)`).ReplaceAllString(text, "$1<i>$2</i>$3")

	// Headings: relative hierarchy rendering based on levels actually used
	text = convertHeadings(text)

	// Horizontal rules: ---, ***, ___ (3+ chars on a line by themselves)
	text = regexp.MustCompile(`(?m)^[-*_]{3,}\s*$`).ReplaceAllString(text, "————————————————")

	// Bullet lists: - item or * item at start of line
	text = regexp.MustCompile(`(?m)^[-*]\s+(.+)$`).ReplaceAllString(text, "  • $1")

	// Ordered lists: 1. item — indent to align with bullets
	text = regexp.MustCompile(`(?m)^(\d+)\.\s+(.+)$`).ReplaceAllString(text, "  $1. $2")

	// Multiline blockquotes: consecutive > lines merged into single <blockquote>
	text = convertBlockquotes(text)

	// Restore code blocks and inline codes
	for i, code := range codeBlocks {
		text = strings.ReplaceAll(text, "[CODEBLOCK"+string(rune('0'+i))+"]", code)
	}
	for i, code := range inlineCodes {
		text = strings.ReplaceAll(text, "[INLINECODE"+string(rune('0'+i))+"]", code)
	}

	return text
}

// convertTables finds markdown table blocks and converts them to <pre> blocks
// with properly padded columns. A table is identified by consecutive lines
// containing | characters where at least one line matches the separator
// pattern (e.g. |---|---|).
func convertTables(text string, codeBlocks *[]string) string {
	lines := strings.Split(text, "\n")
	sepRe := regexp.MustCompile(`^\|?\s*[-:]+[-|\s:]*$`)

	var result []string
	i := 0
	for i < len(lines) {
		// Check if this could be the start of a table (line with |)
		if strings.Contains(lines[i], "|") {
			// Look ahead to find a table block
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

			// Need at least a header + separator + data row, with a separator
			if hasSep && tableEnd-tableStart >= 2 {
				tableLines := lines[tableStart:tableEnd]
				formatted := formatTable(tableLines, sepRe)
				idx := len(*codeBlocks)
				*codeBlocks = append(*codeBlocks, "<pre>"+htmlEscape(formatted)+"</pre>")
				result = append(result, "[CODEBLOCK"+string(rune('0'+idx))+"]")
				i = tableEnd
				continue
			}
		}
		result = append(result, lines[i])
		i++
	}
	return strings.Join(result, "\n")
}

// parseCells splits a table row by | and trims whitespace from each cell.
// Leading/trailing empty cells from outer pipes are removed.
func parseCells(line string) []string {
	parts := strings.Split(line, "|")
	// Remove leading empty element (from leading |)
	if len(parts) > 0 && strings.TrimSpace(parts[0]) == "" {
		parts = parts[1:]
	}
	// Remove trailing empty element (from trailing |)
	if len(parts) > 0 && strings.TrimSpace(parts[len(parts)-1]) == "" {
		parts = parts[:len(parts)-1]
	}
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

// formatTable normalizes column widths across all rows and rebuilds the table.
func formatTable(lines []string, sepRe *regexp.Regexp) string {
	// Parse all rows into cells
	type row struct {
		cells []string
		isSep bool
	}
	var rows []row
	maxCols := 0
	for _, line := range lines {
		isSep := sepRe.MatchString(strings.TrimSpace(line))
		cells := parseCells(line)
		if len(cells) > maxCols {
			maxCols = len(cells)
		}
		rows = append(rows, row{cells: cells, isSep: isSep})
	}

	// Find max width per column (from non-separator rows)
	// Use display width for proper handling of wide characters
	colWidths := make([]int, maxCols)
	for _, r := range rows {
		if r.isSep {
			continue
		}
		for j, cell := range r.cells {
			if j < maxCols {
				w := displayWidth(cell)
				if w > colWidths[j] {
					colWidths[j] = w
				}
			}
		}
	}
	// Minimum column width of 3 (for separator dashes)
	for j := range colWidths {
		if colWidths[j] < 3 {
			colWidths[j] = 3
		}
	}

	// Rebuild each row with padded cells
	var out []string
	for _, r := range rows {
		var parts []string
		for j := 0; j < maxCols; j++ {
			w := colWidths[j]
			if r.isSep {
				parts = append(parts, strings.Repeat("-", w))
			} else {
				cell := ""
				if j < len(r.cells) {
					cell = r.cells[j]
				}
				// Pad with spaces to display width
				parts = append(parts, padRight(cell, w))
			}
		}
		out = append(out, "| "+strings.Join(parts, " | ")+" |")
	}
	return strings.Join(out, "\n")
}

// headingStyle represents the visual style for a heading level
type headingStyle int

const (
	headingBold headingStyle = iota
	headingDoubleLine
	headingTripleLine
)

// convertHeadings detects heading levels used and maps them to styles.
// Mapping is relative based on distinct levels:
// - 1 level: bold
// - 2 levels: double-line + bold
// - 3 levels: triple-line + double-line + bold
// - 4+ levels: triple-line + double-line + bold, extras all bold
func convertHeadings(text string) string {
	h1Re := regexp.MustCompile(`(?m)^#\s+(.+)$`)
	h2Re := regexp.MustCompile(`(?m)^##\s+(.+)$`)
	h3PlusRe := regexp.MustCompile(`(?m)^###+ (.+)$`)

	hasH1 := h1Re.MatchString(text)
	hasH2 := h2Re.MatchString(text)
	hasH3Plus := h3PlusRe.MatchString(text)

	levels := 0
	if hasH1 {
		levels++
	}
	if hasH2 {
		levels++
	}
	if hasH3Plus {
		levels++
	}

	var h1Style, h2Style, h3Style headingStyle
	switch levels {
	case 1:
		h1Style = headingBold
		h2Style = headingBold
		h3Style = headingBold
	case 2:
		h1Style = headingDoubleLine
		h2Style = headingBold
		h3Style = headingBold
	default:
		h1Style = headingTripleLine
		h2Style = headingDoubleLine
		h3Style = headingBold
	}

	text = h1Re.ReplaceAllStringFunc(text, func(m string) string {
		parts := h1Re.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		return formatHeading(parts[1], h1Style)
	})
	text = h2Re.ReplaceAllStringFunc(text, func(m string) string {
		parts := h2Re.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		return formatHeading(parts[1], h2Style)
	})
	text = h3PlusRe.ReplaceAllStringFunc(text, func(m string) string {
		parts := h3PlusRe.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		return formatHeading(parts[1], h3Style)
	})

	return text
}

func formatHeading(title string, style headingStyle) string {
	switch style {
	case headingTripleLine:
		return "═══ " + title + " ═══"
	case headingDoubleLine:
		return "── " + title + " ──"
	default:
		return "<b>" + title + "</b>"
	}
}

// convertBlockquotes merges consecutive > lines into single <blockquote> tags.
func convertBlockquotes(text string) string {
	lines := strings.Split(text, "\n")
	bqRe := regexp.MustCompile(`^> ?(.*)$`)

	var result []string
	i := 0
	for i < len(lines) {
		if m := bqRe.FindStringSubmatch(lines[i]); m != nil {
			// Start of a blockquote — collect consecutive > lines
			var bqLines []string
			for i < len(lines) {
				if m := bqRe.FindStringSubmatch(lines[i]); m != nil {
					bqLines = append(bqLines, m[1])
					i++
				} else {
					break
				}
			}
			result = append(result, "<blockquote>"+strings.Join(bqLines, "\n")+"</blockquote>")
		} else {
			result = append(result, lines[i])
			i++
		}
	}
	return strings.Join(result, "\n")
}

// htmlEscape escapes HTML special characters
func htmlEscape(text string) string {
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")
	text = strings.ReplaceAll(text, "\"", "&quot;")
	return text
}
