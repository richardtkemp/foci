package telegram

import (
	"strings"
	"testing"
	"time"
)

func TestStreamWriter_NoDeltasNoMessage(t *testing.T) {
	// Verifies that if no deltas arrive, Finish returns 0 and no messages are sent.
	// This proves the lazy-start design: no goroutines or messages are created without data.
	mc := &mockClient{}
	sw := newStreamWriter(mc, 123, 50*time.Millisecond)

	msgID := sw.Finish()

	if msgID != 0 {
		t.Errorf("expected msgID 0, got %d", msgID)
	}
	if mc.sentCount() != 0 {
		t.Errorf("expected 0 sends, got %d", mc.sentCount())
	}
}

func TestStreamWriter_FirstDeltaSendsMessage(t *testing.T) {
	// Verifies that the first OnDelta call sends an initial message and that
	// Finish returns the message ID. This proves the lazy-start mechanism works.
	mc := &mockClient{}
	sw := newStreamWriter(mc, 123, 50*time.Millisecond)

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
}

func TestStreamWriter_EditsOnTick(t *testing.T) {
	// Verifies that after the initial message, subsequent deltas trigger periodic
	// edits via the ticker. We send multiple deltas and wait for at least one edit.
	mc := &mockClient{}
	sw := newStreamWriter(mc, 123, 20*time.Millisecond)

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
}

func TestStreamWriter_Truncation(t *testing.T) {
	// Verifies that text exceeding streamMaxChars is truncated with "..." appended.
	// This ensures we stay within Telegram's 4096 character message limit.
	mc := &mockClient{}
	sw := newStreamWriter(mc, 123, 50*time.Millisecond)

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
	sw := newStreamWriter(mc, 123, 20*time.Millisecond)

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
	sw := newStreamWriter(mc, 123, 50*time.Millisecond)

	sw.OnDelta("before")
	sw.Finish()

	sendsBeforeExtra := mc.sentCount()
	sw.OnDelta("after")
	time.Sleep(20 * time.Millisecond)

	if mc.sentCount() != sendsBeforeExtra {
		t.Errorf("extra send after Finish: before=%d, after=%d", sendsBeforeExtra, mc.sentCount())
	}
}
