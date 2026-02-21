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
// - > blockquote -> <blockquote>blockquote</blockquote>
// - # Heading -> <b>Heading</b> (bold, Telegram has no headings)
// - ||spoiler|| -> <tg-spoiler>spoiler</tg-spoiler> (Telegram spoiler)
// - Tables -> <pre> block
func ConvertToTelegramHTML(text string) string {
	// HTML escaping for literal < > & characters (but not in tags we create)
	// For now, assume agent doesn't output raw HTML - just markdown

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

	// Blockquotes: > text (preserve line structure)
	text = regexp.MustCompile(`(?m)^> (.+)$`).ReplaceAllString(text, "<blockquote>$1</blockquote>")

	// Restore code blocks and inline codes
	for i, code := range codeBlocks {
		text = strings.ReplaceAll(text, "[CODEBLOCK"+string(rune('0'+i))+"]", code)
	}
	for i, code := range inlineCodes {
		text = strings.ReplaceAll(text, "[INLINECODE"+string(rune('0'+i))+"]", code)
	}

	return text
}

// htmlEscape escapes HTML special characters
func htmlEscape(text string) string {
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")
	text = strings.ReplaceAll(text, "\"", "&quot;")
	return text
}
