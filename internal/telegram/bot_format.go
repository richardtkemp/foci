package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/toolformat"
)

func isPDFMIME(mime string) bool {
	return platform.NormalizeMIME(mime) == "application/pdf"
}

// isImageMIME returns true if the MIME type is a supported image format.
func isImageMIME(mime string) bool {
	switch platform.NormalizeMIME(mime) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	}
	return false
}

// splitMessage splits text into chunks of at most maxLen bytes.
// It prefers splitting at newline boundaries and preserves HTML formatting
// by closing open tags at split points and reopening them in the next chunk.
//
// vary maxLen to exercise chunk-boundary handling on short inputs.
//
//nolint:unparam // production always passes telegramMaxChars, but the tests
func splitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}
		chunk, rest := splitChunk(text, maxLen)
		chunks = append(chunks, chunk)
		text = rest
	}
	return chunks
}

// splitChunk splits text at a good boundary, returning the chunk and remaining text.
// It closes any open HTML tags at the end of the chunk and reopens them in the rest.
func splitChunk(text string, maxLen int) (chunk, rest string) {
	end := findSplitPoint(text, maxLen)
	open := openHTMLTags(text[:end])

	if len(open) == 0 {
		return text[:end], text[end:]
	}

	var suffix, prefix string
	for i := len(open) - 1; i >= 0; i-- {
		suffix += closingHTMLTag(open[i])
	}
	for _, tag := range open {
		prefix += tag
	}

	// Reduce split point if closing tags would exceed maxLen.
	if end+len(suffix) > maxLen {
		end = findSplitPoint(text, maxLen-len(suffix))
		// Recompute with new split point.
		open = openHTMLTags(text[:end])
		suffix = ""
		prefix = ""
		for i := len(open) - 1; i >= 0; i-- {
			suffix += closingHTMLTag(open[i])
		}
		for _, tag := range open {
			prefix += tag
		}
	}

	return text[:end] + suffix, prefix + text[end:]
}

// findSplitPoint finds the best position to split text, up to maxLen bytes.
// Prefers newline boundaries and avoids splitting inside HTML tags.
func findSplitPoint(text string, maxLen int) int {
	end := maxLen
	if end > len(text) {
		end = len(text)
	}
	if end >= len(text) {
		return end
	}

	// Prefer splitting at a newline.
	if idx := strings.LastIndex(text[:end], "\n"); idx > 0 {
		return idx + 1
	}

	// No newline — avoid splitting inside an HTML tag.
	lastOpen := strings.LastIndexByte(text[:end], '<')
	lastClose := strings.LastIndexByte(text[:end], '>')
	if lastOpen >= 0 && lastOpen > lastClose && lastOpen > 0 {
		return lastOpen
	}

	return end
}

// openHTMLTags scans HTML text and returns the stack of unclosed tags.
// Each entry is the full opening tag (e.g. "<pre>", "<a href=\"url\">").
func openHTMLTags(html string) []string {
	var stack []string
	for i := 0; i < len(html); {
		idx := strings.IndexByte(html[i:], '<')
		if idx < 0 {
			break
		}
		i += idx
		end := strings.IndexByte(html[i:], '>')
		if end < 0 {
			break // incomplete tag at end of string
		}
		tag := html[i : i+end+1]
		i += end + 1

		if strings.HasPrefix(tag, "</") {
			// Closing tag — pop matching from stack.
			name := htmlTagName(tag[2:])
			for j := len(stack) - 1; j >= 0; j-- {
				if htmlTagName(stack[j][1:]) == name {
					stack = append(stack[:j], stack[j+1:]...)
					break
				}
			}
		} else if !strings.HasSuffix(tag, "/>") {
			// Opening tag (skip self-closing).
			stack = append(stack, tag)
		}
	}
	return stack
}

// htmlTagName extracts the tag name from a string starting after '<' or '</'.
// E.g. "pre>", "a href=\"url\">" → "pre", "a".
func htmlTagName(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '>' || s[i] == '/' {
			return s[:i]
		}
	}
	return s
}

// closingHTMLTag returns the closing tag for a full opening tag.
// E.g. "<pre>" → "</pre>", "<a href=\"url\">" → "</a>".
func closingHTMLTag(openTag string) string {
	return "</" + htmlTagName(openTag[1:]) + ">"
}

// toolEmoji maps tool names to per-tool display emoji.
var toolEmoji = map[string]string{
	"shell":           "▶️",
	"web_fetch":       "🔗",
	"web_search":      "🔍",
	"http_request":    "🌍",
	"read":            "📖",
	"write":           "✏️",
	"edit":            "✂️",
	"tmux":            "🪟",
	"todo":            "☑️",
	"send_to_chat":    "📨",
	"memory_search":   "🧠",
	"spawn":           "🐣",
	"scratchpad":      "📋",
	"send_to_session": "💬",
	"remind":          "💭",
}

// emojiForTool returns the per-tool emoji, falling back to 🔧 for unknown tools.
func emojiForTool(name string) string {
	if e, ok := toolEmoji[name]; ok {
		return e
	}
	return "🔧"
}

// formatToolCall formats a tool call for display in Telegram.
// showMode controls truncation: "full" shows everything, other modes truncate.
func (b *Bot) formatToolCall(toolName string, params json.RawMessage, showMode string) string {
	maxChars := b.getDisplay().ToolCallPreviewChars
	if maxChars == 0 {
		maxChars = 450
	}
	// Pretty-print params; truncate only in preview mode
	paramStr := provider.UnescapeUnicodeJSON(string(params))
	var pretty bytes.Buffer
	if json.Indent(&pretty, json.RawMessage(paramStr), "", "  ") == nil {
		paramStr = pretty.String()
	}
	if showMode != "full" && len(paramStr) > maxChars {
		paramStr = paramStr[:maxChars] + "..."
	}
	// Unescape literal \n and \t within JSON string values so they render
	// as actual newlines/tabs in the Telegram <pre> block.
	paramStr = unescapeJSONStringLiterals(paramStr)
	paramStr = htmlEscape(paramStr)
	emoji := emojiForTool(toolName)
	return fmt.Sprintf("%s <b>%s</b>\n<pre>%s</pre>", emoji, htmlEscape(toolName), paramStr)
}

// formatToolCallCompact returns a compact one-line summary for "full" mode.
// e.g. "⚡ exec: ls -la /tmp" or "📡 http_request: GET https://example.com"
func formatToolCallCompact(toolName string, params json.RawMessage) string {
	emoji := emojiForTool(toolName)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(params, &m); err != nil {
		return fmt.Sprintf("%s <b>%s</b>", emoji, htmlEscape(toolName))
	}

	summary := toolformat.CompactSummary(toolName, m)
	if summary == "" {
		return fmt.Sprintf("%s <b>%s</b>", emoji, htmlEscape(toolName))
	}
	return fmt.Sprintf("%s <b>%s</b>: %s", emoji, htmlEscape(toolName), htmlEscape(summary))
}

// unescapeJSONStringLiterals replaces literal \n and \t sequences (as they
// appear in pretty-printed JSON string values) with actual newline and tab
// characters so they render properly inside Telegram <pre> blocks.
func unescapeJSONStringLiterals(s string) string {
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\t`, "\t")
	return s
}

// htmlEscape escapes HTML special characters for Telegram messages.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// truncate is a package-local alias for toolformat.Truncate, used throughout
// the telegram package for log messages, stream previews, etc.
func truncate(s string, max int) string {
	return toolformat.Truncate(s, max)
}

// compactResultHint delegates to toolformat.CompactResultHint.
func compactResultHint(toolName string, params json.RawMessage, result string) string {
	return toolformat.CompactResultHint(toolName, params, result)
}
