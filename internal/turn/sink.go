package turn

import (
	"context"
	"encoding/json"
	"fmt"

	"foci/internal/agent/turnevent"
	"foci/internal/platform"
)

// SinkTracker is the subset of ToolCallTracker methods StreamingSink drives.
// *ToolCallTracker satisfies this automatically; tests can implement it with
// a minimal fake to avoid constructing a full tracker.
type SinkTracker interface {
	ToolTracker
	ObserveToolCall(id, toolName string, params json.RawMessage)
	ObserveToolResult(id, toolName, result string, isError bool)
	NotifyRetry(endpoint string)
	ClearRetryNotification()
}

// StreamingSink is the shared platform sink used by interactive platforms
// (Telegram, Discord) to translate a turnevent.Event stream into renderer and
// tool-tracker calls. All per-turn platform wiring — text, thinking, tool
// calls, retries, typing indicator, cleanup — flows through this single sink.
//
// One StreamingSink per turn. Callers construct it inside processAgentMessage
// (or equivalent) with a fresh renderer, the platform's ToolCallTracker, and
// the platform Connection (for typing indicator control). The renderer's own
// Cleanup() must still be deferred by the caller.
type StreamingSink struct {
	renderer *TurnRenderer
	tracker  SinkTracker
	conn     platform.Connection

	// delivered is set when an intermediate TextBlock or streamed content has
	// already been shown to the user during the turn. On TurnComplete, a
	// delivered sink does cleanup only; an undelivered sink calls Finalize.
	delivered bool
}

// NewStreamingSink constructs a StreamingSink. renderer and tracker are
// required; conn may be nil in tests where typing-indicator side effects are
// irrelevant.
func NewStreamingSink(renderer *TurnRenderer, tracker SinkTracker, conn platform.Connection) *StreamingSink {
	return &StreamingSink{
		renderer: renderer,
		tracker:  tracker,
		conn:     conn,
	}
}

// DeliversToPlatform implements turnevent.Sink. StreamingSink drives a
// renderer backed by a platform.Connection (Telegram, Discord), so output is
// always user-facing — even when conn is nil for tests, the renderer remains
// the contractual delivery path. Returns true unconditionally.
func (s *StreamingSink) DeliversToPlatform() bool { return true }

// Emit implements turnevent.Sink.
func (s *StreamingSink) Emit(ctx context.Context, ev turnevent.Event) {
	switch e := ev.(type) {
	case turnevent.TurnStart:
		if s.conn != nil {
			s.conn.SetTyping(true)
		}

	case turnevent.TextDelta:
		s.renderer.OnTextDelta(e.Delta)

	case turnevent.TextBlock:
		// PhaseFinal is carried by TurnComplete.FinalText; only intermediate
		// blocks (tool-loop replies, mid-turn delegated text) are delivered
		// incrementally here.
		if e.Phase == turnevent.PhaseIntermediate {
			s.renderer.OnReply(e.Text)
			s.delivered = true
		}

	case turnevent.ThinkingDelta:
		s.renderer.OnThinkingDelta(e.Delta)

	case turnevent.ThinkingBlock:
		s.renderer.OnThinking(e.Text)

	case turnevent.ToolCall:
		if s.tracker != nil {
			s.tracker.ObserveToolCall(e.ID, e.Name, e.Args)
		}

	case turnevent.ToolResult:
		if s.tracker != nil {
			s.tracker.ObserveToolResult(e.ID, e.Name, e.Output, e.IsError)
		}

	case turnevent.Activity:
		s.renderer.OnActivity()

	case turnevent.RetryNotice:
		if s.tracker != nil {
			s.tracker.NotifyRetry(e.Endpoint)
		}

	case turnevent.RetrySuccess:
		if s.tracker != nil {
			s.tracker.ClearRetryNotification()
		}

	case turnevent.TurnComplete:
		// Decide the text to render:
		//   - success: the accumulated FinalText
		//   - error (non-cancellation): a synthetic "Error: ..." message that
		//     replaces whatever FinalText had
		//   - cancellation: nothing (caller showed "Stopped." separately)
		text := e.FinalText
		if e.Err != nil && ctx.Err() == nil {
			text = fmt.Sprintf("Error: %s", e.Err.Error())
		}

		if s.delivered {
			// Content was already shown via OnReply during the turn — skip
			// re-delivery. Matches the historical replyDelivered-on-renderer
			// behaviour where errors that land after partial delivery are
			// swallowed in favour of keeping the visible stream intact.
			s.renderer.Cleanup()
			if s.tracker != nil {
				s.tracker.CleanupPreview()
			}
		} else {
			// Silent final text (sentinels, empty) is gated inside Finalize
			// itself — the renderer's OnReply and Finalize methods are the
			// authoritative gates for interactive-turn delivery.
			s.renderer.Finalize(text)
		}

		if s.conn != nil {
			s.conn.SetTyping(false)
		}
	}
}

// SessionSink delivers intermediate and final text via Connection.SendToSession.
// Used by injected/notify flows (agents_notify, session_notify, wakes) that
// don't need renderer streaming — they just need the text in the right chat.
//
// SessionSink owns its own delivered flag: once an intermediate TextBlock
// fires, the final TurnComplete text is suppressed, preventing double-delivery.
type SessionSink struct {
	conn       platform.Connection
	sessionKey string
	trigger    string // used for error logging; caller-provided label

	delivered bool
	onError   func(trigger string, err error)
}

// SessionSinkOption configures optional SessionSink behaviour.
type SessionSinkOption func(*SessionSink)

// WithSessionSinkErrorHandler installs a callback fired when SendToSession
// returns an error. Default is to drop errors silently.
func WithSessionSinkErrorHandler(fn func(trigger string, err error)) SessionSinkOption {
	return func(s *SessionSink) { s.onError = fn }
}

// NewSessionSink constructs a SessionSink for the given connection and session.
// trigger is a short label used for error-log attribution ("scheduled_wake",
// "async_notify", etc.).
func NewSessionSink(conn platform.Connection, sessionKey, trigger string, opts ...SessionSinkOption) *SessionSink {
	s := &SessionSink{conn: conn, sessionKey: sessionKey, trigger: trigger}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// DeliversToPlatform implements turnevent.Sink. SessionSink delivers text via
// Connection.SendToSession, so output reaches the user's platform chat.
// Returns true unconditionally — a nil conn is a misconfiguration handled by
// the per-event nil-guards below, not a deliberate non-delivery contract.
func (s *SessionSink) DeliversToPlatform() bool { return true }

// Emit implements turnevent.Sink.
func (s *SessionSink) Emit(_ context.Context, ev turnevent.Event) {
	switch e := ev.(type) {
	case turnevent.TurnStart:
		if s.conn != nil {
			s.conn.SetTyping(true)
		}
	case turnevent.Activity:
		if s.conn != nil {
			s.conn.SetTyping(true)
		}
	case turnevent.TextBlock:
		if e.Phase != turnevent.PhaseIntermediate || s.conn == nil {
			return
		}
		// Silent intermediate text — skip delivery. Don't set delivered=true,
		// so a non-silent final text on TurnComplete is still permitted.
		if platform.IsSilent(e.Text) {
			return
		}
		if err := s.conn.SendToSession(s.sessionKey, e.Text); err != nil && s.onError != nil {
			s.onError(s.trigger, err)
			return
		}
		s.delivered = true
	case turnevent.TurnComplete:
		if s.conn != nil {
			s.conn.SetTyping(false)
		}
		if s.delivered || platform.IsSilent(e.FinalText) || s.conn == nil {
			return
		}
		if err := s.conn.SendToSession(s.sessionKey, e.FinalText); err != nil && s.onError != nil {
			s.onError(s.trigger, err)
		}
	}
}
