// Package ccstream — re-arm cascade for user-role injections that race
// turn completion. When foci injects a user-role event mid-turn (post-tool
// nudge, mid-turn steer, follow-up via SendCommand priority="next"), CC
// either aborts the in-flight turn or queues the event behind it. In both
// cases CC emits a result message for the original turn AND will produce
// a fresh round of assistant output for the injected event.
//
// Without re-arming, that fresh output hits a cleared handler and the
// text is silently dropped. completeAndRearm fires the original turn's
// OnTurnComplete, then installs a delivery-only handler so the injected
// event's response reaches OnText.
//
// All three injection paths share an identical recovery shape — only the
// log message varies. The single rearmReason scalar replaces three
// previously-parallel boolean flags (nudgePending, steerInjected,
// followUpQueued).

package ccstream

import (
	"fmt"

	"foci/internal/delegator"
)

// rearmReason names the reason the next OnResult must take the
// complete-and-rearm path instead of normal turn cleanup. The three
// production paths are mutually exclusive — see setRearmReason for the
// guard that surfaces violations.
type rearmReason int

const (
	// rearmNone means OnResult should clear the handler normally — no
	// queued user-role injection is awaiting delivery.
	rearmNone rearmReason = iota

	// rearmNudge: a PostToolNudge sent via PriorityNow. CC will process
	// the nudge as a fresh CC-internal turn after this result.
	rearmNudge

	// rearmSteer: checkAndSendSteers sent a PriorityNow steer. CC aborted
	// the in-flight tool execution; the steered response follows.
	rearmSteer

	// rearmFollowUp: SendCommand queued a follow-up via priority="next".
	// CC will process the follow-up after the current result emits.
	rearmFollowUp
)

// String returns a stable lower-case label for logging.
func (r rearmReason) String() string {
	switch r {
	case rearmNone:
		return "none"
	case rearmNudge:
		return "nudge"
	case rearmSteer:
		return "steer"
	case rearmFollowUp:
		return "followup"
	default:
		return fmt.Sprintf("rearmReason(%d)", int(r))
	}
}

// setRearmReason records the reason the next OnResult must re-arm. Caller
// must hold turnMu.
//
// Mutual-exclusion guard: the three injection paths are gated such that
// only one can fire per turn (a steer aborts in-flight tool execution
// before a follow-up could queue; a nudge fires only between tool boundaries
// where a steer wouldn't be in flight). If a second reason arrives before
// OnResult clears the first, the new reason wins but a warning surfaces
// the violation — silent loss of one of the queued responses is the
// failure shape this guard exists to prevent. If real traffic shows
// stacking is legitimate, the model needs to become a queue.
func (b *Backend) setRearmReason(r rearmReason) {
	if b.pendingRearmReason != rearmNone && b.pendingRearmReason != r {
		b.logger().Warnf("rearm reason overwritten: was=%s now=%s — mutual-exclusion assumption violated, queued response may be lost",
			b.pendingRearmReason, r)
	}
	b.pendingRearmReason = r
}

// completeAndRearm fires the original handler's OnTurnComplete, installs a
// delivery-only handler for the queued response, and signals WaitForTurn.
// Used for all three "user-role injection mid-turn" cases (nudge, steer,
// follow-up) — they share an identical recovery shape, only the log
// message varies.
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
	reason rearmReason,
) {
	b.agents.ClearAll()
	if handler.OnTurnComplete != nil {
		handler.OnTurnComplete(result)
	}
	if b.typingFunc != nil {
		b.typingFunc(true)
	}
	b.rearmForNudgeResponse(handler)

	switch reason {
	case rearmNudge:
		b.logger().Infof("OnResult: re-armed for pending nudge response")
	case rearmSteer:
		b.logger().Infof("OnResult: re-armed for steered response (text=%d bytes)", len(result.Text))
	case rearmFollowUp:
		b.logger().Infof("OnResult: re-armed for queued follow-up response (text=%d bytes)", len(result.Text))
	default:
		b.logger().Infof("OnResult: re-armed for %s (text=%d bytes)", reason, len(result.Text))
	}

	if resultCh != nil {
		select {
		case resultCh <- msg:
		default:
		}
	}
}
