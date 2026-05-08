package ccstream

import "foci/internal/delegator"

// applyHandler is a test-only adapter that lets older tests keep using the
// legacy EventHandler shape while ccstream's production code has migrated
// to the SessionEvents (delivery) + TurnEvents (bookkeeping) split. It
// installs the delivery callbacks via AttachSessionEvents and the
// bookkeeping callbacks via beginTurn — i.e. exactly what the agent layer
// does in turn_delegated.go, factored for compactness.
//
// Tests that legitimately want to test the split (e.g. assert delivery
// without bookkeeping, or vice versa) call AttachSessionEvents and
// beginTurn directly.
func applyHandler(b *Backend, h *delegator.EventHandler) {
	if h == nil {
		b.AttachSessionEvents(nil)
		b.beginTurn(nil)
		return
	}
	b.AttachSessionEvents(&delegator.SessionEvents{
		OnText:          h.OnText,
		OnTextDelta:     h.OnTextDelta,
		OnThinkingDelta: h.OnThinkingDelta,
		OnToolStart:     h.OnToolStart,
		OnToolEnd:       h.OnToolEnd,
	})
	b.beginTurn(&delegator.TurnEvents{
		OnTurnComplete:     h.OnTurnComplete,
		PostToolNudgeFunc:  h.PostToolNudgeFunc,
		PreAnswerNudgeFunc: h.PreAnswerNudgeFunc,
	})
}
