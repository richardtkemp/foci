package agent

import (
	"slices"

	"foci/internal/platform"
)

// Limit break (#3128, Dick 2026-11-15: "a new command /limit-break alias /lb
// to force through a message past the limit. E.g. /lb hey one last thing real
// quick...").
//
// A user message whose first word is /limit_break, /lb or /limit-break runs
// its remaining text as a normal user turn now, although the account's usage
// hold is on, in both the hold and the stop phase (if the backend then fails
// at the real limit, the user sees that error as usual). The hold itself
// stays on:
//   - Enqueue strips the word (markUsageForce); the agent sees only the text;
//   - Enqueue does not send it to the worker for the hold: it takes the
//     normal routing, so it steers into a running work turn (a remind wake
//     the hold lets through, #3120 Q57) instead of waiting it out (#3131),
//     and waits out a protected turn as any user message does;
//   - the worker does not park the message (handleDequeued) and its batch
//     does not wait for the lift (runUserBatch → waitUserTurnGates);
//   - while the hold is on, only other limit-break messages join its batch;
//     every other queued message is parked as usual (keepForcedOnly), and
//     what was parked before stays parked;
//   - a hold starting while the batch runs does not interrupt it
//     (interruptTurns skips a forcing inbox), so it runs to completion;
//   - schedulers, injections and every later plain message stay held.
//
// The chat surfaces hand such a message to the agent as text rather than
// dispatching it as a command (dispatch.IsLimitBreakMessage, the app's
// Command-frame branch); the registered command (command.LimitBreakCommand)
// lists it and handles a bare /lb (LimitBreakHeld).
//
// A bare /lb (#3182) runs every held user message of the session now,
// oldest first, as if each had been typed with /lb in front. See
// LimitBreakHeld.

// markUsageForce strips a limit-break word from a user message and marks it
// (Envelope.usageForce). Injections, automated sends and restored copies are
// left alone, as is a bare word with no text.
func markUsageForce(env *Envelope) {
	if env.Inject != nil || env.Automated || env.usageHeldID != "" || env.queuedID != "" || env.usageResume || env.restartResume {
		return
	}
	rest, ok := platform.ParseLimitBreak(env.Text)
	if !ok || rest == "" {
		return
	}
	env.Text = rest
	env.usageForce = true
}

// keepForcedOnly returns the members of batch that may run in a limit-break
// batch now: all of them when the usage hold does not hold user messages,
// else only the limit-break messages, the rest being parked on the session's
// held list as the worker would park them. Worker-confined.
func (a *Agent) keepForcedOnly(inb *sessionInbox, batch []Envelope) []Envelope {
	if len(batch) == 0 || !a.usageHoldsTrigger(inb.sk, "user", false) {
		return batch
	}
	kept := batch[:0:0]
	for _, env := range batch {
		if env.usageForce {
			kept = append(kept, env)
			continue
		}
		a.parkUsageHeld(inb, env)
	}
	return kept
}

// inboxForcing reports whether key's inbox worker runs a limit-break batch.
func (a *Agent) inboxForcing(key string) bool {
	a.inboxesMu.Lock()
	inb, ok := a.inboxes[key]
	a.inboxesMu.Unlock()
	return ok && inb.usageForcing.Load()
}

// LimitBreakOutcome is what a bare /lb did (LimitBreakHeld).
type LimitBreakOutcome int

const (
	// LimitBreakNoHold: the usage hold does not hold user messages now;
	// nothing was done.
	LimitBreakNoHold LimitBreakOutcome = iota
	// LimitBreakNothingHeld: the hold is on but holds no user message of
	// the session; nothing was done.
	LimitBreakNothingHeld
	// LimitBreakReleased: the session's held user messages run now.
	LimitBreakReleased
)

// LimitBreakHeld is a bare /lb in session sk (#3182; Dick 2026-11-19 13:40:
// "should send *every* held message in the current session"): every user
// message the usage hold holds there runs now, past the hold, oldest first,
// as if each had been typed with /lb in front. The hold stays on for
// everything else; automated sends and the hidden resume line of a turn the
// hold cut stay held. A held message is in one of two places:
//   - parked on the held list (the sender saw "Held"); or
//   - still on the worker's queue, because the worker is busy with a turn
//     the hold lets through (a remind wake): the worker parks it only when it
//     takes it. Enqueue's hold branch records the newest such message's seq
//     (heldQueuedSeq); user messages leave the queue in order, so the ones
//     numbered above userTakenThrough are still queued.
//
// The parked ones are taken off the list and their stored records and become
// limit-break messages. While a turn runs with nothing still queued, each is
// enqueued again, oldest first, and takes the normal routing, so it steers
// into the running work turn as "/lb <text>" does. Otherwise they go to the
// head of the worker's queue, and any still queued (newer than every parked
// one) are marked (usageForceThrough, applied by takeUser): the worker runs
// them all in arrival order as one turn, now when idle, or right after the
// running turn.
func (a *Agent) LimitBreakHeld(sk string) LimitBreakOutcome {
	if !a.usageHoldsTrigger(sk, "user", false) {
		return LimitBreakNoHold
	}
	inb := a.lookupInbox(sk)
	if inb == nil {
		return LimitBreakNothingHeld
	}
	// A running turn (a remind wake the hold lets through) takes the
	// released messages as steers, as "/lb <text>" would; an idle worker
	// runs them as one turn from the head of its queue.
	steer := a.sessionTurnActive(inb, sk)
	inb.usageHeldMu.Lock()
	var taken []Envelope
	kept := inb.usageHeld[:0:0]
	for _, h := range inb.usageHeld {
		if h.usageResume || h.Automated {
			kept = append(kept, h)
			continue
		}
		taken = append(taken, h)
	}
	if len(taken) > 0 {
		inb.usageHeld = kept
	}
	q := inb.heldQueuedSeq.Load()
	queued := q > inb.userTakenThrough && q > inb.usageForceThrough
	if queued {
		inb.usageForceThrough = q
	}
	stored := slices.Clone(taken) // their records go after the unlock
	for i := range taken {
		taken[i].usageHeldID, taken[i].queuedID, taken[i].enqSeq = "", "", 0
		taken[i].usageForce = true
	}
	viaQueue := queued || !steer
	if viaQueue && len(taken) > 0 {
		// Under the lock, so the worker cannot take a marked queued message
		// before the older parked ones are ahead of it.
		inb.unshiftPending(taken...)
	}
	inb.usageHeldMu.Unlock()
	a.forgetUsageHeld(stored...)
	if len(taken) == 0 && !queued {
		return LimitBreakNothingHeld
	}
	a.logger().Infof("inbox: bare /lb runs %d parked held message(s) past the usage hold sk=%s (queued ones marked: %v)", len(taken), sk, queued)
	// The worker owns the mirror (it also counts parked injections): kick
	// it to recount, and to run what was put on its queue.
	select {
	case inb.usageKick <- struct{}{}:
	default:
	}
	if !viaQueue {
		for _, env := range taken {
			if !a.Enqueue(env) {
				a.logger().Warnf("inbox: bare /lb could not enqueue a held message sk=%s (%dB)", sk, len(env.Text))
			}
		}
	}
	return LimitBreakReleased
}

// takeUser records that the worker took user message env off its queue and,
// when a bare /lb marked it while it waited there (LimitBreakHeld), makes it
// a limit-break message. Every point where a user message leaves the queue
// calls it. Worker-confined.
func (inb *sessionInbox) takeUser(env *Envelope) {
	if env.Inject != nil || env.enqSeq == 0 {
		return
	}
	inb.usageHeldMu.Lock()
	defer inb.usageHeldMu.Unlock()
	if env.enqSeq > inb.userTakenThrough {
		inb.userTakenThrough = env.enqSeq
	}
	if !env.Automated && env.enqSeq <= inb.usageForceThrough {
		env.usageForce = true
	}
}

// usageHeldLen is how many envelopes the usage hold has parked on inb.
func (inb *sessionInbox) usageHeldLen() int {
	inb.usageHeldMu.Lock()
	defer inb.usageHeldMu.Unlock()
	return len(inb.usageHeld)
}

// usageHeldHeadSeq is the enqSeq of the first parked envelope, if any.
func (inb *sessionInbox) usageHeldHeadSeq() (uint64, bool) {
	inb.usageHeldMu.Lock()
	defer inb.usageHeldMu.Unlock()
	if len(inb.usageHeld) == 0 {
		return 0, false
	}
	return inb.usageHeld[0].enqSeq, true
}
