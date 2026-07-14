package ccstream

import (
	"time"

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
	b.turnMu.Unlock()

	if !active {
		// No foci turn is open for this run — a slash-command run, or a run whose
		// autonomous adopt was declined (onAutonomousOpen unset, e.g. tests). A
		// first-class turn — including an adopted autonomous run (#1261) — always
		// sets turnActive before its text flows, so a real user-facing reply never
		// lands here; there is nothing to complete or deliver.
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

// drainEdgeCallbacks fires queued reader-goroutine edge callbacks (the
// autonomous-open enqueued at the running edge, #1261) in FIFO order. fireMu
// serialises drainers so exactly one fires them in order; turnMu is taken only
// to pop each callback and released before firing it (the callbacks re-enter
// the agent — building sinks, taking agent locks — and must not run under
// turnMu). A concurrent drainer blocks on fireMu, then finds an empty queue.
func (b *Backend) drainEdgeCallbacks() {
	b.fireMu.Lock()
	defer b.fireMu.Unlock()
	for {
		b.turnMu.Lock()
		if len(b.edgeCallbacks) == 0 {
			b.turnMu.Unlock()
			return
		}
		fn := b.edgeCallbacks[0]
		b.edgeCallbacks = b.edgeCallbacks[1:]
		b.turnMu.Unlock()
		fn()
	}
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
	// accumulator so the final result carries only the revised reply.
	// Output-token accumulator is also reset: round-1's usage is stashed
	// separately in PriorCallUsages (one api.db row per round), so round-2's
	// FinalUsage must carry only round-2's output — not the cross-round sum
	// (which would double-count round-1 when both rows are summed).
	b.turnText.Reset()
	b.turnOutputTokens = 0
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
	autonomous := b.turnAutonomous
	b.turnEvents = nil
	b.turnActive = false
	b.turnAutonomous = false
	b.stashedResult = nil
	b.stashedResultMsg = nil
	b.redispatchInFlight = false
	if autonomous {
		// Arm the post-run grace: for a short window after a CC-initiated turn
		// completes, SourceSystem injects still defer (tryBeginTurn) so a
		// reflection/keepalive can't slip in between this turn's idle and a
		// possible back-to-back continuation whose running edge re-arms turnActive.
		b.lastAutonomousEnd = time.Now()
	}
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
