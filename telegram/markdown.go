package telegram

import (
	"regexp"
	"strings"
)

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
		inner := codeBlockRe.FindStringSubmatch(match)[1]
		// HTML-escape the content
		inner = htmlEscape(inner)
		codeBlocks = append(codeBlocks, "<pre><code>"+inner+"</code></pre>")
		return "[CODEBLOCK" + string(rune('0'+idx)) + "]"
	})

	// Tables: detect blocks of lines containing | with a separator row (---).
	// Extract early to protect | chars from other conversions.
	text = convertTables(text, &codeBlocks)

	// Inline code
	var inlineCodes []string
	inlineCodeRe := regexp.MustCompile("`([^`]+)`")
	text = inlineCodeRe.ReplaceAllStringFunc(text, func(match string) string {
		idx := len(inlineCodes)
		code := inlineCodeRe.FindStringSubmatch(match)[1]
		code = htmlEscape(code)
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

	// Italic: _text_
	text = regexp.MustCompile(`_([^_\n]+)_`).ReplaceAllString(text, "<i>$1</i>")

	// Headings: # Text -> <b>Text</b>
	text = regexp.MustCompile(`(?m)^#+\s+(.+)$`).ReplaceAllString(text, "<b>$1</b>")

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

// convertTables finds markdown table blocks and converts them to <pre> blocks.
// A table is identified by consecutive lines containing | characters where at
// least one line matches the separator pattern (e.g. |---|---|).
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
				// Extract table lines and wrap in <pre>
				var tableContent strings.Builder
				for k := tableStart; k < tableEnd; k++ {
					if k > tableStart {
						tableContent.WriteString("\n")
					}
					tableContent.WriteString(htmlEscape(lines[k]))
				}
				idx := len(*codeBlocks)
				*codeBlocks = append(*codeBlocks, "<pre>"+tableContent.String()+"</pre>")
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
