package discord

import (
	"errors"
	"strings"
	"testing"

	"foci/internal/turn"
)

// rendererBackend builds a discordBackend over a fresh test bot.
func rendererBackend(t *testing.T) (*discordBackend, *fakeSession) {
	t.Helper()
	b, fs, _ := newTestBot(t, "a")
	backend := &discordBackend{
		bot:       b,
		msg:       testDiscordMessage("42", "u1", "hi"),
		channelID: "42",
		width:     20,
	}
	return backend, fs
}

// TestComposeBody verifies body composition for each thinking mode: full
// inlines thinking above a divider, compact requests a button, off is plain.
func TestComposeBody(t *testing.T) {
	backend, _ := rendererBackend(t)

	body, hasButton, thinking := backend.ComposeBody(turn.Payload{Text: "answer", ThinkingText: "thought", ThinkingMode: "full"})
	if !strings.Contains(body, "thought") || !strings.Contains(body, "answer") || hasButton {
		t.Errorf("full: body=%q hasButton=%v", body, hasButton)
	}

	body, hasButton, thinking = backend.ComposeBody(turn.Payload{Text: "answer", ThinkingText: "thought", ThinkingMode: "compact"})
	if body != "answer" || !hasButton || thinking != "thought" {
		t.Errorf("compact: body=%q hasButton=%v thinking=%q", body, hasButton, thinking)
	}

	body, hasButton, _ = backend.ComposeBody(turn.Payload{Text: "answer", ThinkingText: "thought", ThinkingMode: "off"})
	if body != "answer" || hasButton {
		t.Errorf("off: body=%q hasButton=%v", body, hasButton)
	}
}

// TestDeliverFreshSend verifies terminal delivery with no live stream sends the
// chunks as fresh messages and reports their IDs.
func TestDeliverFreshSend(t *testing.T) {
	backend, fs := rendererBackend(t)

	res, err := backend.Deliver(turn.Payload{Text: "short answer"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.MsgIDs) != 1 {
		t.Fatalf("expected 1 message ID, got %v", res.MsgIDs)
	}
	if got := fs.lastSend(t); got.content != "short answer" {
		t.Errorf("got %q", got.content)
	}
}

// TestDeliverFreshSendWithThinkingButton verifies compact-mode delivery puts a
// thinking button on the last chunk and stores the thinking entry.
func TestDeliverFreshSendWithThinkingButton(t *testing.T) {
	backend, fs := rendererBackend(t)

	res, err := backend.Deliver(turn.Payload{Text: "answer", ThinkingText: "hmm", ThinkingMode: "compact"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.MsgIDs) != 1 {
		t.Fatalf("expected 1 message ID, got %v", res.MsgIDs)
	}
	got := fs.lastSend(t)
	if len(got.components) != 1 {
		t.Fatal("expected thinking button")
	}
	val, ok := backend.bot.thinkingStore.Load(int64(1))
	if !ok {
		t.Fatal("expected thinking entry stored")
	}
	if e := val.(thinkingEntry); e.thinkingText != "hmm" || e.responseText != "answer" {
		t.Errorf("unexpected entry %+v", e)
	}
}

// TestDeliverFinalizeInPlace verifies delivery over a live stream edits the
// existing messages and deletes orphans when the final text is shorter.
func TestDeliverFinalizeInPlace(t *testing.T) {
	backend, fs := rendererBackend(t)

	// Build a live stream with three surfaced messages.
	sink := backend.OpenStream().(*discordStreamSink)
	long := strings.Repeat("filler line\n", 400) // ~4800 chars -> 3 messages
	sink.Update(long)
	ids := sink.MsgIDs()
	if len(ids) != 3 {
		t.Fatalf("expected 3 live messages, got %d", len(ids))
	}

	res, err := backend.Deliver(turn.Payload{Text: "final short"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.MsgIDs) != 1 || res.MsgIDs[0] != ids[0] {
		t.Errorf("expected first live ID reused, got %v", res.MsgIDs)
	}
	if got := fs.lastEdit(t); got.msgID != ids[0] || got.content != "final short" {
		t.Errorf("expected in-place edit, got %+v", got)
	}
	if len(fs.deletes) != 2 {
		t.Errorf("expected 2 orphans deleted, got %v", fs.deletes)
	}
}

// TestDeliverAppendsBeyondLiveSequence verifies final text longer than the live
// sequence sends extra messages for the overflow chunks.
func TestDeliverAppendsBeyondLiveSequence(t *testing.T) {
	backend, fs := rendererBackend(t)

	sink := backend.OpenStream().(*discordStreamSink)
	sink.Update("short live")
	if len(sink.MsgIDs()) != 1 {
		t.Fatal("expected 1 live message")
	}
	sendsBefore := fs.sendCount()

	long := strings.Repeat("more text\n", 400) // -> multiple chunks
	res, err := backend.Deliver(turn.Payload{Text: long}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.MsgIDs) < 2 {
		t.Fatalf("expected multiple message IDs, got %v", res.MsgIDs)
	}
	if fs.sendCount() <= sendsBefore {
		t.Error("expected overflow chunks sent as new messages")
	}
}

// TestEditInPlace verifies single-message edits succeed, compact mode attaches
// the thinking button, and over-length bodies return ErrTooLongForEdit.
func TestEditInPlace(t *testing.T) {
	backend, fs := rendererBackend(t)

	if err := backend.EditInPlace("7", turn.Payload{Text: "edited"}); err != nil {
		t.Fatal(err)
	}
	if got := fs.lastEdit(t); got.msgID != "7" || got.content != "edited" {
		t.Errorf("unexpected edit %+v", got)
	}

	if err := backend.EditInPlace("7", turn.Payload{Text: "edited", ThinkingText: "t", ThinkingMode: "compact"}); err != nil {
		t.Fatal(err)
	}
	if got := fs.lastEdit(t); len(got.components) != 1 {
		t.Error("expected thinking button on compact edit")
	}

	long := strings.Repeat("z", discordMaxChars*2)
	if err := backend.EditInPlace("7", turn.Payload{Text: long}); !errors.Is(err, turn.ErrTooLongForEdit) {
		t.Errorf("expected ErrTooLongForEdit, got %v", err)
	}
}

// TestSendMarkdownErrorPath verifies send failures return no ID and Unknown
// Channel errors clear the stale channel state.
func TestSendMarkdownErrorPath(t *testing.T) {
	backend, fs := rendererBackend(t)
	backend.bot.SetChatID(42)
	fs.sendErr = unknownChannelErr()

	if id, ok := backend.SendChunk("text"); ok || id != "" {
		t.Error("expected failure")
	}
	if backend.bot.ChatID() != 0 {
		t.Error("expected stale channel cleared")
	}
}

// TestStreamSinkUpdateAndRollover verifies the stream sink sends one message
// for short text, edits on subsequent updates, skips unchanged chunks, and
// rolls over to new messages past the 2000-char limit.
func TestStreamSinkUpdateAndRollover(t *testing.T) {
	backend, fs := rendererBackend(t)
	sink := backend.OpenStream().(*discordStreamSink)

	sink.Update("hello")
	if fs.sendCount() != 1 || len(sink.MsgIDs()) != 1 {
		t.Fatalf("expected 1 send, got %d", fs.sendCount())
	}

	// Same text again: unchanged chunk skipped (no edit, no send).
	sink.Update("hello")
	if len(fs.edits) != 0 || fs.sendCount() != 1 {
		t.Error("expected unchanged chunk skipped")
	}

	// Grown text: edits the existing message.
	sink.Update("hello world")
	if len(fs.edits) != 1 || fs.lastEdit(t).content != "hello world" {
		t.Errorf("expected edit with grown text, edits=%d", len(fs.edits))
	}

	// Past the limit: rolls over to a second message.
	long := strings.Repeat("line\n", 500) // 2500 chars
	sink.Update(long)
	if len(sink.MsgIDs()) != 2 {
		t.Errorf("expected rollover to 2 messages, got %d", len(sink.MsgIDs()))
	}
}

// TestStreamSinkClose verifies Close reports whether messages surfaced and
// stops further updates.
func TestStreamSinkClose(t *testing.T) {
	backend, fs := rendererBackend(t)

	empty := backend.OpenStream()
	if empty.Close() {
		t.Error("expected surfaced=false for unused sink")
	}

	sink := backend.OpenStream()
	sink.Update("text")
	if !sink.Close() {
		t.Error("expected surfaced=true")
	}
	sends := fs.sendCount()
	sink.Update("more text")
	if fs.sendCount() != sends {
		t.Error("expected no sends after Close")
	}
}

// TestNewTurnRenderer verifies the renderer factory wires the discord backend
// without panicking, and SendTyping/Logger delegate to the bot.
func TestNewTurnRenderer(t *testing.T) {
	b, fs, _ := newTestBot(t, "a")
	b.SetChatID(42)
	tracker := newToolCallTracker(b, "42", turn.TurnDisplay{})
	r := newTurnRenderer(b, testDiscordMessage("42", "u1", "hi"), tracker, turn.TurnDisplay{DisplayWidth: 20})
	if r == nil {
		t.Fatal("expected renderer")
	}

	backend := &discordBackend{bot: b, channelID: "42"}
	backend.SendTyping()
	if fs.typingCalls != 1 {
		t.Errorf("expected typing call, got %d", fs.typingCalls)
	}
	b.SetTyping(false)
	if backend.Logger() == nil {
		t.Error("expected logger")
	}
}
