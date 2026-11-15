package agent

import "foci/internal/platform"

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
// lists it and answers a bare /lb with its usage.

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
	if len(batch) == 0 || !a.usageHoldsTrigger("user", false) {
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
