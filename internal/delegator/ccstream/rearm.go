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
// All injection paths share an identical recovery shape, so the rearm state
// is a binary flag: rearmNone (normal completion) or rearmPending (a queued
// response is awaiting delivery).

package ccstream

import (
	"fmt"

	"foci/internal/delegator"
)

// rearmReason names the reason the next OnResult must take the
// complete-and-rearm path instead of normal turn cleanup.
type rearmReason int

const (
	// rearmNone means OnResult should clear the handler normally — no
	// queued user-role injection is awaiting delivery.
	rearmNone rearmReason = iota

	// rearmPending means a user-role event has been injected mid-turn
	// (post-tool nudge, urgent steer dispatch, or follow-up via SendCommand
	// on an in-flight turn) and CC will produce a fresh round of assistant
	// output for it after the current result message. OnResult must complete
	// the foci turn with the pre-injection text, then install a delivery-only
	// handler so the queued response reaches OnText.
	rearmPending
)

// String returns a stable lower-case label for logging.
func (r rearmReason) String() string {
	switch r {
	case rearmNone:
		return "none"
	case rearmPending:
		return "pending"
	default:
		return fmt.Sprintf("rearmReason(%d)", int(r))
	}
}

// setRearmReason records that the next OnResult must re-arm. Caller must
// hold turnMu.
//
// Stacking guard: the protocol assumes at most one queued user-role event
// per CC turn — CC emits one result and one assistant cycle per injected
// event, and the rearm flag is a single bit that re-arms once. If two
// events stack within the same in-flight turn (e.g. a post-tool nudge fires
// just as a Telegram-side urgent steer dispatches), the second event's
// response will stream through the re-armed delivery-only handler from the
// first event's recovery, but the cleanup cycle ends after the first
// queued response — the second response races against handler clear. The
// WARN here surfaces stacking when it happens; if real traffic shows it's
// frequent the rearm state needs to become a counter or queue.
func (b *Backend) setRearmReason(r rearmReason) {
	if b.pendingRearmReason != rearmNone && r != rearmNone && b.pendingRearmReason == r {
		b.logger().Warnf("rearm reason already pending — second event may lose its response (stacking)")
	}
	b.pendingRearmReason = r
}

// completeAndRearm fires the original handler's OnTurnComplete, installs a
// delivery-only handler for the queued response, and signals WaitForTurn.
//
// Caller must NOT hold turnMu — rearmForNudgeResponse takes it. handler is
// the original turn handler (captured before its OnResult clearing); result
// and msg are passed through to the OnTurnComplete callback and the
// WaitForTurn channel respectively.
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
