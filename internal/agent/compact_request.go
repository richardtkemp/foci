// compact_request.go — compaction the agent asks for itself (#3040).
//
// The compact_session tool records a request on the session; the compaction
// runs in the post-turn phase of the turn that made it, after that turn's
// final reply (Dick 2026-11-10: "After the turn."). It runs while subagents
// or background commands run ("can compact while subagents run, why not?"):
// it skips the #1539 refusal, but the post-compaction CC restart is still
// deferred until that work finishes, so nothing is killed. Every backend:
// the hook sits in both transports' RunCompaction.

package agent

import (
	"context"
	"errors"
	"fmt"

	"foci/internal/delegator"
)

// RequestCompaction records that the agent wants sessionKey compacted once
// its current turn ends. A second request before then is the same request.
// The request lives in memory only: a restart or a session reset drops it.
func (a *Agent) RequestCompaction(sessionKey string) error {
	if a.Compactor == nil && a.DelegatedManager == nil {
		return fmt.Errorf("compaction is not configured for this agent")
	}
	if sessionKey == "" {
		return fmt.Errorf("no session to compact")
	}
	a.setMetaLocked(sessionKey, func(sm *sessionMeta) { sm.compactRequest = true })
	a.logger().Infof("session=%s compaction requested by the agent: runs after this turn", sessionKey)
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
// there was one.
func (a *Agent) takeCompactionRequest(sessionKey string) bool {
	sm := a.getSessionMeta(sessionKey)
	a.metaMu.Lock()
	defer a.metaMu.Unlock()
	req := sm.compactRequest
	sm.compactRequest = false
	return req
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
	if !a.takeCompactionRequest(ts.SessionKey) {
		return false
	}
	// ts.Ctx is done once the turn has returned; keep its values (the cost
	// booking) without its cancellation.
	ctx := context.Background()
	if ts.Ctx != nil {
		ctx = context.WithoutCancel(ts.Ctx)
	}
	a.logger().Infof("session=%s running the agent-requested compaction", ts.SessionKey)
	_, err := a.compactSession(ctx, ts.SessionKey, false, compactDeferRestart)
	switch {
	case errors.Is(err, delegator.ErrCompactionNoBoundary):
		a.logger().Infof("session=%s agent-requested compaction declined by backend (no boundary)", ts.SessionKey)
	case err != nil:
		a.logger().Warnf("session=%s agent-requested compaction failed: %v", ts.SessionKey, err)
	default:
		if ts.SessionMeta != nil {
			a.resetAfterCompaction(ts.SessionMeta)
		}
	}
	return true
}
