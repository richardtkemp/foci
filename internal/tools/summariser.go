package tools

import (
	"context"
	"fmt"
	"unicode/utf8"
)

// Summariser produces a short response to a prompt over an arbitrary content
// blob. Implementations differ only in transport: API mode goes through
// provider.Send (foci's API client); CC mode shells out to `claude --print`,
// reusing the parent CC subprocess's auth so the call charges subscription
// mana rather than separate API spend.
//
// The interface stays narrow so the dispatch decision can be made once at
// agent setup time, where the agent's Backend/Delegator is visible.
//
// content is the bytes to summarise (e.g., a file's contents or piped stdin).
// prompt is the user's question or extraction goal.
// filePath is purely informational — it's embedded in the wrapper sent to the
// model ("<file path=...>") so the model knows what it's looking at. Pass
// "stdin" or "" when the content didn't come from a path.
type Summariser interface {
	Summarise(ctx context.Context, content []byte, prompt, filePath string) (string, error)
}

// CapInputChars truncates content to maxChars rune-counted characters, appending
// an annotation that discloses the original size when truncation happened. This
// mirrors the pattern used by Agent.summariseToolResult so both the tool-result
// summariser and foci_summary apply the same cap with the same disclosure.
//
// maxChars <= 0 disables the cap.
//
// The cap is byte-based on a rune-counted threshold (the slice may split a
// multi-byte rune at the boundary, but the model handles partial UTF-8 fine
// and the annotation makes it explicit). If you need exact rune-boundary
// truncation, post-process before calling.
func CapInputChars(content []byte, maxChars int) []byte {
	if maxChars <= 0 {
		return content
	}
	originalRunes := utf8.RuneCount(content)
	if originalRunes <= maxChars {
		return content
	}
	truncated := content[:maxChars]
	annotation := fmt.Sprintf("\n[... truncated — full content is %d chars, only first %d shown]", originalRunes, maxChars)
	return append(truncated, []byte(annotation)...)
}

// summarySystemPrompt is the shared system prompt used by all summariser
// implementations. Defined here so the wording stays consistent regardless
// of transport.
const summarySystemPrompt = "You are a file summarization assistant. Read the file content and respond to the user's prompt about it. Be concise and precise. Quote key sections word-for-word where accuracy matters (names, values, instructions, error messages) rather than paraphrasing."

// summaryUserMessage formats the content+prompt envelope sent to the model.
// Centralised so api/cli paths stay byte-identical except for transport.
func summaryUserMessage(content []byte, prompt, filePath string) string {
	if filePath == "" {
		filePath = "stdin"
	}
	return fmt.Sprintf("<file path=%q>\n%s\n</file>\n\n%s", filePath, string(content), prompt)
}
