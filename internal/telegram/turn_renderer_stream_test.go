package telegram

import (
	"fmt"
	"strings"
	"testing"

	"foci/internal/command"
	"foci/internal/turn"
)

// newStreamSink builds a telegramStreamSink over a fresh test bot.
func newStreamSink(t *testing.T) (*telegramStreamSink, *Bot, *mockClient) {
	t.Helper()
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	backend := testBackend(b, 12345)
	return backend.OpenStream().(*telegramStreamSink), b, mock
}

func TestStreamSink_UpdateSendsThenEdits(t *testing.T) {
	// Proves the first Update sends a new live message and a subsequent
	// Update with changed text edits it in place rather than sending again,
	// while an identical Update is skipped entirely (no churn).
	s, _, mock := newStreamSink(t)

	s.Update("Hello")
	if mock.sentCount() != 1 || mock.editCount() != 0 {
		t.Fatalf("after first update: sends=%d edits=%d, want 1/0", mock.sentCount(), mock.editCount())
	}

	s.Update("Hello world")
	if mock.sentCount() != 1 || mock.editCount() != 1 {
		t.Fatalf("after second update: sends=%d edits=%d, want 1/1", mock.sentCount(), mock.editCount())
	}

	s.Update("Hello world")
	if mock.editCount() != 1 {
		t.Errorf("unchanged update caused churn: edits=%d, want 1", mock.editCount())
	}

	ids := s.MsgIDs()
	if len(ids) != 1 || ids[0] != "1" {
		t.Errorf("MsgIDs = %v, want [1]", ids)
	}
}

func TestStreamSink_RolloverBeyondLimit(t *testing.T) {
	// Proves an accumulated text longer than one Telegram message rolls over:
	// the first chunk is edited and a second live message is sent.
	s, _, mock := newStreamSink(t)

	s.Update("start")
	s.Update(strings.Repeat("x", telegramMaxChars+100))

	if mock.sentCount() != 2 {
		t.Errorf("sends = %d, want 2 (rollover message)", mock.sentCount())
	}
	if ids := s.MsgIDs(); len(ids) != 2 {
		t.Errorf("MsgIDs = %v, want 2 entries", ids)
	}
}

func TestStreamSink_EditFallsBackToPlainText(t *testing.T) {
	// Proves a failed HTML edit retries as plain text (Telegram rejects bad
	// HTML; the stream must keep flowing rather than stall).
	s, _, mock := newStreamSink(t)
	s.Update("first")

	mock.editErr = fmt.Errorf("Bad Request: can't parse entities")
	mock.editErrOnce = true
	s.Update("second")

	if mock.editCount() != 2 {
		t.Errorf("edits = %d, want 2 (HTML attempt + plain fallback)", mock.editCount())
	}
	if mock.lastEditOpts.ParseMode != "" {
		t.Errorf("fallback parse mode = %q, want plain", mock.lastEditOpts.ParseMode)
	}
}

func TestStreamSink_CloseStopsUpdatesAndReportsSurfaced(t *testing.T) {
	// Proves Close reports whether any message surfaced and that updates
	// after Close are ignored.
	s, _, mock := newStreamSink(t)

	s.Update("live")
	if !s.Close() {
		t.Error("Close = false, want true (a message surfaced)")
	}
	s.Update("after close")
	if mock.sentCount() != 1 || mock.editCount() != 0 {
		t.Errorf("post-close update reached API: sends=%d edits=%d", mock.sentCount(), mock.editCount())
	}

	empty, _, _ := newStreamSink(t)
	if empty.Close() {
		t.Error("Close = true on empty stream, want false")
	}
}

func TestStreamSink_SendFallsBackToPlainText(t *testing.T) {
	// Proves a failed HTML send retries as plain text so streaming output is
	// not lost on an HTML parse error.
	s, _, mock := newStreamSink(t)
	mock.sendErr = fmt.Errorf("Bad Request: can't parse entities")
	mock.sendErrOnce = true

	s.Update("hello")
	if mock.sentCount() != 2 {
		t.Errorf("sends = %d, want 2 (HTML attempt + plain fallback)", mock.sentCount())
	}
	if len(s.MsgIDs()) != 1 {
		t.Errorf("MsgIDs = %v, want one surfaced message", s.MsgIDs())
	}
}

func TestDeliver_FinalizeInPlaceEditsLiveSequence(t *testing.T) {
	// Proves Deliver lays the final text over an existing live stream message
	// by editing it in place instead of sending a new message.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	backend := testBackend(b, 12345)
	stream := backend.OpenStream().(*telegramStreamSink)
	stream.Update("partial")
	sendsBefore := mock.sentCount()

	res, err := backend.Deliver(turn.Payload{Text: "final text", ThinkingMode: "off"}, stream)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if mock.sentCount() != sendsBefore {
		t.Errorf("sends = %d, want %d (no new message)", mock.sentCount(), sendsBefore)
	}
	if mock.editCount() != 1 {
		t.Errorf("edits = %d, want 1", mock.editCount())
	}
	if len(res.MsgIDs) != 1 || res.MsgIDs[0] != "1" {
		t.Errorf("MsgIDs = %v, want [1]", res.MsgIDs)
	}
}

func TestDeliver_FinalShorterThanStreamDeletesOrphans(t *testing.T) {
	// Proves that when the final text needs fewer messages than the live
	// stream produced, the leftover live messages are deleted.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	backend := testBackend(b, 12345)
	stream := backend.OpenStream().(*telegramStreamSink)
	stream.Update(strings.Repeat("x", telegramMaxChars+100)) // 2 live messages

	res, err := backend.Deliver(turn.Payload{Text: "short final", ThinkingMode: "off"}, stream)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if mock.editCount() != 1 {
		t.Errorf("edits = %d, want 1 (first live message)", mock.editCount())
	}
	if mock.deleteCount() != 1 {
		t.Errorf("deletes = %d, want 1 (orphan live message)", mock.deleteCount())
	}
	if len(res.MsgIDs) != 1 {
		t.Errorf("MsgIDs = %v, want 1 entry", res.MsgIDs)
	}
}

func TestDeliver_FinalLongerThanStreamAppends(t *testing.T) {
	// Proves that when the final text needs more messages than the live
	// stream produced, the extra chunks are appended as new messages.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	backend := testBackend(b, 12345)
	stream := backend.OpenStream().(*telegramStreamSink)
	stream.Update("partial") // 1 live message
	sendsBefore := mock.sentCount()

	res, err := backend.Deliver(turn.Payload{Text: strings.Repeat("y", telegramMaxChars+100), ThinkingMode: "off"}, stream)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if mock.editCount() != 1 {
		t.Errorf("edits = %d, want 1", mock.editCount())
	}
	if mock.sentCount() != sendsBefore+1 {
		t.Errorf("sends = %d, want %d (one appended chunk)", mock.sentCount(), sendsBefore+1)
	}
	if len(res.MsgIDs) != 2 {
		t.Errorf("MsgIDs = %v, want 2 entries", res.MsgIDs)
	}
}

func TestDeliver_CompactThinkingFreshSendAttachesButton(t *testing.T) {
	// Proves a fresh delivery in compact thinking mode sends the chunk with a
	// "Show thinking" button and stores the thinking entry for later toggle.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	backend := testBackend(b, 12345)

	res, err := backend.Deliver(turn.Payload{
		Text: "answer", ThinkingText: "the plan", ThinkingMode: "compact",
	}, nil)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if mock.sentCount() != 1 {
		t.Fatalf("sends = %d, want 1", mock.sentCount())
	}
	if mock.lastSendOpts.ReplyMarkup == nil {
		t.Fatal("expected thinking button on fresh compact send")
	}
	if len(res.MsgIDs) != 1 {
		t.Fatalf("MsgIDs = %v, want 1 entry", res.MsgIDs)
	}
	if _, ok := b.thinkingStore.Load(parseTelegramMsgID(res.MsgIDs[0])); !ok {
		t.Error("thinking entry not stored for sent message")
	}
}

func TestDeliver_CompactThinkingFinalizeEditsWithButton(t *testing.T) {
	// Proves finalize-in-place in compact mode edits the last live message
	// with the thinking button attached.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	backend := testBackend(b, 12345)
	stream := backend.OpenStream().(*telegramStreamSink)
	stream.Update("partial")

	_, err := backend.Deliver(turn.Payload{
		Text: "answer", ThinkingText: "the plan", ThinkingMode: "compact",
	}, stream)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if mock.editCount() != 1 {
		t.Errorf("edits = %d, want 1", mock.editCount())
	}
	if mock.lastEditOpts.ReplyMarkup.InlineKeyboard == nil {
		t.Error("expected thinking button on finalize edit")
	}
}

func TestSendChunk_FallbackAndFailure(t *testing.T) {
	// Proves SendChunk falls back to plain text when the HTML send fails, and
	// reports ok=false when both attempts fail.
	b, mock := testBot([]string{"111"}, command.NewRegistry())
	backend := testBackend(b, 12345)

	mock.sendErr = fmt.Errorf("Bad Request: can't parse entities")
	mock.sendErrOnce = true
	if id, ok := backend.SendChunk("text"); !ok || id == "" {
		t.Errorf("SendChunk = %q/%v, want an id from plain-text fallback", id, ok)
	}

	mock.sendErr = fmt.Errorf("persistent failure")
	mock.sendErrOnce = false
	if id, ok := backend.SendChunk("text"); ok || id != "" {
		t.Errorf("SendChunk = %q/%v, want \"\"/false when both sends fail", id, ok)
	}
}

func TestBuildThinkingHTML(t *testing.T) {
	// Proves the combined thinking surface is italic thinking + em-dash
	// divider of the display width + response, with thinking HTML-escaped.
	got := buildThinkingHTML("<b>resp</b>", "think <fast>", 10)
	if !strings.Contains(got, "<i>think &lt;fast&gt;</i>") {
		t.Errorf("thinking not escaped/italicised: %q", got)
	}
	if !strings.Contains(got, strings.Repeat("—", 10)) {
		t.Errorf("missing divider: %q", got)
	}
	if !strings.HasSuffix(got, "<b>resp</b>") {
		t.Errorf("response not at end: %q", got)
	}
}
