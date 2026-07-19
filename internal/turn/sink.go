package turn

import (
	"context"
	"encoding/json"
	"fmt"

	"foci/internal/agent/turnevent"
	"foci/internal/log"
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

// logger returns the platform's session/agent-scoped logger so turn-sink lines
// are attributable (e.g. [telegram:clutch], [app:<conv>]) rather than a bare
// [turn-sink] shared across every concurrent session. The "turn-sink" marker
// moves into the message text so the lines stay greppable.
func (s *StreamingSink) logger() *log.ComponentLogger {
	return s.renderer.platform.Logger()
}

// NewStreamingSink constructs a StreamingSink. renderer and tracker are
// required; conn may be nil in tests where typing-indicator side effects are
// irrelevant.
func NewStreamingSink(renderer *TurnRenderer, tracker SinkTracker, conn platform.Connection) *StreamingSink {
	s := &StreamingSink{
		renderer: renderer,
		tracker:  tracker,
		conn:     conn,
	}
	s.logger().Debugf("turn-sink sink=%p NewStreamingSink: created (conn=%v)", s, conn != nil)
	return s
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
		s.logger().Debugf("turn-sink sink=%p TurnStart: activating typing indicator", s)
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
			silent := platform.IsSilent(e.Text)
			s.logger().Debugf("turn-sink sink=%p TextBlock(intermediate): text_len=%d silent=%v delivered_before=%v", s, len(e.Text), silent, s.delivered)
			s.renderer.OnReply(e.Text)
			// Gate delivered on !silent: OnReply's silent path (text empty
			// after stripping trailing sentinels) returns without surfacing
			// anything to the user, so the sink must not claim delivery.
			// Without this gate, a silent intermediate
			// (e.g. [[NO_RESPONSE]]) followed by a TurnComplete carrying
			// non-empty FinalText (from msg.Result when accumulated text
			// is empty, or across pre-answer-gate rounds) would suppress
			// Finalize and drop the real reply.
			if !silent {
				s.delivered = true
			}
			s.logger().Debugf("turn-sink sink=%p TextBlock(intermediate): delivered_after=%v (silent_gated)", s, s.delivered)
		} else {
			s.logger().Debugf("turn-sink sink=%p TextBlock(final): text_len=%d (no-op — final text carried by TurnComplete)", s, len(e.Text))
		}

	case turnevent.SubagentStart:
		s.renderer.OnSubagentStart(e.GroupKey, e.Label, e.RunIndex, e.Prompt)

	case turnevent.SubagentText:
		// Subagent progress is ancillary — route it to the renderer (which
		// hands it to the platform's per-subagent control if supported) but do
		// NOT set s.delivered, so it never suppresses the real final reply.
		s.renderer.OnSubagentReply(e.GroupKey, e.Text)

	case turnevent.SubagentEnd:
		s.renderer.OnSubagentEnd(e.GroupKey, e.RunIndex)

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
		errReplaced := false
		if e.Err != nil && ctx.Err() == nil {
			text = fmt.Sprintf("Error: %s", e.Err.Error())
			errReplaced = true
		}
		s.logger().Debugf("turn-sink sink=%p TurnComplete: final_text_len=%d silent=%v delivered=%v err=%v err_replaced=%v ctx_err=%v", s, len(text), platform.IsSilent(text), s.delivered, e.Err, errReplaced, ctx.Err())

		if s.delivered {
			// Content was already shown via OnReply during the turn — skip
			// re-delivery. Matches the historical replyDelivered-on-renderer
			// behaviour where errors that land after partial delivery are
			// swallowed in favour of keeping the visible stream intact.
			s.logger().Debugf("turn-sink sink=%p TurnComplete: branch=cleanup (delivered=true, FinalText suppressed)", s)
			s.renderer.Cleanup()
			if s.tracker != nil {
				s.tracker.CleanupPreview()
			}
		} else {
			// Silent final text (sentinels, empty) is gated inside Finalize
			// itself — the renderer's OnReply and Finalize methods are the
			// authoritative gates for interactive-turn delivery.
			s.logger().Debugf("turn-sink sink=%p TurnComplete: branch=finalize (delivered=false, calling Finalize)", s)
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
	lg         *log.ComponentLogger

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
	s := &SessionSink{conn: conn, sessionKey: sessionKey, trigger: trigger, lg: log.NewComponentLogger("turn-sink:" + sessionKey)}
	for _, opt := range opts {
		opt(s)
	}
	s.lg.Debugf("sink=%p NewSessionSink: created trigger=%s conn=%v", s, trigger, conn != nil)
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
		s.lg.Debugf("sink=%p SessionSink TurnStart: trigger=%s", s, s.trigger)
		if s.conn != nil {
			s.conn.SetTyping(true)
		}
	case turnevent.Activity:
		if s.conn != nil {
			s.conn.SetTyping(true)
		}
	case turnevent.SubagentStart:
		if sd, ok := s.conn.(SessionSubagentDeliverer); ok {
			sd.DeliverSubagentStartToSession(s.sessionKey, e.GroupKey, e.Label, e.RunIndex, e.Prompt)
		}
	case turnevent.SubagentEnd:
		if sd, ok := s.conn.(SessionSubagentDeliverer); ok {
			sd.DeliverSubagentEndToSession(s.sessionKey, e.GroupKey, e.RunIndex)
		}
	case turnevent.SubagentText:
		// Do not set delivered — subagent progress must not suppress the final reply.
		if s.conn == nil {
			return
		}
		if sd, ok := s.conn.(SessionSubagentDeliverer); ok && e.GroupKey != "" {
			sd.DeliverSubagentTextToSession(s.sessionKey, e.GroupKey, e.Text)
			return
		}
		text := platform.StripSilencingSuffix(platform.StripSpuriousPrefix(e.Text))
		if text == "" {
			return
		}
		if err := s.conn.SendToSession(s.sessionKey, text); err != nil && s.onError != nil {
			s.onError(s.trigger, err)
		}
	case turnevent.TextBlock:
		if e.Phase != turnevent.PhaseIntermediate || s.conn == nil {
			s.lg.Debugf("sink=%p SessionSink TextBlock: skip (phase=%v conn_nil=%v)", s, e.Phase, s.conn == nil)
			return
		}
		// Strip a leading spurious token then trailing silencing sentinel(s)
		// before delivery. Text that is entirely junk/sentinel strips to "" —
		// skip delivery, but don't set delivered=true, so a non-silent final
		// text on TurnComplete is still permitted.
		text := platform.StripSilencingSuffix(platform.StripSpuriousPrefix(e.Text))
		s.lg.Debugf("sink=%p SessionSink TextBlock(intermediate): text_len=%d stripped_len=%d delivered_before=%v", s, len(e.Text), len(text), s.delivered)
		if text == "" {
			return
		}
		if err := s.conn.SendToSession(s.sessionKey, text); err != nil && s.onError != nil {
			s.lg.Debugf("sink=%p SessionSink TextBlock: SendToSession error=%v", s, err)
			s.onError(s.trigger, err)
			return
		}
		s.delivered = true
		s.lg.Debugf("sink=%p SessionSink TextBlock: delivered_after=true", s)
	case turnevent.TurnComplete:
		// Strip a leading spurious token then trailing silencing sentinel(s) so
		// an agent that appended the marker to a real reply still delivers the
		// clean text; a fully-silent/junk FinalText strips to "" and is
		// suppressed below.
		text := platform.StripSilencingSuffix(platform.StripSpuriousPrefix(e.FinalText))
		s.lg.Debugf("sink=%p SessionSink TurnComplete: final_text_len=%d stripped_len=%d delivered=%v conn_nil=%v", s, len(e.FinalText), len(text), s.delivered, s.conn == nil)
		if s.conn != nil {
			s.conn.SetTyping(false)
		}
		if s.delivered || text == "" || s.conn == nil {
			s.lg.Debugf("sink=%p SessionSink TurnComplete: suppressed (delivered=%v stripped_empty=%v conn_nil=%v)", s, s.delivered, text == "", s.conn == nil)
			return
		}
		if err := s.conn.SendToSession(s.sessionKey, text); err != nil && s.onError != nil {
			s.lg.Debugf("sink=%p SessionSink TurnComplete: SendToSession error=%v", s, err)
			s.onError(s.trigger, err)
		} else {
			s.lg.Debugf("sink=%p SessionSink TurnComplete: delivered FinalText", s)
		}
	}
}
