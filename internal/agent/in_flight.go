package agent

import (
	"fmt"
	"time"

	"foci/internal/session"
)

// sessionMetaLastActivity is the session_metadata key holding the unix
// timestamp of the most recent turn executed under this session base,
// regardless of trigger (user, cron, CLI, webhook, agent-to-agent,
// system-injected). Written from OrchestrateFullTurn so every turn-init
// path participates uniformly. Used by --if-active / --if-inactive gates
// to answer "has this session been doing anything recently?" (TODO #753).
//
// Stored against SessionKeyBase(sessionKey) — the {agentID}/{type}{id}
// prefix that is stable across compaction (version rotation) and branching.
// CLI sends target the agent's *main* session, so the gate consults that
// session's row; activity in branches or sub-agents does not falsely keep
// the main session "warm" for keepalive purposes.
//
// Distinct from "last_user_activity" (still in agent_metadata, written by
// internal/telegram/bot_receive.go and internal/discord/receive.go) which
// tracks primary-bot user messages only — that key is deliberately untouched
// to preserve the user-attention gate's "cron cannot defeat itself" property.
const sessionMetaLastActivity = "last_activity"

// IsTurnInFlight returns true if any turn is currently executing under
// OrchestrateFullTurn for the in-flight identity of the given session key —
// covers both API and delegated transports. Distinct from IsProcessing, which
// only reflects the API path's internal counter and is per-agent rather than
// per-session.
//
// Pass the FULL session key; the identity is derived via
// session.SessionInFlightKey, which collapses version rotation (compaction)
// but PRESERVES the child suffix. So a root session and its post-compaction
// versions share one identity, while a facet/branch (a 'b' child on its own
// backend) tracks separately — collapsing a facet onto the parent would wrongly
// couple two independent conversations (TODO #719). Root-injected periodic
// turns (reflection/keepalive/memory) run under the parent key with no child,
// so they still register under the root identity as the #760/#767 gates expect.
//
// This is the runtime signal used by the activity gate to short-circuit
// keepalive sends while a turn is mid-flight (e.g. blocked waiting for a
// permission decision in the delegated path).
func (a *Agent) IsTurnInFlight(key string) bool {
	base := session.SessionInFlightKey(key)
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	return a.inFlight[base] > 0
}

// IsAnyTurnInFlight reports whether any session under this agent currently has
// a turn executing. This is the one place that genuinely needs an agent-wide
// aggregate rather than a per-session check: graceful shutdown drains all
// in-flight work before exit. Everything else should ask IsTurnInFlight(base).
func (a *Agent) IsAnyTurnInFlight() bool {
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	for _, n := range a.inFlight {
		if n > 0 {
			return true
		}
	}
	return false
}

// SetTurnInFlightForTest marks a session's base as in-flight (inFlight=true) or
// clears it, so tests can exercise the in-flight guards without driving a real
// turn. Test-only.
func (a *Agent) SetTurnInFlightForTest(sessionKey string, inFlight bool) {
	base := session.SessionInFlightKey(sessionKey)
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	if a.inFlight == nil {
		a.inFlight = make(map[string]int32)
	}
	if inFlight {
		a.inFlight[base] = 1
	} else {
		delete(a.inFlight, base)
	}
	a.notifyInFlightChangedLocked(base)
}

// IsInFlightDelivering returns true if at least one in-flight turn under base
// has a sink that reports DeliversToPlatform=true (i.e. routes output to a
// user-facing platform). Kept distinct from IsTurnInFlight so that combining
// logic — "in flight AND NOT delivering = block before dispatch" — is visible
// at the call site rather than hidden in a single overloaded predicate.
//
// Counts mirror inFlight: a turn is delivering iff its sink reports true at
// markInFlight time; the bookkeeping is exact across nested/concurrent turns
// on the same base.
func (a *Agent) IsInFlightDelivering(key string) bool {
	base := session.SessionInFlightKey(key)
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	return a.inFlightDelivering[base] > 0
}

// InFlightWaitCh returns a channel that closes the next time the in-flight
// state for base changes (any markInFlight increment or decrement closure
// for that base). Callers re-check state after the channel closes — the
// channel signals "something changed," not "your wait condition is now met."
//
// The channel is created lazily on first call per base and replaced with a
// fresh open channel after each close, so a single waiter can loop:
//
//	for a.IsTurnInFlight(base) && !a.IsInFlightDelivering(base) {
//	    wait := a.InFlightWaitCh(base)
//	    select {
//	    case <-ctx.Done(): return
//	    case <-wait:       // state changed, re-check
//	    }
//	}
//
// Safe under concurrent waiters and concurrent state changes: close-and-replace
// happens under inFlightMu, so every waiter that fetched the channel before
// the change observes the close.
func (a *Agent) InFlightWaitCh(key string) <-chan struct{} {
	base := session.SessionInFlightKey(key)
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	if a.inFlightChanged == nil {
		a.inFlightChanged = make(map[string]chan struct{})
	}
	ch, ok := a.inFlightChanged[base]
	if !ok {
		ch = make(chan struct{})
		a.inFlightChanged[base] = ch
	}
	return ch
}

// notifyInFlightChangedLocked closes base's change-channel and installs a
// fresh open channel for future waiters. Must be called with inFlightMu held.
// Safe to call when no channel has been created yet (no-op).
func (a *Agent) notifyInFlightChangedLocked(base string) {
	if a.inFlightChanged == nil {
		return
	}
	ch, ok := a.inFlightChanged[base]
	if !ok {
		return
	}
	close(ch)
	a.inFlightChanged[base] = make(chan struct{})
}

// markInFlight increments the in-flight counter for the given session base
// and returns a one-shot decrement closure. The closure is safe to call
// exactly once; subsequent calls are no-ops via the local guard.
//
// delivering reports whether the turn's sink ultimately routes to a
// user-facing platform (see turnevent.Sink.DeliversToPlatform). A separate
// counter tracks delivering turns so callers can distinguish "any turn in
// flight" from "an in-flight turn whose output reaches the user."
//
// Pass the FULL session key — markInFlight derives the in-flight identity via
// session.SessionInFlightKey (child-preserving; see IsTurnInFlight).
//
// Usage at call sites:
//
//	delivering := turnevent.SinkFromContext(ctx).DeliversToPlatform()
//	done := a.markInFlight(ts.SessionKey, delivering)
//	defer done()
//
// The orchestrator pairs this with touchLastActivity at turn entry so that
// both signals — runtime "doing something now" and persistent "did something
// at time T" — track every turn-init path through the single chokepoint.
//
// Both increment and decrement notify InFlightWaitCh listeners by
// close-and-replace, so the inbox's wait loop wakes on any state change and
// re-evaluates its predicate.
func (a *Agent) markInFlight(key string, delivering bool) func() {
	base := session.SessionInFlightKey(key)
	a.inFlightMu.Lock()
	if a.inFlight == nil {
		a.inFlight = make(map[string]int32)
	}
	if a.inFlightDelivering == nil {
		a.inFlightDelivering = make(map[string]int32)
	}
	a.inFlight[base]++
	if delivering {
		a.inFlightDelivering[base]++
	}
	a.notifyInFlightChangedLocked(base)
	a.inFlightMu.Unlock()

	var done bool
	return func() {
		if done {
			return
		}
		done = true
		a.inFlightMu.Lock()
		defer a.inFlightMu.Unlock()
		if a.inFlight == nil {
			return
		}
		a.inFlight[base]--
		if a.inFlight[base] <= 0 {
			delete(a.inFlight, base)
		}
		if delivering {
			a.inFlightDelivering[base]--
			if a.inFlightDelivering[base] <= 0 {
				delete(a.inFlightDelivering, base)
			}
		}
		a.notifyInFlightChangedLocked(base)
	}
}

// touchLastActivity writes the current unix timestamp to the session base's
// last_activity metadata key, recording that this session is currently
// executing a turn. No-op if SessionIndex is nil (test agents) or base is
// empty.
//
// Errors are swallowed (logged at debug, not warn) — a transient DB write
// failure should not abort the turn. The next turn refreshes the timestamp.
func (a *Agent) touchLastActivity(base string) {
	if a.SessionIndex == nil || base == "" {
		return
	}
	val := fmt.Sprintf("%d", time.Now().Unix())
	if err := a.SessionIndex.SetSessionMetadata(base, sessionMetaLastActivity, val); err != nil {
		a.logger().Debugf("touchLastActivity: SetSessionMetadata(%s, %s, %s): %v",
			base, sessionMetaLastActivity, val, err)
	}
}
