package discord

import (
	"strings"

	"foci/internal/platform"
)

// splitMessage splits text into chunks of at most maxLen bytes.
// It prefers splitting at newline boundaries and respects code block boundaries
// by closing and reopening ``` blocks at split points.
//
//nolint:unparam // production always passes discordMaxChars, but the tests vary
// maxLen to exercise chunk-boundary handling on short inputs.
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
// It closes any open code blocks at the end of the chunk and reopens them in the rest.
func splitChunk(text string, maxLen int) (chunk, rest string) {
	end := findSplitPoint(text, maxLen)

	// Check for unclosed code fence
	fenceCount := strings.Count(text[:end], "```")
	if fenceCount%2 != 0 {
		suffix := "\n```"
		prefix := "```\n"

		// Hard-split to avoid infinite loops when newline-preferred splits
		// land on the code fence boundary itself (e.g. "```\n").
		end = maxLen - len(suffix)
		if end > len(text) {
			end = len(text)
		}
		if end < 1 {
			end = 1
		}

		fenceCount = strings.Count(text[:end], "```")
		if fenceCount%2 != 0 {
			return text[:end] + suffix, prefix + text[end:]
		}
		// After hard split, fences are balanced — no suffix/prefix needed.
		return text[:end], text[end:]
	}

	return text[:end], text[end:]
}

// findSplitPoint finds the best position to split text, up to maxLen bytes.
// Prefers newline boundaries, then space boundaries, falling back to hard split.
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

	// No newline -- try space.
	if idx := strings.LastIndex(text[:end], " "); idx > 0 {
		return idx + 1
	}

	// Hard split.
	return end
}

// isPDFMIME returns true if the MIME type is a PDF.
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
