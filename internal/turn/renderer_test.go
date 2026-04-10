package turn

import (
	"strings"
	"testing"
	"time"

	"foci/internal/log"
)

// mockBackend records all TurnBackend calls for assertion.
type mockBackend struct {
	formatCalls       []string
	sendReplyCalls    []string
	sendChunkedCalls  []string
	editCalls         []mockEditCall
	thinkingBtnCalls  []mockThinkingBtnCall
	editThinkingCalls []mockEditThinkingCall
	combinedCalls     []mockCombinedCall
	previewCalls      []string
	typingCount       int
	log               *log.ComponentLogger

	editErr      error
	thinkBtnErr  error
	editThinkErr error
}

type mockEditCall struct {
	msgID     string
	formatted string
}

type mockThinkingBtnCall struct {
	formatted    string
	thinkingText string
}

type mockEditThinkingCall struct {
	msgID        string
	formatted    string
	thinkingText string
}

type mockCombinedCall struct {
	responseFormatted string
	thinkingText      string
}

func newMockBackend() *mockBackend {
	return &mockBackend{log: log.NewComponentLogger("test")}
}

func (m *mockBackend) FormatResponse(text string) string {
	m.formatCalls = append(m.formatCalls, text)
	return "[fmt]" + text
}

func (m *mockBackend) SendReply(text string) {
	m.sendReplyCalls = append(m.sendReplyCalls, text)
}

func (m *mockBackend) SendChunked(formatted string) {
	m.sendChunkedCalls = append(m.sendChunkedCalls, formatted)
}

func (m *mockBackend) EditMessage(msgID, formatted string) error {
	m.editCalls = append(m.editCalls, mockEditCall{msgID, formatted})
	return m.editErr
}

func (m *mockBackend) SendWithThinkingButton(formatted, thinkingText string) error {
	m.thinkingBtnCalls = append(m.thinkingBtnCalls, mockThinkingBtnCall{formatted, thinkingText})
	return m.thinkBtnErr
}

func (m *mockBackend) EditWithThinkingButton(msgID, formatted, thinkingText string) error {
	m.editThinkingCalls = append(m.editThinkingCalls, mockEditThinkingCall{msgID, formatted, thinkingText})
	return m.editThinkErr
}

func (m *mockBackend) BuildThinkingCombined(responseFormatted, thinkingText string) string {
	m.combinedCalls = append(m.combinedCalls, mockCombinedCall{responseFormatted, thinkingText})
	return "[thinking]" + thinkingText + "[/thinking]" + responseFormatted
}

func (m *mockBackend) FormatStreamPreview(preview string) string {
	m.previewCalls = append(m.previewCalls, preview)
	return "[preview]" + preview
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

// newTestSW creates a non-live StreamWriter for tests (buffer-only, no sends).
func newTestSW() *StreamWriter {
	return NewStreamWriter(&mockTransport{}, 50*time.Millisecond, 4000, false)
}

func newTestRenderer(backend *mockBackend, tracker *mockTracker, display TurnDisplay) *TurnRenderer {
	return NewTurnRenderer(backend, tracker, display, newTestSW)
}

// liveSWFactory returns a factory that creates live StreamWriters backed by the
// given mockTransport, for tests that need streaming deltas.
func liveSWFactory(transport *mockTransport, maxChars int) func() *StreamWriter {
	return func() *StreamWriter {
		return NewStreamWriter(transport, 50*time.Millisecond, maxChars, true)
	}
}

func TestOnReply_StreamEnabled_NoDeltasArrived_StillDelivers(t *testing.T) {
	// BUG FIX TEST: when streaming is enabled but no deltas arrived (e.g. model
	// returned text without streaming), the text must still be delivered.
	// Previously it was dropped by "else if !streamOutput".
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true, MaxChars: 4096}
	r := newTestRenderer(backend, tracker, display)

	r.OnReply("important text")

	if len(backend.sendReplyCalls) != 1 {
		t.Fatalf("sendReply calls = %d, want 1", len(backend.sendReplyCalls))
	}
	if backend.sendReplyCalls[0] != "important text" {
		t.Errorf("sendReply text = %q, want %q", backend.sendReplyCalls[0], "important text")
	}
}

func TestOnReply_StreamEnabled_DeltasArrived_FinalizesStream(t *testing.T) {
	// When streaming is active and deltas arrived, OnReply should finalize
	// the stream message (edit in-place) and not send a new message.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true, MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))

	// Simulate streaming deltas arriving before the reply.
	r.sw.OnDelta("streamed content")

	r.OnReply("reply text")

	// Should edit the stream message, not send a new one.
	if len(backend.editCalls) != 1 {
		t.Fatalf("editMessage calls = %d, want 1", len(backend.editCalls))
	}
	if backend.editCalls[0].msgID != "100" {
		t.Errorf("edit msgID = %q, want %q", backend.editCalls[0].msgID, "100")
	}
	if len(backend.sendReplyCalls) != 0 {
		t.Errorf("sendReply calls = %d, want 0 (should edit, not send)", len(backend.sendReplyCalls))
	}
	if tracker.cleanupCount != 1 {
		t.Errorf("cleanupPreview calls = %d, want 1", tracker.cleanupCount)
	}
}

func TestOnReply_StreamEnabled_StripsThinkingFromContent(t *testing.T) {
	// When thinking was streamed into the buffer, OnReply must strip thinking
	// from the stream content so only the text portion is finalized. Without
	// this, thinking would be permanently visible in intermediate messages.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true, ShowThinking: "compact", MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))

	r.OnThinking("internal reasoning")
	r.OnTextDelta("visible reply")
	r.OnReply("visible reply")

	// Stream content is "internal reasoning\n\n---\n\nvisible reply" but
	// the edit should only contain the text portion.
	if len(backend.editCalls) != 1 {
		t.Fatalf("editMessage calls = %d, want 1", len(backend.editCalls))
	}
	if !strings.Contains(backend.editCalls[0].formatted, "visible reply") {
		t.Errorf("edit should contain reply text, got %q", backend.editCalls[0].formatted)
	}
	if strings.Contains(backend.editCalls[0].formatted, "internal reasoning") {
		t.Errorf("edit should NOT contain thinking text, got %q", backend.editCalls[0].formatted)
	}
}

func TestOnReply_StreamEnabled_ThinkingOnly_NoEdit(t *testing.T) {
	// When only thinking was streamed (no text deltas), OnReply should not
	// edit with thinking content — there's no text to show.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true, ShowThinking: "compact", MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))

	r.OnThinking("just thinking")
	r.OnReply("")

	// No text content after stripping thinking — should not edit.
	if len(backend.editCalls) != 0 {
		t.Errorf("editMessage calls = %d, want 0 (no text to show)", len(backend.editCalls))
	}
}

func TestOnReply_StreamEnabled_DeltasArrived_FreshWriterForNextSegment(t *testing.T) {
	// After OnReply finalizes the stream message, a fresh stream writer
	// is created for the next segment. The next OnReply should take the
	// no-stream path.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true, MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))

	r.sw.OnDelta("first segment")
	r.OnReply("reply 1") // finalizes stream msg "100"

	// Second OnReply — no deltas on the fresh writer.
	r.OnReply("reply 2")

	// First reply: 1 edit (stream finalization). Second reply: 1 sendReply.
	if len(backend.editCalls) != 1 {
		t.Errorf("editMessage calls = %d, want 1", len(backend.editCalls))
	}
	if len(backend.sendReplyCalls) != 1 {
		t.Fatalf("sendReply calls = %d, want 1", len(backend.sendReplyCalls))
	}
	if backend.sendReplyCalls[0] != "reply 2" {
		t.Errorf("sendReply text = %q, want %q", backend.sendReplyCalls[0], "reply 2")
	}
}

func TestOnReply_NoStream_EditsToolPreview(t *testing.T) {
	// When not streaming and tool call preview exists, OnReply should edit
	// the preview in-place.
	backend := newMockBackend()
	tracker := &mockTracker{lastID: "50"}
	display := TurnDisplay{ShowToolCalls: "preview", MaxChars: 4096}
	r := newTestRenderer(backend, tracker, display)

	r.OnReply("reply text")

	if len(backend.editCalls) != 1 {
		t.Fatalf("editMessage calls = %d, want 1", len(backend.editCalls))
	}
	if backend.editCalls[0].msgID != "50" {
		t.Errorf("edit msgID = %q, want %q", backend.editCalls[0].msgID, "50")
	}
	if len(backend.sendReplyCalls) != 0 {
		t.Errorf("sendReply calls = %d, want 0 (should edit preview)", len(backend.sendReplyCalls))
	}
}

func TestOnReply_NoStream_NoPreview_SendsReply(t *testing.T) {
	// When not streaming and no tool call preview, OnReply sends a new message.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{MaxChars: 4096}
	r := newTestRenderer(backend, tracker, display)

	r.OnReply("reply text")

	if len(backend.sendReplyCalls) != 1 {
		t.Fatalf("sendReply calls = %d, want 1", len(backend.sendReplyCalls))
	}
	if backend.sendReplyCalls[0] != "reply text" {
		t.Errorf("sendReply text = %q, want %q", backend.sendReplyCalls[0], "reply text")
	}
	if !tracker.resetCalled {
		t.Error("expected ResetMsgID to be called")
	}
}

func TestOnReply_ToolPreviewTooLong_CleansUpAndSends(t *testing.T) {
	// When reply text exceeds MaxChars, the tool preview is cleaned up and
	// the reply is sent as a new message.
	backend := newMockBackend()
	tracker := &mockTracker{lastID: "50"}
	display := TurnDisplay{ShowToolCalls: "preview", MaxChars: 10}
	r := newTestRenderer(backend, tracker, display)

	r.OnReply("this text exceeds maxchars")

	if tracker.cleanupCount != 1 {
		t.Errorf("cleanupPreview calls = %d, want 1", tracker.cleanupCount)
	}
	if len(backend.sendReplyCalls) != 1 {
		t.Errorf("sendReply calls = %d, want 1 (fallback after cleanup)", len(backend.sendReplyCalls))
	}
}

func TestFinalize_EmptyResponse(t *testing.T) {
	// Verifies that empty responses produce no output.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{MaxChars: 4096}
	r := newTestRenderer(backend, tracker, display)

	r.Finalize("")

	total := len(backend.sendReplyCalls) + len(backend.editCalls) + len(backend.sendChunkedCalls)
	if total != 0 {
		t.Errorf("expected no sends or edits for empty response, got %d", total)
	}
}

func TestFinalize_StreamShort_NoThinking(t *testing.T) {
	// When stream message exists and response fits, edit in-place with no thinking.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true, MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))
	r.sw.OnDelta("stream content")

	r.Finalize("final response")

	if len(backend.editCalls) != 1 {
		t.Fatalf("editCalls = %d, want 1", len(backend.editCalls))
	}
	if backend.editCalls[0].msgID != "100" {
		t.Errorf("edit msgID = %q, want %q", backend.editCalls[0].msgID, "100")
	}
	if !strings.Contains(backend.editCalls[0].formatted, "final response") {
		t.Errorf("edit text should contain response, got %q", backend.editCalls[0].formatted)
	}
	if tracker.cleanupCount != 1 {
		t.Errorf("cleanupPreview calls = %d, want 1", tracker.cleanupCount)
	}
}

func TestFinalize_StreamShort_CompactThinking(t *testing.T) {
	// Stream short path with compact thinking — should use EditWithThinkingButton.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true, ShowThinking: "compact", MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))
	r.sw.OnDelta("stream content")
	r.OnThinking("deep thoughts")

	r.Finalize("final response")

	if len(backend.editThinkingCalls) != 1 {
		t.Fatalf("editThinkingButton calls = %d, want 1", len(backend.editThinkingCalls))
	}
	if backend.editThinkingCalls[0].msgID != "100" {
		t.Errorf("edit msgID = %q, want %q", backend.editThinkingCalls[0].msgID, "100")
	}
	if backend.editThinkingCalls[0].thinkingText != "deep thoughts" {
		t.Errorf("thinkingText = %q, want %q", backend.editThinkingCalls[0].thinkingText, "deep thoughts")
	}
}

func TestFinalize_StreamShort_FullThinking(t *testing.T) {
	// Stream short path with full thinking — uses BuildThinkingCombined + EditMessage.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true, ShowThinking: "true", MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))
	r.sw.OnDelta("stream content")
	r.OnThinking("deep thoughts")

	r.Finalize("final response")

	if len(backend.combinedCalls) != 1 {
		t.Fatalf("buildCombined calls = %d, want 1", len(backend.combinedCalls))
	}
	if len(backend.editCalls) != 1 {
		t.Fatalf("editMessage calls = %d, want 1", len(backend.editCalls))
	}
	if backend.editCalls[0].msgID != "100" {
		t.Errorf("edit msgID = %q, want %q", backend.editCalls[0].msgID, "100")
	}
}

func TestFinalize_StreamLong_SendsNewAndPreview(t *testing.T) {
	// When stream response exceeds MaxChars, sends new message + edits stream to preview.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true, MaxChars: 100}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 90))
	r.sw.OnDelta("x")

	longResponse := strings.Repeat("x", 200)
	r.Finalize(longResponse)

	// Should send new message (via sendReply) and edit stream to preview.
	if len(backend.sendReplyCalls) != 1 {
		t.Fatalf("sendReply calls = %d, want 1", len(backend.sendReplyCalls))
	}
	if len(backend.editCalls) != 1 {
		t.Fatalf("editMessage calls = %d, want 1 (stream preview)", len(backend.editCalls))
	}
	if backend.editCalls[0].msgID != "100" {
		t.Errorf("preview edit msgID = %q, want %q", backend.editCalls[0].msgID, "100")
	}
	if tracker.cleanupCount != 1 {
		t.Errorf("cleanupPreview calls = %d, want 1", tracker.cleanupCount)
	}
}

func TestFinalize_NoStream_ToolPreview(t *testing.T) {
	// Without streaming, edits tool call preview in-place when possible.
	backend := newMockBackend()
	tracker := &mockTracker{lastID: "50"}
	display := TurnDisplay{ShowToolCalls: "preview", MaxChars: 4096}
	r := newTestRenderer(backend, tracker, display)

	r.Finalize("short response")

	if len(backend.editCalls) != 1 {
		t.Fatalf("editCalls = %d, want 1", len(backend.editCalls))
	}
	if backend.editCalls[0].msgID != "50" {
		t.Errorf("edit msgID = %q, want %q", backend.editCalls[0].msgID, "50")
	}
	if len(backend.sendReplyCalls) != 0 {
		t.Errorf("sendReply calls = %d, want 0", len(backend.sendReplyCalls))
	}
}

func TestFinalize_NoStream_NewMessage(t *testing.T) {
	// Without streaming and no preview, sends a new message.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{MaxChars: 4096}
	r := newTestRenderer(backend, tracker, display)

	r.Finalize("response text")

	if len(backend.sendReplyCalls) != 1 {
		t.Fatalf("sendReply calls = %d, want 1", len(backend.sendReplyCalls))
	}
	if backend.sendReplyCalls[0] != "response text" {
		t.Errorf("sendReply text = %q, want %q", backend.sendReplyCalls[0], "response text")
	}
	if tracker.cleanupCount != 1 {
		t.Errorf("cleanupPreview calls = %d, want 1", tracker.cleanupCount)
	}
}

func TestFinalize_StreamContentFallback(t *testing.T) {
	// When response is empty but stream has content, uses stream content.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{StreamOutput: true, MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))
	r.sw.OnDelta("stream fallback content")

	r.Finalize("")

	// Should use stream content and edit the stream message.
	if len(backend.editCalls) != 1 {
		t.Fatalf("editCalls = %d, want 1", len(backend.editCalls))
	}
	if !strings.Contains(backend.editCalls[0].formatted, "stream fallback content") {
		t.Errorf("edit should contain stream content, got %q", backend.editCalls[0].formatted)
	}
}

func TestFinalize_NoStream_FullThinking_SendsCombined(t *testing.T) {
	// Without streaming and full thinking mode, sends combined thinking+response.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "true", MaxChars: 4096}
	r := newTestRenderer(backend, tracker, display)
	r.OnThinking("my thoughts")

	r.Finalize("response")

	if len(backend.combinedCalls) != 1 {
		t.Fatalf("buildCombined calls = %d, want 1", len(backend.combinedCalls))
	}
	if len(backend.sendChunkedCalls) != 1 {
		t.Fatalf("sendChunked calls = %d, want 1", len(backend.sendChunkedCalls))
	}
	if !strings.Contains(backend.sendChunkedCalls[0], "my thoughts") {
		t.Errorf("chunked text should contain thinking, got %q", backend.sendChunkedCalls[0])
	}
}

func TestFinalize_NoStream_CompactThinking_SendsWithButton(t *testing.T) {
	// Without streaming and compact thinking mode, sends with thinking button.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", MaxChars: 4096}
	r := newTestRenderer(backend, tracker, display)
	r.OnThinking("my thoughts")

	r.Finalize("response")

	if len(backend.thinkingBtnCalls) != 1 {
		t.Fatalf("sendWithThinkingButton calls = %d, want 1", len(backend.thinkingBtnCalls))
	}
	if backend.thinkingBtnCalls[0].thinkingText != "my thoughts" {
		t.Errorf("thinkingText = %q, want %q", backend.thinkingBtnCalls[0].thinkingText, "my thoughts")
	}
}

func TestOnThinking_Accumulates(t *testing.T) {
	// Verifies that thinking blocks are accumulated with newline separators.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true, MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))
	r.sw.OnDelta("x")

	r.OnThinking("thought 1")
	r.OnThinking("thought 2")

	r.Finalize("response")

	if len(backend.editThinkingCalls) != 1 {
		t.Fatalf("editThinkingButton calls = %d, want 1", len(backend.editThinkingCalls))
	}
	if backend.editThinkingCalls[0].thinkingText != "thought 1\nthought 2" {
		t.Errorf("thinkingText = %q, want %q", backend.editThinkingCalls[0].thinkingText, "thought 1\nthought 2")
	}
}

func TestOnThinking_Off_Ignored(t *testing.T) {
	// Verifies that thinking blocks are ignored when mode is "off".
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "off", MaxChars: 4096}
	r := newTestRenderer(backend, tracker, display)

	r.OnThinking("should be ignored")
	r.Finalize("response")

	// Default mode (no thinking): should send reply, not thinking button.
	if len(backend.sendReplyCalls) != 1 {
		t.Fatalf("sendReply calls = %d, want 1", len(backend.sendReplyCalls))
	}
	if len(backend.editThinkingCalls) != 0 {
		t.Errorf("editThinkingButton calls = %d, want 0", len(backend.editThinkingCalls))
	}
}

func TestOnThinking_Compact_StreamsLive(t *testing.T) {
	// When compact + streaming, thinking deltas are fed into the StreamWriter
	// so users see thinking content in real-time. Typing indicator is refreshed.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true, MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))

	r.OnThinking("analyzing the problem")

	// Thinking should be in the stream writer's buffer.
	content := r.sw.Content()
	if content != "analyzing the problem" {
		t.Errorf("stream content = %q, want %q", content, "analyzing the problem")
	}
	// Typing indicator should have been sent.
	if backend.typingCount != 1 {
		t.Errorf("typing count = %d, want 1", backend.typingCount)
	}
}

func TestOnThinking_True_StreamsLive(t *testing.T) {
	// When true + streaming, thinking deltas are also fed into the StreamWriter
	// for live visibility. At finalization, BuildThinkingCombined reformats.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "true", StreamOutput: true, MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))

	r.OnThinking("deep analysis")

	content := r.sw.Content()
	if content != "deep analysis" {
		t.Errorf("stream content = %q, want %q", content, "deep analysis")
	}
	if backend.typingCount != 1 {
		t.Errorf("typing count = %d, want 1", backend.typingCount)
	}
}

func TestOnThinking_Off_NoStream(t *testing.T) {
	// When mode is "off", thinking is not streamed even with streaming enabled.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "off", StreamOutput: true, MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))

	r.OnThinking("should not appear")

	content := r.sw.Content()
	if content != "" {
		t.Errorf("stream content = %q, want empty", content)
	}
}

func TestOnTextDelta_InsertsDividerAfterThinking(t *testing.T) {
	// First text delta after thinking inserts a divider in the stream,
	// visually separating thinking from response content.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true, MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))

	r.OnThinking("let me think")
	r.OnTextDelta("Hello")
	r.OnTextDelta(" world")

	content := r.sw.Content()
	want := "let me think\n\n---\n\nHello world"
	if content != want {
		t.Errorf("stream content = %q, want %q", content, want)
	}
}

func TestOnTextDelta_NoDividerWithoutThinking(t *testing.T) {
	// When no thinking preceded text deltas, no divider is inserted.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true, MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))

	r.OnTextDelta("Hello")
	r.OnTextDelta(" world")

	content := r.sw.Content()
	if content != "Hello world" {
		t.Errorf("stream content = %q, want %q", content, "Hello world")
	}
}

func TestFinalize_StreamContentFallback_StripsThinking(t *testing.T) {
	// When response is empty and thinking was streamed into the buffer,
	// the Content() fallback strips thinking + divider to extract text only.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true, MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))

	r.OnThinking("reasoning here")
	r.OnTextDelta("actual response")
	r.Finalize("") // empty response triggers Content() fallback

	// Should extract text after divider and use EditWithThinkingButton.
	if len(backend.editThinkingCalls) != 1 {
		t.Fatalf("editThinkingButton calls = %d, want 1", len(backend.editThinkingCalls))
	}
	if !strings.Contains(backend.editThinkingCalls[0].formatted, "actual response") {
		t.Errorf("formatted = %q, should contain 'actual response'", backend.editThinkingCalls[0].formatted)
	}
	if backend.editThinkingCalls[0].thinkingText != "reasoning here" {
		t.Errorf("thinkingText = %q, want %q", backend.editThinkingCalls[0].thinkingText, "reasoning here")
	}
}

func TestFinalize_StreamContentFallback_ThinkingOnly(t *testing.T) {
	// When response is empty, thinking was streamed but no text deltas arrived,
	// the response should remain empty (no divider → nothing to extract).
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true, MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))

	r.OnThinking("reasoning only")
	r.Finalize("") // no text deltas, no response

	// Should not send anything — only thinking, no actual response.
	total := len(backend.sendReplyCalls) + len(backend.editCalls) + len(backend.editThinkingCalls)
	if total != 0 {
		t.Errorf("expected no sends/edits for thinking-only response, got %d", total)
	}
}

func TestOnReply_ResetsThinkingPhase(t *testing.T) {
	// OnReply resets thinkingPhase so the next segment doesn't inherit
	// stale thinking state.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true, MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))

	r.OnThinking("segment 1 thinking")
	r.OnReply("reply 1") // resets phase

	// After OnReply, a new text delta should NOT insert a divider.
	r.OnTextDelta("segment 2 text")
	content := r.sw.Content()
	if strings.Contains(content, "---") {
		t.Errorf("stream content should not contain divider after phase reset, got %q", content)
	}
}

func TestFinalize_CompactStream_FullFlow(t *testing.T) {
	// End-to-end test: compact + streaming → thinking streams live with divider,
	// finalization collapses to response + button.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true, MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))

	// Simulate realistic streaming order: thinking first, then text.
	r.OnThinking("I need to consider ")
	r.OnThinking("this carefully")
	r.OnTextDelta("Here is ")
	r.OnTextDelta("the answer.")

	r.Finalize("Here is the answer.")

	// Stream content during streaming should have been: thinking + divider + text.
	// At finalization, the message is edited to show only the response + button.
	if len(backend.editThinkingCalls) != 1 {
		t.Fatalf("editThinkingButton calls = %d, want 1", len(backend.editThinkingCalls))
	}
	if !strings.Contains(backend.editThinkingCalls[0].formatted, "Here is the answer.") {
		t.Errorf("formatted = %q, should contain response", backend.editThinkingCalls[0].formatted)
	}
	if backend.editThinkingCalls[0].thinkingText != "I need to consider \nthis carefully" {
		t.Errorf("thinkingText = %q, want accumulated thinking", backend.editThinkingCalls[0].thinkingText)
	}
}

func TestFinalize_TrueStream_FullFlow(t *testing.T) {
	// End-to-end test: true + streaming → thinking streams live, finalization
	// reformats with BuildThinkingCombined.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{ShowThinking: "true", StreamOutput: true, MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	r := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))

	r.OnThinking("deep thoughts")
	r.OnTextDelta("The answer is 42.")

	r.Finalize("The answer is 42.")

	// Finalization should use BuildThinkingCombined + EditMessage.
	if len(backend.combinedCalls) != 1 {
		t.Fatalf("buildCombined calls = %d, want 1", len(backend.combinedCalls))
	}
	if len(backend.editCalls) != 1 {
		t.Fatalf("editMessage calls = %d, want 1", len(backend.editCalls))
	}
	if backend.combinedCalls[0].thinkingText != "deep thoughts" {
		t.Errorf("thinkingText = %q, want %q", backend.combinedCalls[0].thinkingText, "deep thoughts")
	}
}

func TestOnTextDelta_SendsTyping(t *testing.T) {
	// Verifies that OnTextDelta refreshes the typing indicator.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{MaxChars: 4096}
	r := newTestRenderer(backend, tracker, display)

	r.OnTextDelta("hello")

	if backend.typingCount != 1 {
		t.Errorf("typing count = %d, want 1", backend.typingCount)
	}
}

func TestOnActivity_SendsTyping(t *testing.T) {
	// Verifies that OnActivity refreshes the typing indicator.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{MaxChars: 4096}
	r := newTestRenderer(backend, tracker, display)

	r.OnActivity()

	if backend.typingCount != 1 {
		t.Errorf("typing count = %d, want 1", backend.typingCount)
	}
}

func TestCleanup_Idempotent(t *testing.T) {
	// Verifies Cleanup is safe to call after Finalize.
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{MaxChars: 4096}
	r := newTestRenderer(backend, tracker, display)

	r.Finalize("response")
	r.Cleanup() // should not panic
}

// TestFinalize_NoResponseSentinelSilenced verifies that [[NO_RESPONSE]] and
// other silent sentinels are caught by Finalize before reaching the platform.
// This is the end-to-end test for the bug where delegated agents returned the
// sentinel as FinalText and Finalize delivered it to the user.
func TestFinalize_NoResponseSentinelSilenced(t *testing.T) {
	for _, text := range []string{"[[NO_RESPONSE]]", "No response requested.", "  [[NO_RESPONSE]]  ", ""} {
		t.Run(text, func(t *testing.T) {
			backend := newMockBackend()
			tracker := &mockTracker{}
			display := TurnDisplay{MaxChars: 4096}
			r := newTestRenderer(backend, tracker, display)

			r.Finalize(text)

			total := len(backend.sendReplyCalls) + len(backend.editCalls) + len(backend.sendChunkedCalls)
			if total != 0 {
				t.Errorf("Finalize(%q): expected no sends, got %d", text, total)
			}
		})
	}
}

// TestFinalize_RealTextNotSilenced verifies that non-sentinel text still
// gets delivered normally through Finalize.
func TestFinalize_RealTextNotSilenced(t *testing.T) {
	backend := newMockBackend()
	tracker := &mockTracker{}
	display := TurnDisplay{MaxChars: 4096}
	r := newTestRenderer(backend, tracker, display)

	r.Finalize("Here is my answer.")

	total := len(backend.sendReplyCalls) + len(backend.editCalls) + len(backend.sendChunkedCalls)
	if total == 0 {
		t.Error("Finalize with real text should produce output")
	}
}
