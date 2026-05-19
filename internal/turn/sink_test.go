package turn

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"foci/internal/agent/turnevent"
)

// fakeSinkTracker is a minimal SinkTracker for sink tests. It records which
// observer calls fired so assertions can verify routing without constructing
// a real *ToolCallTracker (which would need a full TrackerBackend mock).
type fakeSinkTracker struct {
	mockTracker // embeds the renderer-test mock for narrow ToolTracker methods

	toolCalls   []string
	toolResults []string
	retries     []string
	retryClears int
}

func (f *fakeSinkTracker) ObserveToolCall(_, toolName string, _ json.RawMessage) {
	f.toolCalls = append(f.toolCalls, toolName)
}

func (f *fakeSinkTracker) ObserveToolResult(_, toolName, _ string, _ bool) {
	f.toolResults = append(f.toolResults, toolName)
}

func (f *fakeSinkTracker) NotifyRetry(endpoint string) {
	f.retries = append(f.retries, endpoint)
}

func (f *fakeSinkTracker) ClearRetryNotification() {
	f.retryClears++
}

// fakeTypingConn is a no-op platform.Connection stand-in that only records
// SetTyping calls. StreamingSink only touches SetTyping, so every other method
// can panic if called — they shouldn't be during these tests.
type fakeTypingConn struct {
	typingCalls []bool
}

func (f *fakeTypingConn) SetTyping(on bool) { f.typingCalls = append(f.typingCalls, on) }

// Everything else panics — if StreamingSink ever reaches for these we want
// the test to fail loudly so we know the sink grew a new coupling.
func (f *fakeTypingConn) SendText(string) error                 { panic("SendText") }
func (f *fakeTypingConn) SendTextToChat(int64, string) error    { panic("SendTextToChat") }
func (f *fakeTypingConn) SessionKey() string                    { panic("SessionKey") }
func (f *fakeTypingConn) SendDocument(string, string) error     { panic("SendDocument") }
func (f *fakeTypingConn) SendVoice(string) error                { panic("SendVoice") }
func (f *fakeTypingConn) SendVideo(string, string) error        { panic("SendVideo") }
func (f *fakeTypingConn) SendPhoto(string, string) error        { panic("SendPhoto") }
func (f *fakeTypingConn) SendAudio(string, string) error        { panic("SendAudio") }
func (f *fakeTypingConn) SendAnimation(string, string) error    { panic("SendAnimation") }
func (f *fakeTypingConn) SendVoiceData([]byte) error            { panic("SendVoiceData") }
func (f *fakeTypingConn) SendDocumentToChat(int64, string, string) error {
	panic("SendDocumentToChat")
}
func (f *fakeTypingConn) SendVoiceToChat(int64, string) error             { panic("SendVoiceToChat") }
func (f *fakeTypingConn) SendVideoToChat(int64, string, string) error     { panic("SendVideoToChat") }
func (f *fakeTypingConn) SendPhotoToChat(int64, string, string) error     { panic("SendPhotoToChat") }
func (f *fakeTypingConn) SendAudioToChat(int64, string, string) error     { panic("SendAudioToChat") }
func (f *fakeTypingConn) SendAnimationToChat(int64, string, string) error { panic("SendAnimationToChat") }
func (f *fakeTypingConn) SendVoiceDataToChat(int64, []byte) error { panic("SendVoiceDataToChat") }
func (f *fakeTypingConn) PlatformName() string                    { panic("PlatformName") }
func (f *fakeTypingConn) SessionKeyForChat(int64) string          { panic("SessionKeyForChat") }
func (f *fakeTypingConn) DefaultSessionKey() string               { panic("DefaultSessionKey") }
func (f *fakeTypingConn) SetSessionKey(string)                    { panic("SetSessionKey") }
func (f *fakeTypingConn) SetSessionKeyDirect(string)              { panic("SetSessionKeyDirect") }
func (f *fakeTypingConn) SetChatID(int64)                         { panic("SetChatID") }
func (f *fakeTypingConn) ChatID() int64                           { panic("ChatID") }
func (f *fakeTypingConn) Username() string                        { panic("Username") }
func (f *fakeTypingConn) UpdateChatSessionKey(int64, string)      { panic("UpdateChatSessionKey") }
func (f *fakeTypingConn) SendInjectedMessage(string, string) error {
	panic("SendInjectedMessage")
}
func (f *fakeTypingConn) SendToSession(string, string) error     { panic("SendToSession") }
func (f *fakeTypingConn) SendNotification(string)                { panic("SendNotification") }
func (f *fakeTypingConn) SendNotificationDirect(string) string   { panic("SendNotificationDirect") }

// TestStreamingSinkTypingLifecycle asserts the sink drives the typing
// indicator entirely through events: TurnStart turns typing on, TurnComplete
// turns it off. This is load-bearing because it replaces the old out-of-band
// SetTyping(true)/defer SetTyping(false) in the worker.
func TestStreamingSinkTypingLifecycle(t *testing.T) {
	backend := newMockBackend()
	tracker := &fakeSinkTracker{}
	renderer := NewTurnRenderer(backend, tracker, TurnDisplay{MaxChars: 4096}, newTestSW)
	conn := &fakeTypingConn{}
	sink := NewStreamingSink(renderer, tracker, conn)

	ctx := context.Background()
	sink.Emit(ctx, turnevent.TurnStart{})
	sink.Emit(ctx, turnevent.TurnComplete{FinalText: "done"})

	if len(conn.typingCalls) != 2 {
		t.Fatalf("typing calls = %d, want 2 (on, off)", len(conn.typingCalls))
	}
	if conn.typingCalls[0] != true {
		t.Errorf("first typing call = %v, want true (TurnStart → on)", conn.typingCalls[0])
	}
	if conn.typingCalls[1] != false {
		t.Errorf("last typing call = %v, want false (TurnComplete → off)", conn.typingCalls[1])
	}
}

// TestStreamingSinkNilConnSkipsTyping asserts the sink tolerates a nil conn
// for tests or headless call sites where typing indicator control is
// irrelevant.
func TestStreamingSinkNilConnSkipsTyping(t *testing.T) {
	backend := newMockBackend()
	tracker := &fakeSinkTracker{}
	renderer := NewTurnRenderer(backend, tracker, TurnDisplay{MaxChars: 4096}, newTestSW)
	sink := NewStreamingSink(renderer, tracker, nil)

	// Must not panic.
	ctx := context.Background()
	sink.Emit(ctx, turnevent.TurnStart{})
	sink.Emit(ctx, turnevent.TurnComplete{FinalText: "ok"})
}

// TestStreamingSinkRoutesThinkingDelta asserts ThinkingDelta events reach
// the renderer's OnThinkingDelta path, so tests and downstream consumers
// can rely on per-token thinking streaming rather than the block fallback.
func TestStreamingSinkRoutesThinkingDelta(t *testing.T) {
	backend := newMockBackend()
	tracker := &fakeSinkTracker{}
	display := TurnDisplay{ShowThinking: "compact", StreamOutput: true, MaxChars: 4096}
	transport := &mockTransport{sendMsgID: "100"}
	renderer := NewTurnRenderer(backend, tracker, display, liveSWFactory(transport, 3900))
	sink := NewStreamingSink(renderer, tracker, nil)

	ctx := context.Background()
	sink.Emit(ctx, turnevent.ThinkingDelta{Delta: "I'm "})
	sink.Emit(ctx, turnevent.ThinkingDelta{Delta: "pondering"})
	sink.Emit(ctx, turnevent.ThinkingBlock{Text: "I'm pondering"})

	content := renderer.sw.Content()
	if strings.Count(content, "I'm ") != 1 || strings.Count(content, "pondering") != 1 {
		t.Errorf("stream content duplicated after delta+block; got %q", content)
	}
	if renderer.thinking.String() != "I'm pondering" {
		t.Errorf("thinking builder = %q, want %q", renderer.thinking.String(), "I'm pondering")
	}
}

// TestStreamingSinkRoutesToolEvents asserts ToolCall/ToolResult/RetryNotice/
// RetrySuccess events reach the tracker's observer methods — this is the
// refactor's promise that tool visibility flows through the sink and not
// through a parallel BuildTurnObservers wiring.
func TestStreamingSinkRoutesToolEvents(t *testing.T) {
	backend := newMockBackend()
	tracker := &fakeSinkTracker{}
	renderer := NewTurnRenderer(backend, tracker, TurnDisplay{MaxChars: 4096}, newTestSW)
	sink := NewStreamingSink(renderer, tracker, nil)

	ctx := context.Background()
	sink.Emit(ctx, turnevent.ToolCall{Name: "shell", ID: "t1", Args: json.RawMessage(`{}`)})
	sink.Emit(ctx, turnevent.ToolResult{Name: "shell", ID: "t1", Output: "ok"})
	sink.Emit(ctx, turnevent.RetryNotice{Endpoint: "anthropic"})
	sink.Emit(ctx, turnevent.RetrySuccess{})

	if len(tracker.toolCalls) != 1 || tracker.toolCalls[0] != "shell" {
		t.Errorf("toolCalls = %v, want [shell]", tracker.toolCalls)
	}
	if len(tracker.toolResults) != 1 || tracker.toolResults[0] != "shell" {
		t.Errorf("toolResults = %v, want [shell]", tracker.toolResults)
	}
	if len(tracker.retries) != 1 || tracker.retries[0] != "anthropic" {
		t.Errorf("retries = %v, want [anthropic]", tracker.retries)
	}
	if tracker.retryClears != 1 {
		t.Errorf("retryClears = %d, want 1", tracker.retryClears)
	}
}

// TestStreamingSinkDeliveredFlagSuppressesFinalize asserts that an intermediate
// TextBlock marks the sink as delivered, so the terminal TurnComplete skips
// re-delivery through Finalize. This is the contract that replaces
// replyDelivered-on-renderer.
func TestStreamingSinkDeliveredFlagSuppressesFinalize(t *testing.T) {
	backend := newMockBackend()
	tracker := &fakeSinkTracker{}
	renderer := NewTurnRenderer(backend, tracker, TurnDisplay{MaxChars: 4096}, newTestSW)
	sink := NewStreamingSink(renderer, tracker, nil)

	ctx := context.Background()
	sink.Emit(ctx, turnevent.TextBlock{Text: "intermediate", Phase: turnevent.PhaseIntermediate})
	sink.Emit(ctx, turnevent.TurnComplete{FinalText: "final"})

	// OnReply delivered the intermediate text.
	if len(backend.sendReplyCalls) != 1 || backend.sendReplyCalls[0] != "intermediate" {
		t.Errorf("sendReply = %v, want [intermediate]", backend.sendReplyCalls)
	}
	// Finalize was NOT called — the final text must not reach the backend a
	// second time.
	for _, c := range backend.sendReplyCalls {
		if c == "final" {
			t.Error("final text re-delivered after intermediate; delivered flag not respected")
		}
	}
	// Tracker cleanup fired instead.
	if tracker.cleanupCount == 0 {
		t.Error("CleanupPreview not called on delivered TurnComplete")
	}
}

// TestStreamingSinkSilentIntermediateDoesNotSuppressFinalize asserts that an
// intermediate TextBlock containing a silencing sentinel ([[NO_RESPONSE]])
// must NOT mark the sink as delivered. The agent never surfaced anything to
// the user, so a subsequent non-silent FinalText on TurnComplete must still
// reach the backend via Finalize.
//
// Regression for the 2026-05-18 22:33 delivery gap: a turn emitted
// [[NO_RESPONSE]] as an intermediate TextBlock, then a real 1024-byte reply
// as FinalText. The unconditional `s.delivered = true` swallowed the real
// reply. See docs/delivery-gap-2026-05-18-2233.md.
func TestStreamingSinkSilentIntermediateDoesNotSuppressFinalize(t *testing.T) {
	backend := newMockBackend()
	tracker := &fakeSinkTracker{}
	renderer := NewTurnRenderer(backend, tracker, TurnDisplay{MaxChars: 4096}, newTestSW)
	sink := NewStreamingSink(renderer, tracker, nil)

	ctx := context.Background()
	// Silent intermediate — OnReply's IsSilent gate drops the text without
	// delivering. The sink must NOT flip delivered=true because nothing
	// reached the user.
	sink.Emit(ctx, turnevent.TextBlock{Text: "[[NO_RESPONSE]]", Phase: turnevent.PhaseIntermediate})
	// Real final text arrives — must reach the backend via Finalize.
	sink.Emit(ctx, turnevent.TurnComplete{FinalText: "the real reply"})

	if len(backend.sendReplyCalls) != 1 || backend.sendReplyCalls[0] != "the real reply" {
		t.Errorf("sendReply = %v, want [\"the real reply\"]; silent intermediate must not block final delivery",
			backend.sendReplyCalls)
	}
}

// TestSessionSinkSilentIntermediateAllowsFinalText asserts that a silent
// intermediate TextBlock ([[NO_RESPONSE]]) does NOT set the SessionSink
// delivered flag, so a subsequent non-silent FinalText on TurnComplete
// reaches the user via SendToSession. Mirror of
// TestStreamingSinkSilentIntermediateDoesNotSuppressFinalize for the
// session-router fallback path. SessionSink already gates correctly; this
// test pins the behaviour so future refactors don't regress it.
func TestSessionSinkSilentIntermediateAllowsFinalText(t *testing.T) {
	conn := &fakeSessionConn{}
	sink := NewSessionSink(conn, "sess-1", "test")

	ctx := context.Background()
	sink.Emit(ctx, turnevent.TextBlock{Text: "[[NO_RESPONSE]]", Phase: turnevent.PhaseIntermediate})
	sink.Emit(ctx, turnevent.TurnComplete{FinalText: "the real reply"})

	if len(conn.sendCalls) != 1 || conn.sendCalls[0] != "the real reply" {
		t.Errorf("sendCalls = %v, want [\"the real reply\"]; silent intermediate must not block final delivery",
			conn.sendCalls)
	}
}

// TestStreamingSinkUnDeliveredCallsFinalize asserts the opposite: when no
// intermediate delivery happened, TurnComplete drives renderer.Finalize with
// FinalText. This is the path used by turns with no streaming/OnReply output.
func TestStreamingSinkUnDeliveredCallsFinalize(t *testing.T) {
	backend := newMockBackend()
	tracker := &fakeSinkTracker{}
	renderer := NewTurnRenderer(backend, tracker, TurnDisplay{MaxChars: 4096}, newTestSW)
	sink := NewStreamingSink(renderer, tracker, nil)

	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "only final"})

	if len(backend.sendReplyCalls) != 1 || backend.sendReplyCalls[0] != "only final" {
		t.Errorf("sendReply = %v, want [only final]", backend.sendReplyCalls)
	}
}

// TestStreamingSinkErrorOverridesFinalText asserts that a non-cancellation
// error in TurnComplete builds the Error: ... message and drives Finalize,
// so the user sees the failure rather than silent completion.
func TestStreamingSinkErrorOverridesFinalText(t *testing.T) {
	backend := newMockBackend()
	tracker := &fakeSinkTracker{}
	renderer := NewTurnRenderer(backend, tracker, TurnDisplay{MaxChars: 4096}, newTestSW)
	sink := NewStreamingSink(renderer, tracker, nil)

	sink.Emit(context.Background(), turnevent.TurnComplete{
		FinalText: "partial",
		Err:       errors.New("upstream 500"),
	})

	if len(backend.sendReplyCalls) != 1 {
		t.Fatalf("sendReply calls = %d, want 1", len(backend.sendReplyCalls))
	}
	if backend.sendReplyCalls[0] != "Error: upstream 500" {
		t.Errorf("sendReply = %q, want %q", backend.sendReplyCalls[0], "Error: upstream 500")
	}
}

// TestStreamingSinkCancelledContextDropsError asserts that when ctx is
// cancelled (user pressed /stop), a TurnComplete carrying an error does not
// produce a visible "Error: ..." message — /stop already showed "Stopped.".
func TestStreamingSinkCancelledContextDropsError(t *testing.T) {
	backend := newMockBackend()
	tracker := &fakeSinkTracker{}
	renderer := NewTurnRenderer(backend, tracker, TurnDisplay{MaxChars: 4096}, newTestSW)
	sink := NewStreamingSink(renderer, tracker, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sink.Emit(ctx, turnevent.TurnComplete{FinalText: "", Err: errors.New("boom")})

	for _, c := range backend.sendReplyCalls {
		if c == "Error: boom" {
			t.Error("error rendered despite cancelled context")
		}
	}
}

// --- SessionSink tests ---

// fakeSessionConn records SendToSession / SetTyping calls made by SessionSink.
// Unlike fakeTypingConn, this panics on fewer methods — SessionSink touches
// SetTyping and SendToSession only.
type fakeSessionConn struct {
	fakeTypingConn

	sendCalls  []string
	sendErr    error
}

func (f *fakeSessionConn) SendToSession(_ string, text string) error {
	f.sendCalls = append(f.sendCalls, text)
	return f.sendErr
}

// TestSessionSinkDeliveredFlag asserts that an intermediate TextBlock marks
// the SessionSink delivered so the TurnComplete final text does not fire a
// second SendToSession — preventing the "double delivery" bug injected turns
// used to silently produce.
func TestSessionSinkDeliveredFlag(t *testing.T) {
	conn := &fakeSessionConn{}
	sink := NewSessionSink(conn, "sess-1", "test")

	ctx := context.Background()
	sink.Emit(ctx, turnevent.TextBlock{Text: "live update", Phase: turnevent.PhaseIntermediate})
	sink.Emit(ctx, turnevent.TurnComplete{FinalText: "the same live update"})

	if len(conn.sendCalls) != 1 || conn.sendCalls[0] != "live update" {
		t.Errorf("sendCalls = %v, want [live update]", conn.sendCalls)
	}
}

// TestSessionSinkFallsBackToFinalTextWhenSilent asserts that when no
// intermediate TextBlock arrived, the SessionSink delivers the TurnComplete
// text — this is the path non-streaming HTTP or injected turns use.
func TestSessionSinkFallsBackToFinalTextWhenSilent(t *testing.T) {
	conn := &fakeSessionConn{}
	sink := NewSessionSink(conn, "sess-1", "test")

	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "final only"})

	if len(conn.sendCalls) != 1 || conn.sendCalls[0] != "final only" {
		t.Errorf("sendCalls = %v, want [final only]", conn.sendCalls)
	}
}

// TestSessionSinkEmptyFinalTextSkipped asserts that a TurnComplete with empty
// FinalText and no prior delivery is a no-op — matches current agents_notify
// behaviour which skips empty responses.
func TestSessionSinkEmptyFinalTextSkipped(t *testing.T) {
	conn := &fakeSessionConn{}
	sink := NewSessionSink(conn, "sess-1", "test")

	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: ""})

	if len(conn.sendCalls) != 0 {
		t.Errorf("sendCalls = %v, want []", conn.sendCalls)
	}
}

// TestSessionSinkErrorHandlerInvoked asserts that a SendToSession error fires
// the configured error handler so callers can log it without tying sink
// internals to any particular logger.
func TestSessionSinkErrorHandlerInvoked(t *testing.T) {
	conn := &fakeSessionConn{sendErr: errors.New("network")}
	var captured error
	sink := NewSessionSink(conn, "sess-1", "test", WithSessionSinkErrorHandler(func(_ string, err error) {
		captured = err
	}))

	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "final"})

	if captured == nil || captured.Error() != "network" {
		t.Errorf("error handler got %v, want network", captured)
	}
}

// TestSinkDeliversToPlatform pins the DeliversToPlatform answer per
// production sink in this package. StreamingSink drives a renderer backed by
// a platform.Connection; SessionSink delivers via Connection.SendToSession.
// Both must report true so the sink-delivery gate (TODO #767) allows
// Telegram follow-ups to fold into in-flight turns that use them.
func TestSinkDeliversToPlatform(t *testing.T) {
	// StreamingSink with nil conn still reports true — nil-conn is a test
	// affordance, not a deliberate non-delivery contract.
	backend := newMockBackend()
	tracker := &fakeSinkTracker{}
	renderer := NewTurnRenderer(backend, tracker, TurnDisplay{MaxChars: 4096}, newTestSW)
	stream := NewStreamingSink(renderer, tracker, nil)
	if !stream.DeliversToPlatform() {
		t.Errorf("StreamingSink.DeliversToPlatform() = false, want true")
	}

	// SessionSink delivers to the user's chat via Connection.SendToSession.
	sess := NewSessionSink(&fakeSessionConn{}, "sess-1", "test")
	if !sess.DeliversToPlatform() {
		t.Errorf("SessionSink.DeliversToPlatform() = false, want true")
	}
}
