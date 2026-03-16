package discord

import (
	"strings"
	"testing"
)

// TestSplitMessageShortText verifies that messages under the limit are returned as-is.
func TestSplitMessageShortText(t *testing.T) {
	text := "Hello, world!"
	chunks := splitMessage(text, 2000)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != text {
		t.Errorf("expected %q, got %q", text, chunks[0])
	}
}

// TestSplitMessageExactLimit verifies that a message exactly at the limit is a single chunk.
func TestSplitMessageExactLimit(t *testing.T) {
	text := strings.Repeat("a", 2000)
	chunks := splitMessage(text, 2000)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
}

// TestSplitMessageLongText verifies that long text is split into multiple chunks,
// each within the limit.
func TestSplitMessageLongText(t *testing.T) {
	text := strings.Repeat("word ", 500) // 2500 chars
	chunks := splitMessage(text, 2000)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		if len(chunk) > 2000 {
			t.Errorf("chunk %d exceeds limit: %d chars", i, len(chunk))
		}
	}
	// Rejoin should equal original
	joined := strings.Join(chunks, "")
	if joined != text {
		t.Errorf("rejoined text does not match original")
	}
}

// TestSplitMessagePrefersNewline verifies that the splitter prefers newline boundaries
// over mid-word splits.
func TestSplitMessagePrefersNewline(t *testing.T) {
	line1 := strings.Repeat("a", 50) + "\n"
	line2 := strings.Repeat("b", 50) + "\n"
	text := line1 + line2
	chunks := splitMessage(text, 60)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	if chunks[0] != line1 {
		t.Errorf("expected first chunk to be line1, got %q", chunks[0])
	}
}

// TestSplitMessageCodeFenceHandling verifies that code blocks are properly closed
// and reopened at split boundaries.
func TestSplitMessageCodeFenceHandling(t *testing.T) {
	code := "```go\n" + strings.Repeat("x", 2000) + "\n```"
	chunks := splitMessage(code, 100)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}

	// Every chunk with an odd number of ``` fences should have been balanced.
	// The first chunk should end with ``` closure, subsequent chunks should
	// start with ``` reopening.
	for i, chunk := range chunks {
		fences := strings.Count(chunk, "```")
		if fences%2 != 0 {
			t.Errorf("chunk %d has odd fence count (%d): %q...", i, fences, truncate(chunk, 80))
		}
	}
}

// TestSplitMessageEmptyText verifies that empty input returns a single empty chunk.
func TestSplitMessageEmptyText(t *testing.T) {
	chunks := splitMessage("", 2000)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != "" {
		t.Errorf("expected empty chunk, got %q", chunks[0])
	}
}

// TestIsPDFMIME verifies PDF MIME detection.
func TestIsPDFMIME(t *testing.T) {
	if !isPDFMIME("application/pdf") {
		t.Error("expected application/pdf to be PDF")
	}
	if isPDFMIME("image/png") {
		t.Error("expected image/png to not be PDF")
	}
	// Parameterized MIME
	if !isPDFMIME("application/pdf; charset=utf-8") {
		t.Error("expected parameterized PDF MIME to match")
	}
}

// TestIsImageMIME verifies image MIME detection.
func TestIsImageMIME(t *testing.T) {
	for _, mime := range []string{"image/jpeg", "image/png", "image/gif", "image/webp"} {
		if !isImageMIME(mime) {
			t.Errorf("expected %s to be image", mime)
		}
	}
	if isImageMIME("application/pdf") {
		t.Error("expected application/pdf to not be image")
	}
}

