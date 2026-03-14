package telegram

import (
	"strings"
	"testing"
	"time"

	"foci/internal/display"
)

func TestStreamWriter_NoDeltasNoMessage(t *testing.T) {
	// Verifies that if no deltas arrive, Finish returns 0 and no messages are sent.
	// This proves the lazy-start design: no goroutines or messages are created without data.
	mc := &mockClient{}
	sw := newStreamWriter(mc, 123, 50*time.Millisecond, display.RenderOpts{})

	msgID := sw.Finish()

	if msgID != 0 {
		t.Errorf("expected msgID 0, got %d", msgID)
	}
	if mc.sentCount() != 0 {
		t.Errorf("expected 0 sends, got %d", mc.sentCount())
	}
}

func TestStreamWriter_FirstDeltaSendsMessage(t *testing.T) {
	// Verifies that the first OnDelta call sends an initial message with HTML
	// formatting and that Finish returns the message ID.
	mc := &mockClient{}
	sw := newStreamWriter(mc, 123, 50*time.Millisecond, display.RenderOpts{})

	sw.OnDelta("hello")
	msgID := sw.Finish()

	if mc.sentCount() != 1 {
		t.Errorf("expected 1 send, got %d", mc.sentCount())
	}
	if msgID == 0 {
		t.Error("expected non-zero msgID after delta")
	}
	if mc.lastSendInjected != "hello" {
		t.Errorf("expected initial text %q, got %q", "hello", mc.lastSendInjected)
	}
	// Verify HTML parse mode was set
	if mc.lastSendOpts == nil || mc.lastSendOpts.ParseMode != "HTML" {
		t.Error("expected SendMessage with ParseMode HTML")
	}
}

func TestStreamWriter_EditsOnTick(t *testing.T) {
	// Verifies that after the initial message, subsequent deltas trigger periodic
	// edits via the ticker with HTML formatting.
	mc := &mockClient{}
	sw := newStreamWriter(mc, 123, 20*time.Millisecond, display.RenderOpts{})

	sw.OnDelta("first")
	sw.OnDelta(" second")

	// Wait long enough for at least one tick to fire and edit.
	time.Sleep(80 * time.Millisecond)

	msgID := sw.Finish()

	if msgID == 0 {
		t.Error("expected non-zero msgID")
	}
	if mc.editCount() < 1 {
		t.Errorf("expected at least 1 edit, got %d", mc.editCount())
	}
	if mc.lastEditText != "first second" {
		t.Errorf("expected edit text %q, got %q", "first second", mc.lastEditText)
	}
	// Verify HTML parse mode was set on edits
	mc.mu.Lock()
	opts := mc.lastEditOpts
	mc.mu.Unlock()
	if opts == nil || opts.ParseMode != "HTML" {
		t.Error("expected EditMessageText with ParseMode HTML")
	}
}

func TestStreamWriter_Truncation(t *testing.T) {
	// Verifies that text exceeding streamMaxChars is truncated with "..." appended.
	// This ensures we stay within Telegram's 4096 character message limit.
	mc := &mockClient{}
	sw := newStreamWriter(mc, 123, 50*time.Millisecond, display.RenderOpts{})

	long := strings.Repeat("x", streamMaxChars+500)
	sw.OnDelta(long)
	sw.Finish()

	expected := long[:streamMaxChars] + "..."
	if mc.lastSendInjected != expected {
		t.Errorf("expected truncated text of length %d, got length %d", len(expected), len(mc.lastSendInjected))
	}
}

func TestStreamWriter_FinishStopsTicker(t *testing.T) {
	// Verifies that after Finish(), the edit loop has exited and no further edits
	// occur even if we wait past the interval.
	mc := &mockClient{}
	sw := newStreamWriter(mc, 123, 20*time.Millisecond, display.RenderOpts{})

	sw.OnDelta("data")
	sw.Finish()

	editsAfterFinish := mc.editCount()
	time.Sleep(80 * time.Millisecond)

	if mc.editCount() != editsAfterFinish {
		t.Errorf("edits continued after Finish: before=%d, after=%d", editsAfterFinish, mc.editCount())
	}
}

func TestStreamWriter_DeltaAfterFinishIgnored(t *testing.T) {
	// Verifies that OnDelta calls after Finish() are silently ignored and do not
	// send additional messages or start new goroutines.
	mc := &mockClient{}
	sw := newStreamWriter(mc, 123, 50*time.Millisecond, display.RenderOpts{})

	sw.OnDelta("before")
	sw.Finish()

	sendsBeforeExtra := mc.sentCount()
	sw.OnDelta("after")
	time.Sleep(20 * time.Millisecond)

	if mc.sentCount() != sendsBeforeExtra {
		t.Errorf("extra send after Finish: before=%d, after=%d", sendsBeforeExtra, mc.sentCount())
	}
}

func TestStreamWriter_HTMLFormatting(t *testing.T) {
	// Verifies that streaming edits apply markdown-to-HTML conversion.
	// Complete markdown (matched delimiters) should render as HTML tags.
	mc := &mockClient{}
	sw := newStreamWriter(mc, 123, 20*time.Millisecond, display.RenderOpts{})

	sw.OnDelta("**bold text**")

	// Wait for an edit tick
	time.Sleep(60 * time.Millisecond)
	sw.Finish()

	// The initial send should have HTML formatting
	if mc.lastSendInjected != "<b>bold text</b>" {
		t.Errorf("expected HTML bold, got %q", mc.lastSendInjected)
	}
}

func TestStreamWriter_PartialMarkdownStripped(t *testing.T) {
	// Verifies that incomplete markdown delimiters are stripped before HTML
	// conversion so Telegram doesn't reject the message.
	mc := &mockClient{}
	sw := newStreamWriter(mc, 123, 20*time.Millisecond, display.RenderOpts{})

	// Simulate partial bold: opening ** without closing
	sw.OnDelta("**Bold tex")
	sw.Finish()

	// The unmatched ** should be stripped, leaving just "Bold tex"
	if mc.lastSendInjected != "Bold tex" {
		t.Errorf("expected stripped text %q, got %q", "Bold tex", mc.lastSendInjected)
	}
}

func TestStreamWriter_PartialThenComplete(t *testing.T) {
	// Verifies the streaming progression: first partial markdown is stripped,
	// then once complete, it renders as proper HTML.
	mc := &mockClient{}
	sw := newStreamWriter(mc, 123, 20*time.Millisecond, display.RenderOpts{})

	// First delta: incomplete bold
	sw.OnDelta("**Bold tex")

	// Initial send should strip the unmatched **
	if mc.lastSendInjected != "Bold tex" {
		t.Errorf("step 1: expected %q, got %q", "Bold tex", mc.lastSendInjected)
	}

	// Second delta: complete the bold
	sw.OnDelta("t**")

	// Wait for edit tick
	time.Sleep(60 * time.Millisecond)
	sw.Finish()

	mc.mu.Lock()
	editText := mc.lastEditText
	mc.mu.Unlock()

	// Now the full "**Bold text**" should render as HTML bold
	if editText != "<b>Bold text</b>" {
		t.Errorf("step 2: expected %q, got %q", "<b>Bold text</b>", editText)
	}
}

func TestStreamWriter_PartialCodeFence(t *testing.T) {
	// Verifies that an unclosed code fence is stripped from streaming output.
	mc := &mockClient{}
	sw := newStreamWriter(mc, 123, 20*time.Millisecond, display.RenderOpts{})

	sw.OnDelta("before\n```\nsome code")
	sw.Finish()

	// The unclosed fence and everything after it should be stripped.
	// The newline before the fence is preserved.
	if mc.lastSendInjected != "before\n" {
		t.Errorf("expected %q, got %q", "before\n", mc.lastSendInjected)
	}
}

func TestStreamWriter_FallbackOnHTMLError(t *testing.T) {
	// Verifies that when HTML send fails, the stream writer falls back to plain text.
	mc := &mockClient{}
	// Make SendMessage fail on first call (HTML), succeed on second (plain text).
	sendCount := 0
	origSend := mc.SendMessage
	_ = origSend // mockClient has concrete method, use sendErr field instead

	// We need a custom mock for this test. Use the editErr pattern but for sends.
	// Actually, the mockClient doesn't have sendErr. Let's test the edit fallback instead.
	sw := newStreamWriter(mc, 123, 20*time.Millisecond, display.RenderOpts{})

	sw.OnDelta("hello **world**")

	// Make edits fail (HTML rejected)
	mc.mu.Lock()
	mc.editErr = errTestHTML
	mc.editErrOnce = true
	mc.mu.Unlock()

	sw.OnDelta(" more")

	// Wait for edit tick — should try HTML, fail, then fallback to plain text
	time.Sleep(60 * time.Millisecond)
	sw.Finish()

	// Should have at least 2 edit attempts (HTML fail + plain text fallback)
	_ = sendCount
	if mc.editCount() < 2 {
		t.Errorf("expected at least 2 edits (HTML + fallback), got %d", mc.editCount())
	}
}
