// inbox_work_turns.go — queued injections share one turn, and /stop reaches an
// injection's turn (#3120).
//
// Dick 2026-11-15 (Q60, "Batch, build now"): consecutive queued system
// injections run as ONE turn, keeping arrival order relative to user messages
// (Q58, "keep arrival order"). Before this, each queued injection ran a turn of
// its own when the worker reached it: a backlog of ~25 spawn results and
// inter-session replies took ~25 turns of 2-4 s each, all ahead of the user's
// next message.
//
// The worker runs the head injection as before. The moment that injection's
// turn begins (beginTurn calls foldQueuedInjects), the messages within the
// agent queued directly behind it (SkipHolds, with fold text) are steered into
// that turn, in order, the same way one arriving mid-turn is (steerPeerInject).
// The first entry that cannot fold ends the walk: a user message, any other
// injection, or a message the turn refuses (a protected turn, #2591; a usage
// hold; a backend without native mid-turn steer, Q45). Only entries queued
// AHEAD of everything that stays queued fold, so nothing runs before a message
// that arrived earlier than it.
//
// Dick 2026-11-14 (Q59, "Stop any turn"): /stop interrupts whatever turn runs,
// system turns included. runInject registers the injection's turn with the
// inbox (injectCancel), so CancelSession reaches it; bindInjectCancel makes an
// API-transport turn listen. A delegated turn is stopped by its backend's
// interrupt (DelegatedManager.StopSession), which /stop sends first.

package agent

import (
	"context"
	"time"
)

// peekPending returns the head of the worker's queue without removing it.
func (inb *sessionInbox) peekPending() (Envelope, bool) {
	inb.pendMu.Lock()
	defer inb.pendMu.Unlock()
	if len(inb.pending) == 0 {
		return Envelope{}, false
	}
	return inb.pending[0], true
}

// foldsIntoRunningInject reports whether a queued envelope may fold into the
// injection turn that just began: a message within the agent or an async tool
// result with text to fold (#3114 SkipHolds), not pinned behind an earlier one
// that a hold parked (heldSiblingPins keeps their FIFO).
func foldsIntoRunningInject(inb *sessionInbox, env Envelope) bool {
	return env.Inject != nil && env.Inject.SkipHolds && env.Text != "" && !heldSiblingPins(inb.held, env)
}

// foldQueuedInjects folds the messages queued directly behind the injection
// whose turn just began on sk into that turn (#3120 Q60). It acts once per
// injection (foldArmed) and only while the inbox worker runs one, so a user
// batch's turn never triggers it. It runs on the worker goroutine: runInject
// calls the injection's Run, whose turn begins inside it.
func (a *Agent) foldQueuedInjects(sk string) {
	inb := a.lookupInbox(sk)
	if inb == nil || !inb.foldArmed.CompareAndSwap(true, false) {
		return
	}
	// What the channel holds joins the queue first, in order, as at the
	// worker's loop top.
	inb.pushPending(inb.drainAvailable()...)
	folded := 0
	for {
		env, ok := inb.peekPending()
		if !ok || !foldsIntoRunningInject(inb, env) || !a.steerPeerInject(inb, env) {
			break
		}
		inb.popPending()
		// One a usage hold parked and stored leaves the restart store, as
		// runInject takes it off when it runs (#2491).
		a.forgetUsageHeld(env)
		folded++
	}
	if folded > 0 {
		a.logger().Infof("inbox: folded %d queued injection(s) into the injection turn that just began sk=%s (#3120)", folded, sk)
	}
}

// bindInjectCancel returns ctx made cancellable by /stop when sk's inbox worker
// is running an injection's turn (#3120 Q59), and the release to defer. An
// API-transport turn ends on its ctx; a delegated one ends on its backend's
// interrupt, so HandleMessage binds only the API transport. Unchanged ctx and
// a no-op release otherwise.
func (a *Agent) bindInjectCancel(ctx context.Context, sk string) (context.Context, func()) {
	inb := a.lookupInbox(sk)
	if inb == nil {
		return ctx, func() {}
	}
	inb.cancelMu.Lock()
	injCtx := inb.injectCtx
	inb.cancelMu.Unlock()
	if injCtx == nil {
		return ctx, func() {}
	}
	bound, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(injCtx, cancel)
	return bound, func() {
		stop()
		cancel()
	}
}

// stopPollInterval is how often WaitTurnStopped re-reads the backend.
const stopPollInterval = 25 * time.Millisecond

// WaitTurnStopped reports whether the turn that ran on sessionKey when /stop
// began has ended, waiting up to timeout (#3120 Q59: the "Stopped." reply must
// be true). turnID is that turn's LiveTurnID ("" when foci did not record
// one). The turn has ended once the backend has no turn in flight, or once
// the live turn is no longer turnID: a queued turn that began right after the
// stopped one is a different turn. Always true for an agent without a
// delegated backend, whose turn ends on its cancelled ctx.
func (a *Agent) WaitTurnStopped(ctx context.Context, sessionKey, turnID string, timeout time.Duration) bool {
	if a.DelegatedManager == nil {
		return true
	}
	ended := func() bool {
		if !a.DelegatedManager.BackendTurnInFlight(sessionKey) {
			return true
		}
		return turnID != "" && a.LiveTurnID(sessionKey) != turnID
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(stopPollInterval)
	defer tick.Stop()
	for !ended() {
		select {
		case <-ctx.Done():
			return ended()
		case <-deadline.C:
			return ended()
		case <-tick.C:
		}
	}
	return true
}
