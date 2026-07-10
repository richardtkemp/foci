package main

import (
	"context"
	"fmt"
	"time"

	"foci/internal/agent"
	"foci/internal/log"
	"foci/internal/periodic"
	"foci/internal/session"
)

// BranchDoneFunc is called after a branch session completes, receiving the
// branch type and the branch session key.
type BranchDoneFunc func(branchType, branchKey string)

// buildBranchFunc returns a function that creates a branch session from a
// caller-specified parent session and runs a single turn with the given prompt.
// If onDone is non-nil, it is called after the turn completes with the
// branch type and branch session key.
//
// For delegated agents (CC), branching is handled differently — the strategy
// comes from the single authority, agent.BranchStrategyFor:
//   - BranchInPlace (reflection, keepalive) → inject into the existing CC session
//   - BranchIndependent (background, …)     → spin up a new independent CC session
func buildBranchFunc(
	agentID string,
	ag *agent.Agent,
	sessions *session.Store,
	orientTemplate string,
	ctx context.Context,
	onDone BranchDoneFunc,
) periodic.BranchFunc {
	return func(branchType, parentKey, promptText string, noCompact bool) bool {
		if parentKey == "" {
			log.Warnf(branchType, "[%s] no parent session, skipping", agentID)
			return false
		}

		// Delegated agents: the strategy (BranchStrategyFor) decides between a
		// real backend fork, an in-place inject, or a fresh independent session.
		if ag.DelegatedManager != nil {
			return handleDelegatedBranch(ag, agentID, branchType, parentKey, promptText, orientTemplate, ctx)
		}

		// API agents: create a branch session as before.
		branchKey, err := sessions.CreateBranchWithOptions(parentKey, session.BranchOptions{
			NoResetHook:         true,
			BranchType:          branchType,
			OrientationTemplate: orientTemplate,
		})
		if err != nil {
			log.Errorf(branchType, "[%s] branch error: %v", agentID, err)
			return false
		}
		ag.TouchRootCacheForBranch(branchKey) // branching warms root's shared prefix once

		turnCtx := agent.WithTrigger(ctx, branchType)
		if noCompact {
			ag.SetSessionNoCompact(branchKey, true)
		}

		if err := ag.HandleMessage(turnCtx, branchKey, []string{promptText}, nil); err != nil {
			log.Warnf(branchType, "[%s] session=%s turn error: %v", agentID, branchKey, err)
			return false
		}

		if onDone != nil {
			onDone(branchType, branchKey)
		}
		return true
	}
}

// handleDelegatedBranch handles branch operations for delegated (CC) agents,
// dispatching on the strategy from the single authority, agent.BranchStrategyFor:
//   - BranchForkBackend → real backend fork (clone), run the turn on the branch
//     key; falls back to in-place inject if the fork can't be performed.
//   - BranchInPlace     → inject one more turn into the existing session.
//   - BranchIndependent → spin up a fresh, isolated session and reset it after.
func handleDelegatedBranch(ag *agent.Agent, agentID, branchType, parentKey, promptText, orientTemplate string, ctx context.Context) bool {
	turnCtx := agent.WithTrigger(ctx, branchType)

	// injectInPlace runs one turn on the parent's existing session. Used both
	// for the BranchInPlace strategy and as the BranchForkBackend fallback.
	// Routed through the session's inbox worker: these are system turns and must
	// serialise with (never steer) any in-flight platform turn. EnqueueInjectWait
	// blocks until the turn has run, preserving the scheduler's completion
	// semantics.
	injectInPlace := func() bool {
		log.Infof(branchType, "[%s] delegated: injecting into main session %s", agentID, parentKey)
		var turnErr error
		if err := ag.EnqueueInjectWait(ctx, parentKey, branchType, func() {
			turnErr = ag.HandleMessage(turnCtx, parentKey, []string{promptText}, nil)
		}); err != nil {
			log.Warnf(branchType, "[%s] session=%s inject error: %v", agentID, parentKey, err)
			return false
		}
		if turnErr != nil {
			log.Warnf(branchType, "[%s] session=%s turn error: %v", agentID, parentKey, turnErr)
			return false
		}
		return true
	}

	switch ag.BranchStrategyFor(branchType) {
	case agent.BranchInPlace:
		return injectInPlace()

	case agent.BranchForkBackend:
		// Clone the parent transcript with the parent held quiescent: run the
		// fork inside the parent's inbox worker (EnqueueInjectWait) so no turn
		// writes the transcript mid-copy. Only the clone is under the lock; the
		// branch turn below runs on the isolated branch key.
		var branchKey string
		var ok bool
		if err := ag.EnqueueInjectWait(ctx, parentKey, branchType+"-fork", func() {
			branchKey, ok = ag.ForkBackendBranch(ctx, parentKey, session.BranchOptions{
				NoResetHook:         true,
				BranchType:          branchType,
				OrientationTemplate: orientTemplate,
			})
		}); err != nil {
			log.Warnf(branchType, "[%s] session=%s fork-lock error: %v", agentID, parentKey, err)
			return false
		}
		if !ok {
			// No parent backend session to fork yet — run in-place instead.
			log.Infof(branchType, "[%s] delegated: fork unavailable, injecting in-place", agentID)
			return injectInPlace()
		}
		ag.SetSessionNoCompact(branchKey, true)
		log.Infof(branchType, "[%s] delegated: backend fork %s from %s", agentID, branchKey, parentKey)
		// These internal passes (reflection/keepalive/background/…) are one-offs:
		// close the forked backend after the turn so its CC process doesn't leak
		// until the idle reaper. The parent session is untouched.
		defer ag.DelegatedManager.ResetSession(branchKey)
		if err := ag.HandleMessage(turnCtx, branchKey, []string{promptText}, nil); err != nil {
			log.Warnf(branchType, "[%s] session=%s turn error: %v", agentID, branchKey, err)
			return false
		}
		return true

	default: // BranchIndependent
		// New independent CC session for isolated work. Fresh key — nothing can
		// be in flight on it, so the turn runs directly.
		sessionKey := session.SessionKey{
			AgentID: agentID,
			Type:    'i',
			ID:      fmt.Sprintf("%s-%d", branchType, time.Now().Unix()),
		}.String()
		log.Infof(branchType, "[%s] delegated: new session %s", agentID, sessionKey)

		if err := ag.HandleMessage(turnCtx, sessionKey, []string{promptText}, nil); err != nil {
			log.Warnf(branchType, "[%s] session=%s turn error: %v", agentID, sessionKey, err)
			return false
		}

		// Close independent CC sessions after the turn completes. Without this,
		// the backend process leaks until the idle reaper (24h default).
		ag.DelegatedManager.ResetSession(sessionKey)
		return true
	}
}
