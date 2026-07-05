package ccstream

import (
	"foci/internal/delegator"
)

// onSessionIdle handles CC's session_state_changed:idle — the authoritative
// end of CC's internal run loop and therefore of the foci turn. Every ask
// cycle CC ran for this turn (the original message plus any drained steers,
// follow-ups, nudges, aborts and background-agent flushes) has emitted its
// result by now, so the stashed result is the turn's final accounting.
//
// The pre-answer nudge gate runs here, on the final stashed result, rather
// than per-result: with 0/1/N results per turn only idle identifies the
// answer worth verifying. When the gate returns a follow-up it is sent as a
// fresh user message and the turn is held open (redispatchInFlight) — CC
// starts a new run for it, whose result clears the flag and whose idle
// completes the turn with the revised answer. The gate's caller guarantees
// it stops returning a follow-up after the first fire (the turn_delegated
// closure tracks "fired" locally), so the second idle falls through to
// completion.
func (b *Backend) onSessionIdle() {
	b.turnMu.Lock()
	turn := b.turnEvents
	active := b.turnActive
	redispatch := b.redispatchInFlight
	result := b.stashedResult
	b.autonomousActive = false // idle ends any autonomous turn
	b.turnMu.Unlock()

	if !active {
		// Autonomous turn: a run foci didn't open a turn for (slash commands,
		// task-notification runs after a background-agent/Bash finishes,
		// proactive ticks). Nothing to complete.
		return
	}
	if redispatch {
		// A pre-answer follow-up is still owed a result — this idle closed
		// the run that ended before CC picked the follow-up up. Keep the
		// turn open; the follow-up's own run ends in the idle that counts.
		b.logger().Debugf("onSessionIdle: pre-answer re-dispatch outstanding; holding turn open")
		return
	}

	if b.tryPreAnswerRedispatch(turn, result) {
		return
	}

	b.completeTurn("idle")
}

// tryPreAnswerRedispatch runs the pre-answer nudge gate against the turn's
// final (stashed) result. When the gate returns a follow-up it is sent as a
// fresh user message and the turn is held open for the revised answer;
// returns true so the caller skips completion. A send failure degrades to
// completing with the first-round result (returns false). Shared by
// onSessionIdle (the normal path) and OnResult's legacy fallback, so the
// verification nudge behaves identically with and without session-state
// events.
func (b *Backend) tryPreAnswerRedispatch(turn *delegator.TurnEvents, result *delegator.TurnResult) bool {
	if turn == nil || turn.PreAnswerNudgeFunc == nil || result == nil {
		return false
	}
	followUp := turn.PreAnswerNudgeFunc(result)
	if followUp == "" {
		return false
	}
	if err := b.writer.SendUser(followUp); err != nil {
		b.logger().Errorf("pre-answer re-dispatch: send user: %v — completing with first-round result", err)
		return false
	}
	b.turnMu.Lock()
	b.redispatchInFlight = true
	// The revision supersedes the first-round answer: reset the text
	// accumulator so the final result carries only the revised reply (output
	// tokens keep accumulating — the first round's cost is real).
	b.turnText.Reset()
	b.turnMu.Unlock()
	if b.typingFunc != nil {
		b.typingFunc(true)
	}
	b.touchActivity()
	b.logger().Debugf("turn_lifecycle event=preanswer_redispatch followup_len=%d round1_textlen=%d",
		len(followUp), len(result.Text))
	return true
}

// completeTurn fires OnTurnComplete for the current turn with the stashed
// result and resets turn state. Callers: onSessionIdle (the normal path) and
// OnResult's legacy fallback when CC emits no session-state events. The claim
// of turnEvents happens in one turnMu critical section, so a racing
// finalizeExit (which claims the same way) fires at most one completion; the
// agent layer's sync.Once is the hard backstop.
func (b *Backend) completeTurn(reason string) {
	b.turnMu.Lock()
	if !b.turnActive {
		b.turnMu.Unlock()
		return
	}
	turn := b.turnEvents
	result := b.stashedResult
	msg := b.stashedResultMsg
	resultCh := b.turnResultCh
	cycles := b.turnCalls
	b.turnEvents = nil
	b.turnActive = false
	b.stashedResult = nil
	b.stashedResultMsg = nil
	b.redispatchInFlight = false
	b.turnMu.Unlock()

	if result == nil {
		// Idle with no result at all this turn (CC aborted before any ask
		// cycle finished). Complete empty so the orchestrator releases.
		result = &delegator.TurnResult{}
	}

	b.logger().Debugf("turn complete (%s): had_turn_events=%v cycles=%d textlen=%d",
		reason, turn != nil, cycles, len(result.Text))
	b.logger().Debugf("turn_lifecycle event=complete reason=%s had_turn_events=%v cycles=%d textlen=%d out_total=%d",
		reason, turn != nil, cycles, len(result.Text), resultOutputTokens(result))

	// Background agents outlive the turn that spawned them, so they are NOT
	// cleared here — they persist across turns and are removed individually by
	// task_notification (or the tracker's max-age prune). ClearAll now runs
	// only on session exit (lifecycle finalizeExit).

	// Fire OnTurnComplete OUTSIDE any lock.
	if turn != nil && turn.OnTurnComplete != nil {
		turn.OnTurnComplete(result)
	}

	if b.typingFunc != nil {
		b.typingFunc(false)
	}

	// Signal WaitForTurn (non-blocking). msg may be nil when the turn had no
	// result; the channel signal alone is what waiters block on.
	if resultCh != nil {
		select {
		case resultCh <- msg:
		default:
		}
	}
}

// resultOutputTokens is a nil-safe accessor for logging.
func resultOutputTokens(r *delegator.TurnResult) int {
	if r == nil || r.Usage == nil {
		return 0
	}
	return r.Usage.OutputTokens
}
