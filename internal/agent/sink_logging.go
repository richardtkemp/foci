package agent

import (
	"context"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/convo"
	"foci/internal/session"
)

// activityHeartbeatInterval bounds how often a single turn's loggingSink
// persists a mid-turn activity touch. Comfortably below the 1m default cold
// window so the durable timestamps stay fresh, while keeping DB writes to at
// most ~3/min/turn regardless of how many rounds fire.
const activityHeartbeatInterval = 20 * time.Second

// loggingSink wraps a turnevent.Sink with conversation-database logging for
// TextBlock events. The conv DB log used to live inside the per-turn OnText
// closure in turn_delegated.go, but with the SessionEvents/TurnEvents split
// (TODO #747) delivery callbacks are session-scoped and don't have per-turn
// metadata in lexical scope. Moving the log to the sink layer keeps it
// reachable both during the turn (per-turn StreamingSink wrapped here) and
// for late delivery (fallback SessionSink wrapped in lateDeliverySink).
//
// The wrapper logs intermediate TextBlock events; final text on
// TurnComplete is logged elsewhere (LogConversationSent for API turns; the
// delegated path doesn't double-log because intermediate delivery handles
// the whole turn's text). Other event types pass through unchanged.
type loggingSink struct {
	inner     turnevent.Sink
	a         *Agent
	chatID    int64
	meta      *TurnMetadata
	sk        string
	lastTouch time.Time // debounce for the mid-turn activity heartbeat; single-producer (Emit is sequential per turn), so no lock
}

// newLoggingSink wraps inner with conv-DB logging using the supplied
// per-turn metadata. Returns inner unwrapped if a is nil — defensive
// against test wiring that lacks an agent.
func newLoggingSink(inner turnevent.Sink, a *Agent, chatID int64, meta *TurnMetadata, sk string) turnevent.Sink {
	if a == nil || inner == nil {
		return inner
	}
	if meta == nil {
		meta = &TurnMetadata{}
	}
	// Seed lastTouch to construction time (≈ turn entry, where recordTurnActivity
	// already wrote) so the first heartbeat waits a full interval rather than
	// firing a redundant write immediately after the entry stamp.
	return &loggingSink{inner: inner, a: a, chatID: chatID, meta: meta, sk: sk, lastTouch: time.Now()}
}

// Emit forwards every event to inner, additionally logging intermediate
// TextBlock events to the conversation DB.
func (s *loggingSink) Emit(ctx context.Context, ev turnevent.Event) {
	if e, ok := ev.(turnevent.TextBlock); ok && e.Phase == turnevent.PhaseIntermediate && e.Text != "" {
		s.a.logConversationSent(s.chatID, s.meta, s.sk, e.Text)
	}
	s.heartbeat(ctx, ev)
	s.inner.Emit(ctx, ev)
}

// heartbeat persists a mid-turn activity touch on per-round events so the
// durable timestamps track the turn's progress instead of freezing at entry
// (see Agent.touchTurnActivity). Rounds are the backend-agnostic mid-turn
// signals — a completed message (TextBlock), a tool result (ToolResult), or the
// Activity liveness beat — all emitted by both the API tool loop and the
// delegated ask-cycle path via the shared emit* helpers. Other event types
// (deltas, TurnStart/Complete, thinking) are ignored.
//
// Two guards keep it honest and cheap:
//   - IsTurnInFlight(sk): only touch while a turn is genuinely running, so a
//     loggingSink reused for POST-turn late delivery (inbox.go) does not mark
//     phantom activity.
//   - lastTouch debounce: at most one write per activityHeartbeatInterval per
//     turn, however many rounds fire. Emit is sequential within a turn, so the
//     unlocked lastTouch read/write is safe.
//
// The trigger comes from ctx (memory-formation turns must not bump
// last_activity_at — touchTurnActivity applies that exclusion).
func (s *loggingSink) heartbeat(ctx context.Context, ev turnevent.Event) {
	switch ev.(type) {
	case turnevent.TextBlock, turnevent.ToolResult, turnevent.Activity:
	default:
		return
	}
	if s.a == nil || s.sk == "" || !s.a.IsTurnInFlight(s.sk) {
		return
	}
	now := time.Now()
	if now.Sub(s.lastTouch) < activityHeartbeatInterval {
		return
	}
	s.lastTouch = now
	s.a.touchTurnActivity(s.sk, TriggerFromContext(ctx))
}

// DeliversToPlatform implements turnevent.Sink by forwarding the answer from
// inner. loggingSink is a transparent wrapper — adding conv-DB logging
// doesn't change whether the underlying sink reaches a user-facing platform.
func (s *loggingSink) DeliversToPlatform() bool {
	return s.inner.DeliversToPlatform()
}

// WrapConversationLogging wraps sink so the session's outbound text reaches the
// conversation DB. Turn paths that build their own delivery sink instead of
// going through Agent.RunTurn — the async HTTP /send path and every
// system-injected delivery, both via turnSinkForConn in cmd/foci-gw — must call
// this, or their replies deliver to the user and are never persisted (#1784).
//
// Injected turns carry no incoming message, so the chat ID resolves from the
// session key rather than TurnMetadata, exactly as the adopted-autonomous path
// in in_flight.go does.
func (a *Agent) WrapConversationLogging(sink turnevent.Sink, sessionKey string) turnevent.Sink {
	return newLoggingSink(sink, a, session.ChatIDFromKey(sessionKey), &TurnMetadata{}, sessionKey)
}

// RecordConversationSent persists text as an outbound conversation entry for
// sessionKey, for delivery paths that have no per-event sink to wrap. The
// broadcast fan-out runs the turn behind a turnevent.BufferSink, which emits no
// TextBlock at all, so WrapConversationLogging has nothing to intercept there
// and the text would otherwise reach every surface unlogged (#1784).
func RecordConversationSent(sessionKey, text string) {
	if text == "" {
		return
	}
	convo.Record(convo.Entry{
		Direction: "sent",
		ChatID:    session.ChatIDFromKey(sessionKey),
		Text:      text,
		Session:   sessionKey,
	})
}
