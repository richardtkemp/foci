package telegram

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/command"
)

// TestCancelTurn_NoActiveTurn verifies that cancelTurn does not panic when
// no turn is active.
func TestCancelTurn_NoActiveTurn(t *testing.T) {
	b, _ := testBot([]string{}, command.NewRegistry())
	// Should not panic when no turn is active
	b.cancelTurn()
}

// TestCancelTurn_CancelsContext verifies that cancelTurn properly cancels
// the active turn's context.
func TestCancelTurn_CancelsContext(t *testing.T) {
	b, _ := testBot([]string{}, command.NewRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()

	b.cancelTurn()

	select {
	case <-ctx.Done():
		// expected
	case <-time.After(time.Second):
		t.Error("context should be cancelled")
	}
}

// TestSplitMessage_Short verifies that short messages are not split.
func TestSplitMessage_Short(t *testing.T) {
	chunks := splitMessage("hello", 100)
	if len(chunks) != 1 || chunks[0] != "hello" {
		t.Errorf("expected [hello], got %v", chunks)
	}
}

// TestSplitMessage_ExactLimit verifies that messages exactly at the limit
// are not split.
func TestSplitMessage_ExactLimit(t *testing.T) {
	chunks := splitMessage("hello", 5)
	if len(chunks) != 1 || chunks[0] != "hello" {
		t.Errorf("expected [hello], got %v", chunks)
	}
}

// TestSplitMessage_SplitsAtNewline verifies that message splitting prefers
// newline boundaries.
func TestSplitMessage_SplitsAtNewline(t *testing.T) {
	text := "line1\nline2\nline3"
	chunks := splitMessage(text, 10)

	// Should prefer splitting at newline boundaries
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d: %v", len(chunks), chunks)
	}
	// Reconstruct and verify
	var reconstructed string
	for _, c := range chunks {
		reconstructed += c
	}
	if reconstructed != text {
		t.Errorf("reconstruction mismatch: got %q, want %q", reconstructed, text)
	}
}

// TestSplitMessage_LongNoNewlines verifies that long text without newlines
// is split at the limit.
func TestSplitMessage_LongNoNewlines(t *testing.T) {
	text := "abcdefghijklmnop"
	chunks := splitMessage(text, 5)
	if len(chunks) != 4 {
		t.Fatalf("expected 4 chunks, got %d: %v", len(chunks), chunks)
	}
	var reconstructed string
	for _, c := range chunks {
		reconstructed += c
	}
	if reconstructed != text {
		t.Errorf("reconstruction mismatch: got %q, want %q", reconstructed, text)
	}
}

// TestSplitMessage_Empty verifies that empty messages are handled correctly.
func TestSplitMessage_Empty(t *testing.T) {
	chunks := splitMessage("", 100)
	if len(chunks) != 1 || chunks[0] != "" {
		t.Errorf("expected [\"\"], got %v", chunks)
	}
}

// TestSplitMessage_PreservesCodeBlock verifies that <pre><code> blocks are
// properly closed and reopened when split.
func TestSplitMessage_PreservesCodeBlock(t *testing.T) {
	// A <pre><code> block that exceeds maxLen — tags must be closed/reopened.
	inner := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n"
	text := "<pre><code>" + inner + "</code></pre>"
	chunks := splitMessage(text, 40)

	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		if !strings.HasPrefix(chunk, "<pre><code>") {
			t.Errorf("chunk %d missing opening tags: %q", i, chunk)
		}
		if !strings.HasSuffix(chunk, "</code></pre>") {
			t.Errorf("chunk %d missing closing tags: %q", i, chunk)
		}
		if len(chunk) > 40 {
			t.Errorf("chunk %d exceeds maxLen: len=%d", i, len(chunk))
		}
	}
}

// TestSplitMessage_PreservesPreBlock verifies that <pre> blocks are properly
// closed and reopened when split.
func TestSplitMessage_PreservesPreBlock(t *testing.T) {
	// A <pre> block (table) that exceeds maxLen.
	inner := "row1\nrow2\nrow3\nrow4\nrow5\nrow6\n"
	text := "<pre>" + inner + "</pre>"
	chunks := splitMessage(text, 25)

	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		if !strings.HasPrefix(chunk, "<pre>") {
			t.Errorf("chunk %d missing <pre>: %q", i, chunk)
		}
		if !strings.HasSuffix(chunk, "</pre>") {
			t.Errorf("chunk %d missing </pre>: %q", i, chunk)
		}
	}
}

// TestSplitMessage_NoTagsUnchanged verifies that plain text without HTML
// tags splits correctly.
func TestSplitMessage_NoTagsUnchanged(t *testing.T) {
	// Plain text without HTML tags — same behavior as before.
	text := "line1\nline2\nline3"
	chunks := splitMessage(text, 10)
	var reconstructed string
	for _, c := range chunks {
		reconstructed += c
	}
	if reconstructed != text {
		t.Errorf("reconstruction mismatch: got %q, want %q", reconstructed, text)
	}
}

// TestSplitMessage_ClosedTagsBeforeSplit verifies that fully closed tags
// before split point are not reopened.
func TestSplitMessage_ClosedTagsBeforeSplit(t *testing.T) {
	// Tags are fully closed before the split point — no reopening needed.
	text := "<b>bold</b>\nplain text that is long enough to need splitting"
	chunks := splitMessage(text, 30)

	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	// First chunk has balanced tags; second chunk should be plain.
	if strings.Contains(chunks[1], "<b>") {
		t.Errorf("second chunk should not reopen <b>: %q", chunks[1])
	}
}

// TestSplitMessage_NestedTags verifies that nested tags are properly closed
// and reopened in the correct order.
func TestSplitMessage_NestedTags(t *testing.T) {
	// Nested <b> inside <pre> — both should be closed/reopened.
	text := "<pre><b>" + strings.Repeat("x\n", 20) + "</b></pre>"
	chunks := splitMessage(text, 30)

	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	// First chunk should close in reverse order: </b></pre>
	if !strings.HasSuffix(chunks[0], "</b></pre>") {
		t.Errorf("first chunk should close nested tags: %q", chunks[0])
	}
	// Second chunk should reopen in original order: <pre><b>
	if !strings.HasPrefix(chunks[1], "<pre><b>") {
		t.Errorf("second chunk should reopen nested tags: %q", chunks[1])
	}
}

// TestOpenHTMLTags verifies that openHTMLTags correctly identifies open
// HTML tags in a string.
func TestOpenHTMLTags(t *testing.T) {
	cases := []struct {
		html string
		want []string
	}{
		{"hello", nil},
		{"<pre>text", []string{"<pre>"}},
		{"<pre><code>text", []string{"<pre>", "<code>"}},
		{"<pre><code>text</code></pre>", nil},
		{"<b>bold</b> <i>open", []string{"<i>"}},
		{`<a href="url">link`, []string{`<a href="url">`}},
		{"<pre><code>line1\nline2\n", []string{"<pre>", "<code>"}},
	}
	for _, tc := range cases {
		got := openHTMLTags(tc.html)
		if len(got) != len(tc.want) {
			t.Errorf("openHTMLTags(%q) = %v, want %v", tc.html, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("openHTMLTags(%q)[%d] = %q, want %q", tc.html, i, got[i], tc.want[i])
			}
		}
	}
}

// TestClosingHTMLTag verifies that closingHTMLTag returns the correct closing
// tag for a given opening tag.
func TestClosingHTMLTag(t *testing.T) {
	cases := []struct {
		open, want string
	}{
		{"<pre>", "</pre>"},
		{"<code>", "</code>"},
		{"<b>", "</b>"},
		{`<a href="url">`, "</a>"},
	}
	for _, tc := range cases {
		if got := closingHTMLTag(tc.open); got != tc.want {
			t.Errorf("closingHTMLTag(%q) = %q, want %q", tc.open, got, tc.want)
		}
	}
}
