package agent

import (
	"context"

	"foci/internal/agent/turnevent"
)

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
	inner   turnevent.Sink
	a       *Agent
	chatID  int64
	meta    *TurnMetadata
	sk      string
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
	return &loggingSink{inner: inner, a: a, chatID: chatID, meta: meta, sk: sk}
}

// Emit forwards every event to inner, additionally logging intermediate
// TextBlock events to the conversation DB.
func (s *loggingSink) Emit(ctx context.Context, ev turnevent.Event) {
	if e, ok := ev.(turnevent.TextBlock); ok && e.Phase == turnevent.PhaseIntermediate && e.Text != "" {
		s.a.logConversationSent(s.chatID, s.meta, s.sk, e.Text)
	}
	s.inner.Emit(ctx, ev)
}
