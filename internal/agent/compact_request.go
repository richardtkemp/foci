// compact_request.go — compaction the agent asks for itself (#3040).
//
// The compact_session tool records a request on the session; the compaction
// runs in the post-turn phase of the turn that made it, after that turn's
// final reply (Dick 2026-11-10: "After the turn."). It runs while subagents
// or background commands run ("can compact while subagents run, why not?"):
// it skips the #1539 refusal, but the post-compaction CC restart is still
// deferred until that work finishes, so nothing is killed. Every backend:
// the hook sits in both transports' RunCompaction.
//
// A request can carry a wake message (#3041, Dick 2026-11-10): once the
// compaction finishes, the message starts a turn in the same session, the
// same way as a foci_remind wake (trigger "scheduled_wake"), so the agent
// continues on the compacted context without arming a wake of its own. The
// wake is injected whatever the outcome (compacted, declined by the backend,
// failed, or dropped because no_compact was set since the request), with a
// line saying which: the agent asked to be woken, and a lost wake stalls the
// work it was chaining.

package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"foci/internal/delegator"
	"foci/shared/prompts"
)

// compactionWakeTrigger is the trigger of the turn a compaction wake starts:
// the same as a foci_remind wake (via=wake, Medium dispatch tier, runs during
// a usage hold).
const compactionWakeTrigger = "scheduled_wake"

// RequestCompaction records that the agent wants sessionKey compacted once
// its current turn ends. wakeMessage, when not empty, is injected into the
// session as a wake turn once that compaction finishes (#3041). A second
// request before then is the same request; a second wake message replaces
// the first, and a second request without one keeps it. The request lives in
// memory only: a restart or a session reset drops it, and its wake with it.
func (a *Agent) RequestCompaction(sessionKey, wakeMessage string) error {
	if a.Compactor == nil && a.DelegatedManager == nil {
		return fmt.Errorf("compaction is not configured for this agent")
	}
	if sessionKey == "" {
		return fmt.Errorf("no session to compact")
	}
	// A no_compact session refuses the agent's request (Dick 2026-11-10,
	// Q64), unlike the user's /compact, which ignores the flag.
	if a.SessionNoCompact(sessionKey) {
		return fmt.Errorf("this session has compaction turned off (no_compact)")
	}
	if wakeMessage != "" && a.AsyncNotifier == nil {
		return fmt.Errorf("a wake message is not supported for this agent (no injection path)")
	}
	a.setMetaLocked(sessionKey, func(sm *sessionMeta) {
		sm.compactRequest = true
		if wakeMessage != "" {
			sm.compactWake = wakeMessage
		}
	})
	if wakeMessage != "" {
		a.logger().Infof("session=%s compaction requested by the agent: runs after this turn, then wakes the session", sessionKey)
	} else {
		a.logger().Infof("session=%s compaction requested by the agent: runs after this turn", sessionKey)
	}
	return nil
}

// CompactionRequested reports whether sessionKey has a compaction request
// waiting for its turn to end.
func (a *Agent) CompactionRequested(sessionKey string) bool {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	return sm.compactRequest
}

// takeCompactionRequest clears sessionKey's request and reports whether
// there was one, with its wake message ("" = none).
func (a *Agent) takeCompactionRequest(sessionKey string) (bool, string) {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	req, wake := sm.compactRequest, sm.compactWake
	sm.compactRequest = false
	sm.compactWake = ""
	return req, wake
}

// runRequestedCompaction runs the session's requested compaction, if any,
// from a transport's RunCompaction, and reports whether it took one (the
// caller then skips its threshold check: the context was just compacted, or
// the compaction failed and the next turn's check decides again).
//
// Callers reach it only for a turn that really ended (FinalUsage set): a
// steered follow-up's post-turn runs while the backend's run continues, and
// the request waits for that run's own end.
func (a *Agent) runRequestedCompaction(ts *TurnState) bool {
	req, wake := a.takeCompactionRequest(ts.SessionKey)
	if !req {
		return false
	}
	// no_compact set after the request was made: drop it (Q64).
	if a.SessionNoCompact(ts.SessionKey) {
		a.logger().Infof("session=%s agent-requested compaction dropped: no_compact set since the request", ts.SessionKey)
		a.injectCompactionWake(ts.SessionKey, wake, "Compaction did not run: compaction was turned off for this session (no_compact) after you asked for it. Your context is unchanged.")
		return false
	}
	// ts.Ctx is done once the turn has returned; keep its values (the cost
	// booking) without its cancellation.
	ctx := context.Background()
	if ts.Ctx != nil {
		ctx = context.WithoutCancel(ts.Ctx)
	}
	if wake != "" {
		ctx = withCompactionWake(ctx)
	}
	a.logger().Infof("session=%s running the agent-requested compaction", ts.SessionKey)
	_, err := a.compactSession(ctx, ts.SessionKey, false, compactDeferRestart)
	var status string
	switch {
	case errors.Is(err, delegator.ErrCompactionNoBoundary):
		a.logger().Infof("session=%s agent-requested compaction declined by backend (no boundary)", ts.SessionKey)
		status = "Compaction did not run: the backend found nothing to compact. Your context is unchanged."
	case err != nil:
		a.logger().Warnf("session=%s agent-requested compaction failed: %v", ts.SessionKey, err)
		status = fmt.Sprintf("Compaction failed: %v. Your context may be unchanged.", err)
	default:
		if ts.SessionMeta != nil {
			a.resetAfterCompaction(ts.SessionMeta)
		}
		status = "Compaction finished: your context is now the summary."
	}
	a.injectCompactionWake(ts.SessionKey, wake, status)
	return true
}

// injectCompactionWake starts the wake turn a compact_session request asked
// for (#3041): message, under a line giving the compaction's outcome, goes to
// sessionKey as a wake. No-op for an empty message. It enqueues on the
// session's inbox, so the turn runs after the current one has finished.
func (a *Agent) injectCompactionWake(sessionKey, message, status string) {
	if message == "" {
		return
	}
	if a.AsyncNotifier == nil {
		// RequestCompaction refuses a wake message without a notifier; this
		// guards a notifier removed since.
		a.logger().Warnf("session=%s compaction wake dropped: no injection path", sessionKey)
		return
	}
	a.logger().Infof("session=%s injecting the compaction wake", sessionKey)
	body := status + "\n\n" + message
	a.AsyncNotifier.InjectToAgent(sessionKey,
		prompts.FormatInjectedMessage("COMPACTION WAKE", time.Now(), body), "", compactionWakeTrigger)
}

// compactionWakeKey marks a compaction context whose request carries a wake
// message: the wake restarts the agent, so the compaction-resume nudge is not
// sent as well (it would run a second turn).
type compactionWakeKey struct{}

func withCompactionWake(ctx context.Context) context.Context {
	return context.WithValue(ctx, compactionWakeKey{}, true)
}

func compactionWakeFollows(ctx context.Context) bool {
	v, _ := ctx.Value(compactionWakeKey{}).(bool)
	return v
}
