// Package ccstream — re-arm cascade for user-role injections that race
// turn completion. When foci injects a user-role event mid-turn (post-tool
// nudge, mid-turn steer dispatch, or follow-up via SendCommand on an
// in-flight turn), CC either aborts the in-flight turn or queues the event
// behind it. In both cases CC emits a result message for the original turn
// AND will produce a fresh round of assistant output for the injected event.
//
// Without re-arming, that fresh output hits a cleared handler and the
// text is silently dropped. completeAndRearm fires the original turn's
// OnTurnComplete, then installs a delivery-only handler so the injected
// event's response reaches OnText.
//
// pendingRearmCount semantics:
//
// When non-zero, OnResult must take the complete-and-rearm path: complete
// the foci turn (the first time) or absorb the next queued response (every
// time after), then install a delivery-only handler so the queued user-role
// response reaches OnText. Each injected event increments the count; each
// OnResult that takes the rearm path decrements it. When multiple events
// stack within a single in-flight CC turn, the count drains across
// successive OnResult cycles — each event gets its own delivery handler,
// and the count reaches zero only after CC has emitted a result for every
// queued event.
//
// The "fire OnTurnComplete only once" invariant falls out naturally: the
// original handler (with a real OnTurnComplete) is captured by the first
// rearm cycle; rearmForNudgeResponse then installs a delivery-only handler
// whose OnTurnComplete is nil. Subsequent OnResult cycles capture that
// nil-OnTurnComplete handler, so completeAndRearm's nil-check no-ops the
// callback. Consumers see exactly one foci-turn boundary regardless of
// how many CC result messages flow through.
//
// Pre-counter (single-bit rearmReason) the second event's response raced
// against the first cycle's handler clear and was dropped — surfaced as
// the production "text block dropped (no handler/OnText)" WARN. The
// counter eliminates that race by giving every queued event its own
// delivery cycle.
//
// The protocol assumes CC emits one result + one assistant cycle per
// injected event. If CC fails to produce a result for some queued event,
// the count stays positive — but it doesn't underflow (decRearm clamps at
// zero) and the next beginTurn resets it.

package ccstream

import (
	"foci/internal/delegator"
)

// incRearm signals that a user-role event has been injected mid-turn and
// the next OnResult must complete-and-rearm. Caller must hold turnMu.
//
// Stacking is supported: two events queued within the same in-flight turn
// increment the count to 2, and successive OnResult cycles drain it.
func (b *Backend) incRearm() {
	b.pendingRearmCount++
}

// decRearm consumes one pending rearm signal. Returns the count BEFORE
// decrement, so callers can branch on whether the rearm path applies.
// Clamps at zero on underflow (defensive — should not happen in practice
// since OnResult only consumes when count > 0). Caller must hold turnMu.
func (b *Backend) decRearm() int {
	prev := b.pendingRearmCount
	if prev > 0 {
		b.pendingRearmCount--
	}
	return prev
}

// completeAndRearm fires the original handler's OnTurnComplete (if set),
// installs a delivery-only handler for the queued response, and signals
// WaitForTurn.
//
// Caller must NOT hold turnMu — rearmForNudgeResponse takes it. handler is
// the turn handler captured before its OnResult clearing; result and msg
// are passed through to the OnTurnComplete callback and the WaitForTurn
// channel respectively.
//
// Multi-cycle behaviour: on the first rearm cycle the handler is the
// caller's original (real OnTurnComplete fires); on subsequent cycles the
// handler is the delivery-only one installed by a prior rearmForNudgeResponse
// (OnTurnComplete is nil and the nil-check no-ops). This keeps the foci-turn
// boundary firing exactly once across stacked events.
func (b *Backend) completeAndRearm(
	handler *delegator.EventHandler,
	result *delegator.TurnResult,
	msg *ResultMessage,
	resultCh chan *ResultMessage,
) {
	b.agents.ClearAll()
	if handler.OnTurnComplete != nil {
		handler.OnTurnComplete(result)
	}
	if b.typingFunc != nil {
		b.typingFunc(true)
	}
	b.rearmForNudgeResponse(handler)

	b.logger().Infof("OnResult: re-armed for queued response (text=%d bytes)", len(result.Text))

	if resultCh != nil {
		select {
		case resultCh <- msg:
		default:
		}
	}
}
