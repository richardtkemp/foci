package turn

import (
	"strings"
	"testing"
	"time"

	"foci/internal/log"
)

// mockDeliver records a Deliver call with the FULL (uncut) payload and whether
// a surfaced stream sequence was passed.
type mockDeliver struct {
	Payload      Payload
	StreamMsgIDs []string // MsgIDs() of the stream passed (nil if no stream surfaced)
}

// mockEditInPlace records an EditInPlace call.
type mockEditInPlace struct {
	MsgID   string
	Payload Payload
}

// mockBackend implements the Platform interface and records all calls. Layout
// (chopping at a char limit) is simulated via maxChars: when a payload's Text
// exceeds maxChars, EditInPlace returns ErrTooLongForEdit (driving the
// over-limit fallback) — Deliver never truncates, mirroring the real platform.
type mockBackend struct {
	deliverCalls     []mockDeliver
	editInPlaceCalls []mockEditInPlace
	typingCount      int
	log              *log.ComponentLogger

	// maxChars > 0 makes EditInPlace return ErrTooLongForEdit when the payload
	// text exceeds it (simulating the platform's chop-would-split decision).
	maxChars int
	// editInPlaceErr, when set, overrides the maxChars check and is returned
	// from EditInPlace unconditionally.
	editInPlaceErr error
	// deliverResult is returned from Deliver (its MsgIDs).
	deliverResult DeliveryResult
}

func newMockBackend() *mockBackend {
	return &mockBackend{log: log.NewComponentLogger("test")}
}

func (m *mockBackend) OpenStream() StreamSink {
	// Default: a non-surfacing sink. Tests that need a surfaced stream build
	// the factory with a configured mockSink (see liveSBFactory).
	return &mockSink{}
}

func (m *mockBackend) Deliver(p Payload, stream StreamSink) (DeliveryResult, error) {
	call := mockDeliver{Payload: p}
	if stream != nil {
		call.StreamMsgIDs = stream.MsgIDs()
	}
	m.deliverCalls = append(m.deliverCalls, call)
	return m.deliverResult, nil
}

func (m *mockBackend) EditInPlace(msgID string, p Payload) error {
	m.editInPlaceCalls = append(m.editInPlaceCalls, mockEditInPlace{MsgID: msgID, Payload: p})
	if m.editInPlaceErr != nil {
		return m.editInPlaceErr
	}
	if m.maxChars > 0 && len(p.Text) > m.maxChars {
		return ErrTooLongForEdit
	}
	return nil
}

func (m *mockBackend) SendTyping() {
	m.typingCount++
}

func (m *mockBackend) Logger() *log.ComponentLogger {
	return m.log
}

// mockTracker records ToolTracker calls.
type mockTracker struct {
	lastID       string
	resetCalled  bool
	cleanupCount int
}

func (t *mockTracker) LastMsgID() string { return t.lastID }
func (t *mockTracker) ResetMsgID()       { t.resetCalled = true; t.lastID = "" }
func (t *mockTracker) CleanupPreview()   { t.cleanupCount++; t.lastID = "" }

// newTestSB creates a non-live StreamBuffer for tests (buffer-only, no sink
// surfacing). Used by renderers that don't exercise live streaming.
func newTestSB() *StreamBuffer {
	return NewStreamBuffer(&mockSink{}, 50*time.Millisecond, false)
}

func newTestRenderer(backend *mockBackend, tracker *mockTracker, display TurnDisplay) *TurnRenderer {
	return NewTurnRenderer(backend, tracker, display, newTestSB)
}

// mockSubagentBackend is a Platform that also implements SubagentDeliverer, for
// testing subagent presentation. raw toggles SubagentTextRaw.
type mockSubagentBackend struct {
	*mockBackend
	raw   bool
	texts []string
}

func (m *mockSubagentBackend) DeliverSubagentStart(string, string, int, string) {}
func (m *mockSubagentBackend) DeliverSubagentText(_, text string, _ int) { m.texts = append(m.texts, text) }
func (m *mockSubagentBackend) DeliverSubagentEnd(string, int)            {}
func (m *mockSubagentBackend) SubagentTextRaw() bool                { return m.raw }

// The renderer heads each blockquoted subagent block with the agent name (from
// the start event) for non-raw platforms, and passes text through unchanged for
// the app (raw).
func TestRenderer_SubagentHeader(t *testing.T) {
	nonRaw := &mockSubagentBackend{mockBackend: newMockBackend()}
	r := NewTurnRenderer(nonRaw, &mockTracker{}, TurnDisplay{}, newTestSB)
	r.OnSubagentStart("g1", "Explore", 1, "")
	r.OnSubagentReply("g1", "found it", 1)
	if len(nonRaw.texts) != 1 || nonRaw.texts[0] != "**Explore**\n> found it" {
		t.Fatalf("non-raw text = %q, want [\"**Explore**\\n> found it\"]", nonRaw.texts)
	}

	raw := &mockSubagentBackend{mockBackend: newMockBackend(), raw: true}
	r2 := NewTurnRenderer(raw, &mockTracker{}, TurnDisplay{}, newTestSB)
	r2.OnSubagentStart("g2", "Plan", 1, "")
	r2.OnSubagentReply("g2", "done", 1)
	if len(raw.texts) != 1 || raw.texts[0] != "done" {
		t.Fatalf("raw text = %q, want [done]", raw.texts)
	}
}

// liveSBFactory returns a factory producing live StreamBuffers that share the
// given sink. The sink's surfacedRet/msgIDsRet drive the renderer's surfaced
// branch. Because the renderer recreates the buffer per segment, the same sink
// is reused across segments — acceptable for the single-segment tests that use
// this; multi-segment tests construct their own factory.
func liveSBFactory(sink *mockSink) func() *StreamBuffer {
	return func() *StreamBuffer {
		return NewStreamBuffer(sink, 50*time.Millisecond, true)
	}
}

func TestOnReply_StreamEnabled_NoDeltasArrived_StillDelivers(t *testing.T) {
	// BUG FIX TEST: when streaming is enabled but no deltas arrived (no surface),
	// the text must still be delivered via Deliver.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true}
	r := newTestRenderer(backend, tracker, display)

	r.OnReply("important text")

	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	if backend.deliverCalls[0].Payload.Text != "important text" {
		t.Errorf("deliver text = %q, want %q", backend.deliverCalls[0].Payload.Text, "important text")
	}
	if backend.deliverCalls[0].StreamMsgIDs != nil {
		t.Errorf("expected no stream msgIDs (no surface), got %v", backend.deliverCalls[0].StreamMsgIDs)
	}
}

func TestOnReply_StreamEnabled_DeltasArrived_FinalizesStream(t *testing.T) {
	// When the live stream surfaced, OnReply delivers terminally reusing the
	// stream sequence (one Deliver carrying the stream's msgIDs), and cleans up
	// the tool preview.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true}
	sink := &mockSink{surfacedRet: true, msgIDsRet: []string{"100"}}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))

	r.sw.OnDelta("streamed content")
	r.OnReply("reply text")

	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	if backend.deliverCalls[0].Payload.Text != "reply text" {
		t.Errorf("deliver text = %q, want %q", backend.deliverCalls[0].Payload.Text, "reply text")
	}
	if len(backend.deliverCalls[0].StreamMsgIDs) != 1 || backend.deliverCalls[0].StreamMsgIDs[0] != "100" {
		t.Errorf("deliver stream msgIDs = %v, want [100]", backend.deliverCalls[0].StreamMsgIDs)
	}
	if len(backend.editInPlaceCalls) != 0 {
		t.Errorf("editInPlace calls = %d, want 0", len(backend.editInPlaceCalls))
	}
	if tracker.cleanupCount != 1 {
		t.Errorf("cleanupPreview calls = %d, want 1", tracker.cleanupCount)
	}
}

func TestOnReply_StreamEnabled_NoThinkingInPayload(t *testing.T) {
	// Intermediate replies never carry thinking, even when thinking was streamed
	// live during the segment. The Payload to Deliver has no thinking.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true, ShowThinking: "compact"}
	sink := &mockSink{surfacedRet: true, msgIDsRet: []string{"100"}}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))

	r.OnThinking("internal reasoning")
	r.OnTextDelta("visible reply")
	r.OnReply("visible reply")

	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	p := backend.deliverCalls[0].Payload
	if p.Text != "visible reply" {
		t.Errorf("deliver text = %q, want %q", p.Text, "visible reply")
	}
	if p.ThinkingText != "" || (p.ThinkingMode != "" && p.ThinkingMode != "off") {
		t.Errorf("intermediate reply must carry no thinking, got mode=%q text=%q", p.ThinkingMode, p.ThinkingText)
	}
}

func TestOnReply_StreamEnabled_DeltasArrived_FreshWriterForNextSegment(t *testing.T) {
	// After OnReply delivers, a fresh stream buffer is created for the next
	// segment. The second OnReply takes the no-surface path.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true}
	// First segment surfaces; the recreated buffer for segment 2 also wraps this
	// sink, so reset surfacedRet between calls to simulate "no deltas arrived".
	sink := &mockSink{surfacedRet: true, msgIDsRet: []string{"100"}}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))

	r.sw.OnDelta("first segment")
	r.OnReply("reply 1") // delivers reusing stream "100"

	// Segment 2: no deltas, simulate non-surface.
	sink.mu.Lock()
	sink.surfacedRet = false
	sink.msgIDsRet = nil
	sink.mu.Unlock()
	r.OnReply("reply 2")

	if len(backend.deliverCalls) != 2 {
		t.Fatalf("deliver calls = %d, want 2", len(backend.deliverCalls))
	}
	if backend.deliverCalls[1].Payload.Text != "reply 2" {
		t.Errorf("second deliver text = %q, want %q", backend.deliverCalls[1].Payload.Text, "reply 2")
	}
	if backend.deliverCalls[1].StreamMsgIDs != nil {
		t.Errorf("second deliver should have no stream msgIDs, got %v", backend.deliverCalls[1].StreamMsgIDs)
	}
}

func TestOnReply_NoStream_EditsToolPreview(t *testing.T) {
	// When no stream surfaced and a tool preview exists, OnReply edits it
	// in-place rather than delivering a new message.
	backend := newMockBackend()
	tracker := &mockTracker{lastID: "50"}
	display := TurnDisplay{ShowToolCalls: "preview"}
	r := newTestRenderer(backend, tracker, display)

	r.OnReply("reply text")

	if len(backend.editInPlaceCalls) != 1 {
		t.Fatalf("editInPlace calls = %d, want 1", len(backend.editInPlaceCalls))
	}
	if backend.editInPlaceCalls[0].MsgID != "50" {
		t.Errorf("edit msgID = %q, want %q", backend.editInPlaceCalls[0].MsgID, "50")
	}
	if len(backend.deliverCalls) != 0 {
		t.Errorf("deliver calls = %d, want 0 (should edit preview)", len(backend.deliverCalls))
	}
}

func TestOnReply_NoStream_NoPreview_SendsReply(t *testing.T) {
	// When no stream and no tool preview, OnReply delivers a new message.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{}
	r := newTestRenderer(backend, tracker, display)

	r.OnReply("reply text")

	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	if backend.deliverCalls[0].Payload.Text != "reply text" {
		t.Errorf("deliver text = %q, want %q", backend.deliverCalls[0].Payload.Text, "reply text")
	}
	if !tracker.resetCalled {
		t.Error("expected ResetMsgID to be called")
	}
}

func TestOnReply_ToolPreviewTooLong_CleansUpAndSends(t *testing.T) {
	// When the reply exceeds the platform's edit-in-place limit, EditInPlace
	// returns ErrTooLongForEdit: the preview is cleaned up and the reply is
	// delivered as a new (split) message — with the FULL text, uncut.
	backend := newMockBackend()
	backend.maxChars = 10 // edit-in-place limit
	tracker := &mockTracker{lastID: "50"}
	display := TurnDisplay{ShowToolCalls: "preview"}
	r := newTestRenderer(backend, tracker, display)

	const tail = "TAILMARKER"
	longReply := "this text exceeds maxchars " + tail
	r.OnReply(longReply)

	if len(backend.editInPlaceCalls) != 1 {
		t.Fatalf("editInPlace calls = %d, want 1 (attempted before falling back)", len(backend.editInPlaceCalls))
	}
	if tracker.cleanupCount != 1 {
		t.Errorf("cleanupPreview calls = %d, want 1", tracker.cleanupCount)
	}
	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1 (fallback after cleanup)", len(backend.deliverCalls))
	}
	if backend.deliverCalls[0].Payload.Text != longReply {
		t.Errorf("deliver text = %q, want full reply %q (no truncation)", backend.deliverCalls[0].Payload.Text, longReply)
	}
	if !strings.Contains(backend.deliverCalls[0].Payload.Text, tail) {
		t.Errorf("tail marker %q missing from delivered text — reply was truncated", tail)
	}
}

// TestOnReply_StreamMessage_OverMaxChars_SplitsNotInPlaceEdit is the #738 guard.
// When a live stream surfaced and the full reply far exceeds any single-message
// limit, OnReply must hand the FULL untruncated text to Deliver (with the stream
// sequence) — never a truncating in-place edit. The platform owns splitting.
func TestOnReply_StreamMessage_OverMaxChars_SplitsNotInPlaceEdit(t *testing.T) {
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true}
	sink := &mockSink{surfacedRet: true, msgIDsRet: []string{"100"}}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))

	// A short delta surfaces the live stream.
	r.sw.OnDelta("partial streamed prefix")

	// Full reply far exceeds any single-message limit; the tail sits well past it.
	const tail = "TAILMARKER738"
	longReply := strings.Repeat("a", 5000) + tail

	r.OnReply(longReply)

	// Contract: the FULL reply reaches Deliver uncut, and a stream sequence is
	// passed so the platform can roll over / reuse the live messages.
	if len(backend.deliverCalls) != 1 {
		t.Fatalf("#738: deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	if len(backend.editInPlaceCalls) != 0 {
		t.Fatalf("#738: editInPlace must NOT be used for a surfaced stream (got %d calls)", len(backend.editInPlaceCalls))
	}
	got := backend.deliverCalls[0].Payload.Text
	if got != longReply {
		t.Fatalf("#738: deliver text len=%d, want full reply len=%d (turn must not truncate)", len(got), len(longReply))
	}
	if !strings.Contains(got, tail) {
		t.Errorf("#738: tail marker %q missing from delivered text — reply was truncated before delivery", tail)
	}
	if len(backend.deliverCalls[0].StreamMsgIDs) != 1 || backend.deliverCalls[0].StreamMsgIDs[0] != "100" {
		t.Errorf("#738: deliver must pass the stream sequence, got msgIDs=%v", backend.deliverCalls[0].StreamMsgIDs)
	}
}

func TestFinalize_EmptyResponse(t *testing.T) {
	// Empty response → silent branch: no delivery, preview cleaned up.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{}
	r := newTestRenderer(backend, tracker, display)

	r.Finalize("")

	if len(backend.deliverCalls) != 0 {
		t.Errorf("deliver calls = %d, want 0 (empty response is silent)", len(backend.deliverCalls))
	}
}

func TestFinalize_StreamShort_NoThinking(t *testing.T) {
	// Stream surfaced, no thinking → Deliver reusing the stream sequence, plain payload.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true}
	sink := &mockSink{surfacedRet: true, msgIDsRet: []string{"100"}}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))
	r.sw.OnDelta("stream content")

	r.Finalize("final response")

	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	p := backend.deliverCalls[0].Payload
	if p.Text != "final response" {
		t.Errorf("deliver text = %q, want %q", p.Text, "final response")
	}
	if p.ThinkingMode != "off" {
		t.Errorf("thinking mode = %q, want off", p.ThinkingMode)
	}
	if len(backend.deliverCalls[0].StreamMsgIDs) != 1 || backend.deliverCalls[0].StreamMsgIDs[0] != "100" {
		t.Errorf("deliver stream msgIDs = %v, want [100]", backend.deliverCalls[0].StreamMsgIDs)
	}
	if tracker.cleanupCount != 1 {
		t.Errorf("cleanupPreview calls = %d, want 1", tracker.cleanupCount)
	}
}

func TestFinalize_StreamShort_CompactThinking(t *testing.T) {
	// Stream surfaced + compact thinking → Deliver with ThinkingMode "compact".
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true, ShowThinking: "compact"}
	sink := &mockSink{surfacedRet: true, msgIDsRet: []string{"100"}}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))
	r.sw.OnDelta("stream content")
	r.OnThinking("deep thoughts")

	r.Finalize("final response")

	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	p := backend.deliverCalls[0].Payload
	if p.ThinkingMode != "compact" {
		t.Errorf("thinking mode = %q, want compact", p.ThinkingMode)
	}
	if p.ThinkingText != "deep thoughts" {
		t.Errorf("thinkingText = %q, want %q", p.ThinkingText, "deep thoughts")
	}
}

func TestFinalize_StreamShort_FullThinking(t *testing.T) {
	// Stream surfaced + "true" thinking → Deliver with ThinkingMode "full".
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true, ShowThinking: "true"}
	sink := &mockSink{surfacedRet: true, msgIDsRet: []string{"100"}}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))
	r.sw.OnDelta("stream content")
	r.OnThinking("deep thoughts")

	r.Finalize("final response")

	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	p := backend.deliverCalls[0].Payload
	if p.ThinkingMode != "full" {
		t.Errorf("thinking mode = %q, want full", p.ThinkingMode)
	}
	if p.ThinkingText != "deep thoughts" {
		t.Errorf("thinkingText = %q, want %q", p.ThinkingText, "deep thoughts")
	}
}

func TestFinalize_StreamLong_SendsFullTextWithStream(t *testing.T) {
	// When the stream surfaced and the response is very long, Finalize still
	// hands the FULL text to Deliver with the stream sequence — turn never
	// truncates, the platform splits/rolls over.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true}
	sink := &mockSink{surfacedRet: true, msgIDsRet: []string{"100"}}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))
	r.sw.OnDelta("x")

	const tail = "TAILMARKERLONG"
	longResponse := strings.Repeat("x", 5000) + tail
	r.Finalize(longResponse)

	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	got := backend.deliverCalls[0].Payload.Text
	if got != longResponse {
		t.Fatalf("deliver text len=%d, want full response len=%d (no truncation)", len(got), len(longResponse))
	}
	if !strings.Contains(got, tail) {
		t.Errorf("tail marker %q missing — response truncated before delivery", tail)
	}
	if len(backend.deliverCalls[0].StreamMsgIDs) != 1 {
		t.Errorf("deliver must pass stream sequence, got %v", backend.deliverCalls[0].StreamMsgIDs)
	}
}

func TestFinalize_NoStream_ToolPreview(t *testing.T) {
	// No stream, no thinking, tool preview present → edit in place.
	backend := newMockBackend()
	tracker := &mockTracker{lastID: "50"}
	display := TurnDisplay{ShowToolCalls: "preview"}
	r := newTestRenderer(backend, tracker, display)

	r.Finalize("short response")

	if len(backend.editInPlaceCalls) != 1 {
		t.Fatalf("editInPlace calls = %d, want 1", len(backend.editInPlaceCalls))
	}
	if backend.editInPlaceCalls[0].MsgID != "50" {
		t.Errorf("edit msgID = %q, want %q", backend.editInPlaceCalls[0].MsgID, "50")
	}
	if len(backend.deliverCalls) != 0 {
		t.Errorf("deliver calls = %d, want 0", len(backend.deliverCalls))
	}
}

func TestFinalize_NoStream_NewMessage(t *testing.T) {
	// No stream, no preview → deliver a new message.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{}
	r := newTestRenderer(backend, tracker, display)

	r.Finalize("response text")

	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	if backend.deliverCalls[0].Payload.Text != "response text" {
		t.Errorf("deliver text = %q, want %q", backend.deliverCalls[0].Payload.Text, "response text")
	}
	if tracker.cleanupCount != 1 {
		t.Errorf("cleanupPreview calls = %d, want 1", tracker.cleanupCount)
	}
}

func TestFinalize_StreamContentFallback(t *testing.T) {
	// Empty response but stream has content → use stream content, deliver with
	// the surfaced stream sequence.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true}
	sink := &mockSink{surfacedRet: true, msgIDsRet: []string{"100"}}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))
	r.sw.OnDelta("stream fallback content")

	r.Finalize("")

	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	if backend.deliverCalls[0].Payload.Text != "stream fallback content" {
		t.Errorf("deliver text = %q, want stream content", backend.deliverCalls[0].Payload.Text)
	}
}

func TestFinalize_NoStream_FullThinking_SendsCombined(t *testing.T) {
	// No stream, "true" thinking → deliver with ThinkingMode "full".
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "true"}
	r := newTestRenderer(backend, tracker, display)
	r.OnThinking("my thoughts")

	r.Finalize("response")

	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	p := backend.deliverCalls[0].Payload
	if p.ThinkingMode != "full" {
		t.Errorf("thinking mode = %q, want full", p.ThinkingMode)
	}
	if p.ThinkingText != "my thoughts" {
		t.Errorf("thinkingText = %q, want %q", p.ThinkingText, "my thoughts")
	}
}

func TestFinalize_NoStream_CompactThinking_SendsWithButton(t *testing.T) {
	// No stream, compact thinking → deliver with ThinkingMode "compact".
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact"}
	r := newTestRenderer(backend, tracker, display)
	r.OnThinking("my thoughts")

	r.Finalize("response")

	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	p := backend.deliverCalls[0].Payload
	if p.ThinkingMode != "compact" {
		t.Errorf("thinking mode = %q, want compact", p.ThinkingMode)
	}
	if p.ThinkingText != "my thoughts" {
		t.Errorf("thinkingText = %q, want %q", p.ThinkingText, "my thoughts")
	}
}

func TestFinalize_NoStream_CompactThinking_NotEditedAsPreview(t *testing.T) {
	// Even with a tool preview present, a thinking-carrying Finalize must NOT
	// use EditInPlace (previews can't carry thinking buttons) — it must Deliver.
	backend := newMockBackend()
	tracker := &mockTracker{lastID: "50"}
	display := TurnDisplay{ShowToolCalls: "preview", ShowThinking: "compact"}
	r := newTestRenderer(backend, tracker, display)
	r.OnThinking("my thoughts")

	r.Finalize("response")

	if len(backend.editInPlaceCalls) != 0 {
		t.Errorf("editInPlace calls = %d, want 0 (thinking can't edit preview)", len(backend.editInPlaceCalls))
	}
	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	if backend.deliverCalls[0].Payload.ThinkingMode != "compact" {
		t.Errorf("thinking mode = %q, want compact", backend.deliverCalls[0].Payload.ThinkingMode)
	}
}

func TestOnThinking_Accumulates(t *testing.T) {
	// Thinking blocks accumulate with newline separators; surfaced as ThinkingText.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true}
	sink := &mockSink{surfacedRet: true, msgIDsRet: []string{"100"}}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))
	r.sw.OnDelta("x")

	r.OnThinking("thought 1")
	r.OnThinking("thought 2")

	r.Finalize("response")

	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	if backend.deliverCalls[0].Payload.ThinkingText != "thought 1\nthought 2" {
		t.Errorf("thinkingText = %q, want %q", backend.deliverCalls[0].Payload.ThinkingText, "thought 1\nthought 2")
	}
}

func TestOnThinking_Off_Ignored(t *testing.T) {
	// Thinking blocks ignored when mode is "off"; payload has no thinking.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "off"}
	r := newTestRenderer(backend, tracker, display)

	r.OnThinking("should be ignored")
	r.Finalize("response")

	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	if backend.deliverCalls[0].Payload.ThinkingMode != "off" {
		t.Errorf("thinking mode = %q, want off", backend.deliverCalls[0].Payload.ThinkingMode)
	}
	if backend.deliverCalls[0].Payload.ThinkingText != "" {
		t.Errorf("thinkingText = %q, want empty", backend.deliverCalls[0].Payload.ThinkingText)
	}
}

func TestOnThinking_Compact_StreamsLive(t *testing.T) {
	// Compact + streaming → thinking written into the stream buffer; typing refreshed.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true}
	sink := &mockSink{surfacedRet: true, msgIDsRet: []string{"100"}}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))

	r.OnThinking("analyzing the problem")

	if content := r.sw.Content(); content != "analyzing the problem" {
		t.Errorf("stream content = %q, want %q", content, "analyzing the problem")
	}
	if backend.typingCount != 1 {
		t.Errorf("typing count = %d, want 1", backend.typingCount)
	}
}

func TestOnThinking_True_StreamsLive(t *testing.T) {
	// "true" + streaming → thinking written into the stream buffer; typing refreshed.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "true", StreamOutput: true}
	sink := &mockSink{surfacedRet: true, msgIDsRet: []string{"100"}}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))

	r.OnThinking("deep analysis")

	if content := r.sw.Content(); content != "deep analysis" {
		t.Errorf("stream content = %q, want %q", content, "deep analysis")
	}
	if backend.typingCount != 1 {
		t.Errorf("typing count = %d, want 1", backend.typingCount)
	}
}

func TestOnThinking_Off_NoStream(t *testing.T) {
	// Mode "off" → thinking not streamed even with streaming enabled.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "off", StreamOutput: true}
	sink := &mockSink{}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))

	r.OnThinking("should not appear")

	if content := r.sw.Content(); content != "" {
		t.Errorf("stream content = %q, want empty", content)
	}
}

func TestOnTextDelta_InsertsDividerAfterThinking(t *testing.T) {
	// First text delta after thinking inserts a divider in the stream buffer.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true}
	sink := &mockSink{}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))

	r.OnThinking("let me think")
	r.OnTextDelta("Hello")
	r.OnTextDelta(" world")

	want := "let me think\n\n---\n\nHello world"
	if content := r.sw.Content(); content != want {
		t.Errorf("stream content = %q, want %q", content, want)
	}
}

func TestOnTextDelta_NoDividerWithoutThinking(t *testing.T) {
	// No thinking before text deltas → no divider.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true}
	sink := &mockSink{}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))

	r.OnTextDelta("Hello")
	r.OnTextDelta(" world")

	if content := r.sw.Content(); content != "Hello world" {
		t.Errorf("stream content = %q, want %q", content, "Hello world")
	}
}

func TestFinalize_StreamContentFallback_StripsThinking(t *testing.T) {
	// Empty response + thinking streamed live → Content() fallback strips
	// thinking+divider and delivers the text after the divider, with thinking.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true}
	sink := &mockSink{surfacedRet: true, msgIDsRet: []string{"100"}}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))

	r.OnThinking("reasoning here")
	r.OnTextDelta("actual response")
	r.Finalize("") // empty response triggers Content() fallback

	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	p := backend.deliverCalls[0].Payload
	if p.Text != "actual response" {
		t.Errorf("deliver text = %q, want %q", p.Text, "actual response")
	}
	if p.ThinkingMode != "compact" {
		t.Errorf("thinking mode = %q, want compact", p.ThinkingMode)
	}
	if p.ThinkingText != "reasoning here" {
		t.Errorf("thinkingText = %q, want %q", p.ThinkingText, "reasoning here")
	}
}

func TestFinalize_StreamContentFallback_ThinkingOnly(t *testing.T) {
	// Empty response + only thinking streamed (no text deltas) → no text content
	// after the divider, so the response stays empty → silent branch (no deliver).
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true}
	sink := &mockSink{surfacedRet: true, msgIDsRet: []string{"100"}}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))

	r.OnThinking("reasoning only")
	r.Finalize("") // no text deltas, no response

	if len(backend.deliverCalls) != 0 {
		t.Errorf("deliver calls = %d, want 0 (thinking-only, no text → silent)", len(backend.deliverCalls))
	}
}

func TestOnReply_ResetsThinkingPhase(t *testing.T) {
	// OnReply resets thinkingPhase so the next segment doesn't insert a stale divider.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true}
	sink := &mockSink{surfacedRet: true, msgIDsRet: []string{"100"}}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))

	r.OnThinking("segment 1 thinking")
	r.OnReply("reply 1") // resets phase

	r.OnTextDelta("segment 2 text")
	if content := r.sw.Content(); strings.Contains(content, "---") {
		t.Errorf("stream content should not contain divider after phase reset, got %q", content)
	}
}

func TestFinalize_CompactStream_FullFlow(t *testing.T) {
	// End-to-end: compact + streaming → finalize delivers response with compact
	// thinking and the accumulated thinking text.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true}
	sink := &mockSink{surfacedRet: true, msgIDsRet: []string{"100"}}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))

	r.OnThinking("I need to consider ")
	r.OnThinking("this carefully")
	r.OnTextDelta("Here is ")
	r.OnTextDelta("the answer.")

	r.Finalize("Here is the answer.")

	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	p := backend.deliverCalls[0].Payload
	if p.Text != "Here is the answer." {
		t.Errorf("deliver text = %q, want %q", p.Text, "Here is the answer.")
	}
	if p.ThinkingMode != "compact" {
		t.Errorf("thinking mode = %q, want compact", p.ThinkingMode)
	}
	if p.ThinkingText != "I need to consider \nthis carefully" {
		t.Errorf("thinkingText = %q, want accumulated thinking", p.ThinkingText)
	}
}

func TestFinalize_TrueStream_FullFlow(t *testing.T) {
	// End-to-end: true + streaming → finalize delivers response with full thinking.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "true", StreamOutput: true}
	sink := &mockSink{surfacedRet: true, msgIDsRet: []string{"100"}}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))

	r.OnThinking("deep thoughts")
	r.OnTextDelta("The answer is 42.")

	r.Finalize("The answer is 42.")

	if len(backend.deliverCalls) != 1 {
		t.Fatalf("deliver calls = %d, want 1", len(backend.deliverCalls))
	}
	p := backend.deliverCalls[0].Payload
	if p.ThinkingMode != "full" {
		t.Errorf("thinking mode = %q, want full", p.ThinkingMode)
	}
	if p.ThinkingText != "deep thoughts" {
		t.Errorf("thinkingText = %q, want %q", p.ThinkingText, "deep thoughts")
	}
}

func TestOnTextDelta_SendsTyping(t *testing.T) {
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{}
	r := newTestRenderer(backend, tracker, display)

	r.OnTextDelta("hello")

	if backend.typingCount != 1 {
		t.Errorf("typing count = %d, want 1", backend.typingCount)
	}
}

func TestOnActivity_SendsTyping(t *testing.T) {
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{}
	r := newTestRenderer(backend, tracker, display)

	r.OnActivity()

	if backend.typingCount != 1 {
		t.Errorf("typing count = %d, want 1", backend.typingCount)
	}
}

func TestCleanup_Idempotent(t *testing.T) {
	// Cleanup is safe to call after Finalize.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{}
	r := newTestRenderer(backend, tracker, display)

	r.Finalize("response")
	r.Cleanup() // should not panic
}

func TestFinalize_RealTextDelivered(t *testing.T) {
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{}
	r := newTestRenderer(backend, tracker, display)

	r.Finalize("Here is my answer.")

	if len(backend.deliverCalls)+len(backend.editInPlaceCalls) == 0 {
		t.Error("Finalize with real text should produce output")
	}
}

func TestOnReply_RealTextDelivered(t *testing.T) {
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{}
	r := newTestRenderer(backend, tracker, display)

	r.OnReply("Here is my answer.")

	if len(backend.deliverCalls)+len(backend.editInPlaceCalls) == 0 {
		t.Error("OnReply with real text should produce output")
	}
}

func TestFinalize_WithoutOnReply_DeliversOnce(t *testing.T) {
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{}
	r := newTestRenderer(backend, tracker, display)

	r.Finalize("my response")

	total := len(backend.deliverCalls) + len(backend.editInPlaceCalls)
	if total != 1 {
		t.Errorf("Finalize without prior OnReply should deliver once, got %d", total)
	}
}

func TestOnThinkingDelta_StreamsLive(t *testing.T) {
	// OnThinkingDelta writes each delta to the stream buffer and sets the
	// streamedThinkingLive guard so a subsequent OnThinking doesn't re-stream.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true}
	sink := &mockSink{}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))

	r.OnThinkingDelta("I need ")
	r.OnThinkingDelta("to think")
	r.OnThinking("I need to think")

	content := r.sw.Content()
	if strings.Count(content, "I need ") != 1 || strings.Count(content, "to think") != 1 {
		t.Errorf("deltas should stream once and OnThinking should not duplicate; got %q", content)
	}
	if r.thinking.String() != "I need to think" {
		t.Errorf("thinking builder = %q, want %q", r.thinking.String(), "I need to think")
	}
}

func TestOnThinkingDelta_NoStreamingDoesNothing(t *testing.T) {
	// The delta path is gated on StreamOutput; disabled → no-op.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: false}
	r := newTestRenderer(backend, tracker, display)

	r.OnThinkingDelta("fragment")

	if r.streamedThinkingLive {
		t.Error("streamedThinkingLive set despite StreamOutput=false")
	}
	if r.thinking.Len() != 0 {
		t.Error("builder populated by delta path; deltas must not accumulate")
	}
}

func TestOnThinking_FallbackWhenNoDelta(t *testing.T) {
	// OnThinking streams the full block when no delta fired.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true}
	sink := &mockSink{}
	r := NewTurnRenderer(backend, tracker, display, liveSBFactory(sink))

	r.OnThinking("whole block")

	if content := r.sw.Content(); !strings.Contains(content, "whole block") {
		t.Errorf("OnThinking block fallback did not stream text; got %q", content)
	}
}
