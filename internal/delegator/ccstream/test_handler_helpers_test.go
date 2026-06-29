package ccstream

import "foci/internal/delegator"

// testHandler is a test-only bundle of all per-turn callbacks in one struct —
// the shape the production EventHandler used to have. It lets the many ccstream
// tests written against the combined form stay compact; applyHandler splits it
// into the production SessionEvents (delivery) + TurnEvents (bookkeeping) form,
// exactly as the agent layer does in turn_delegated.go.
//
// Tests that legitimately want to exercise the split (delivery without
// bookkeeping, or vice versa) call AttachSessionEvents and beginTurn directly.
type testHandler struct {
	OnText          func(text string)
	OnTextDelta     func(delta string)
	OnThinkingDelta func(delta string)
	OnToolStart     func(id, name, input string)
	OnToolEnd       func(id, name, output string, isError bool)
	OnTurnComplete  func(result *delegator.TurnResult)

	PostToolNudgeFunc  func(toolName, toolInput string, isError bool) []string
	PreAnswerNudgeFunc func(result *delegator.TurnResult) string
}

// session/turn split a testHandler into the production delivery/bookkeeping
// pair, for tests that drive Inject directly (AttachSessionEvents + Inject.Turn).
func (h *testHandler) session() *delegator.SessionEvents {
	if h == nil {
		return nil
	}
	return &delegator.SessionEvents{
		OnText:          h.OnText,
		OnTextDelta:     h.OnTextDelta,
		OnThinkingDelta: h.OnThinkingDelta,
		OnToolStart:     h.OnToolStart,
		OnToolEnd:       h.OnToolEnd,
	}
}

func (h *testHandler) turn() *delegator.TurnEvents {
	if h == nil {
		return nil
	}
	return &delegator.TurnEvents{
		OnTurnComplete:     h.OnTurnComplete,
		PostToolNudgeFunc:  h.PostToolNudgeFunc,
		PreAnswerNudgeFunc: h.PreAnswerNudgeFunc,
	}
}

func applyHandler(b *Backend, h *testHandler) {
	if h == nil {
		b.AttachSessionEvents(nil)
		b.beginTurn(nil, true)
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
	}, true)
}
