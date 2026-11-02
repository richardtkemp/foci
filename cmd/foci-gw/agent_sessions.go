package main

import (
	"context"
	"errors"
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
//   - RunInPlace (reflection, keepalive) → inject into the existing CC session
//   - RunIndependent (background, …)     → spin up a new independent CC session
func buildBranchFunc(
	agentID string,
	ag *agent.Agent,
	sessions *session.Store,
	orientTemplate string,
	ctx context.Context,
	onDone BranchDoneFunc,
) periodic.BranchFunc {
	return func(callCtx context.Context, branchType, parentKey, promptText string, noCompact bool) bool {
		if callCtx == nil {
			callCtx = context.Background()
		}
		if parentKey == "" {
			log.NewComponentLogger(branchType).Warnf("[%s] no parent session, skipping", agentID)
			return false
		}

		// Delegated agents: the strategy (BranchStrategyFor) decides between a
		// real backend fork, an in-place inject, or a fresh independent session.
		if ag.DelegatedManager != nil {
			return handleDelegatedBranch(callCtx, ag, agentID, branchType, parentKey, promptText, orientTemplate, ctx)
		}

		// API agents: create a branch session as before.
		branchKey, err := sessions.CreateBranchWithOptions(parentKey, session.BranchOptions{
			NoResetHook:         true,
			BranchType:          branchType,
			OrientationTemplate: orientTemplate,
		})
		if err != nil {
			log.NewComponentLogger(branchType).Errorf("[%s] branch error: %v", agentID, err)
			return false
		}
		ag.TouchRootCacheForBranch(branchKey) // branching warms root's shared prefix once

		// The API turn's context also ends when the caller's does (a
		// keepalive timeout, #2489), which cancels the model call.
		apiCtx, stop := boundToCaller(ctx, callCtx)
		defer stop()
		turnCtx := agent.WithTrigger(apiCtx, branchType)
		if noCompact {
			ag.SetSessionNoCompact(branchKey, true)
		}

		if err := ag.HandleMessage(turnCtx, branchKey, []string{promptText}, nil); err != nil {
			log.NewComponentLogger(branchType).Warnf("[%s] session=%s turn error: %v", agentID, branchKey, err)
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
//   - RunInPlace     → inject one more turn into the existing session.
//   - RunIndependent → spin up a fresh, isolated session and reset it after.
//
// callCtx bounds the turn (#2489): when it ends first, the backend turn on the
// session the branch runs in is interrupted, and an inject still waiting in
// the inbox does not start.
func handleDelegatedBranch(callCtx context.Context, ag *agent.Agent, agentID, branchType, parentKey, promptText, orientTemplate string, ctx context.Context) bool {
	turnCtx := agent.WithTrigger(ctx, branchType)
	// interruptOnCallerDone stops the backend turn on key when callCtx ends
	// before the returned stop func runs.
	interruptOnCallerDone := func(key string) (stop func() bool) {
		return context.AfterFunc(callCtx, func() {
			log.NewComponentLogger(branchType).Warnf("[%s] session=%s %s cancelled (%v): interrupting its turn", agentID, key, branchType, context.Cause(callCtx))
			if err := ag.DelegatedManager.StopSession(context.Background(), key); err != nil {
				log.NewComponentLogger(branchType).Infof("[%s] session=%s interrupt: %v", agentID, key, err)
			}
		})
	}
	waitCtx, stopWait := boundToCaller(ctx, callCtx)
	defer stopWait()

	// injectInPlace runs one turn on the parent's existing session. Used both
	// for the RunInPlace strategy and as the BranchForkBackend fallback.
	// Routed through the session's inbox worker: these are system turns and must
	// serialise with (never steer) any in-flight platform turn. EnqueueInjectWait
	// blocks until the turn has run, preserving the scheduler's completion
	// semantics.
	injectInPlace := func() bool {
		log.NewComponentLogger(branchType).Infof("[%s] delegated: injecting into main session %s", agentID, parentKey)
		var turnErr error
		if err := ag.EnqueueInjectWait(waitCtx, parentKey, branchType, func() {
			if callCtx.Err() != nil {
				turnErr = context.Cause(callCtx) // the caller gave up while this waited
				return
			}
			stop := interruptOnCallerDone(parentKey)
			defer stop()
			turnErr = ag.HandleMessage(turnCtx, parentKey, []string{promptText}, nil)
		}); err != nil {
			log.NewComponentLogger(branchType).Warnf("[%s] session=%s inject error: %v", agentID, parentKey, err)
			return false
		}
		if errors.Is(turnErr, agent.ErrKeepaliveSkipped) {
			// A turn was in flight, so the cache is already warm (#2233 N1).
			log.NewComponentLogger(branchType).Infof("[%s] session=%s %v", agentID, parentKey, turnErr)
			return true
		}
		if turnErr != nil {
			log.NewComponentLogger(branchType).Warnf("[%s] session=%s turn error: %v", agentID, parentKey, turnErr)
			return false
		}
		return true
	}

	switch ag.BranchStrategyFor(branchType) {
	case agent.RunInPlace:
		return injectInPlace()

	case agent.BranchForkBackend:
		branchKey, ok, err := ag.ForkSession(ctx, parentKey, session.BranchOptions{
			NoResetHook:         true,
			BranchType:          branchType,
			OrientationTemplate: orientTemplate,
		})
		if err != nil {
			log.NewComponentLogger(branchType).Warnf("[%s] session=%s fork error: %v", agentID, parentKey, err)
			return false
		}
		if !ok {
			// No parent backend session to fork yet — run in-place instead.
			log.NewComponentLogger(branchType).Infof("[%s] delegated: fork unavailable, injecting in-place", agentID)
			return injectInPlace()
		}
		ag.SetSessionNoCompact(branchKey, true)
		log.NewComponentLogger(branchType).Infof("[%s] delegated: backend fork %s from %s", agentID, branchKey, parentKey)
		// These internal passes (reflection/keepalive/background/…) are one-offs:
		// close the forked backend after the turn so its CC process doesn't leak
		// until the idle reaper. The cloned transcript file is left for the daily
		// ephemeral-session GC (it outlives this process). Parent is untouched.
		defer ag.DelegatedManager.ResetSession(branchKey)
		stop := interruptOnCallerDone(branchKey)
		defer stop()
		if err := ag.HandleMessage(turnCtx, branchKey, []string{promptText}, nil); err != nil {
			log.NewComponentLogger(branchType).Warnf("[%s] session=%s turn error: %v", agentID, branchKey, err)
			return false
		}
		return true

	default: // RunIndependent
		// New independent CC session for isolated work. Fresh key — nothing can
		// be in flight on it, so the turn runs directly.
		sessionKey := session.SessionKey{
			AgentID: agentID,
			Type:    'i',
			ID:      fmt.Sprintf("%s-%d", branchType, time.Now().Unix()),
		}.String()
		log.NewComponentLogger(branchType).Infof("[%s] delegated: new session %s", agentID, sessionKey)

		stop := interruptOnCallerDone(sessionKey)
		defer stop()
		if err := ag.HandleMessage(turnCtx, sessionKey, []string{promptText}, nil); err != nil {
			log.NewComponentLogger(branchType).Warnf("[%s] session=%s turn error: %v", agentID, sessionKey, err)
			return false
		}

		// Close independent CC sessions after the turn completes. Without this,
		// the backend process leaks until the idle reaper (agent.DefaultIdleTimeout,
		// 3h default).
		ag.DelegatedManager.ResetSession(sessionKey)
		return true
	}
}

// boundToCaller returns a child of base that also ends when caller does, and
// the func that releases it. base carries the gateway's lifetime and values;
// caller carries a periodic task's own bound (a keepalive timeout, #2489).
func boundToCaller(base, caller context.Context) (context.Context, func()) {
	c, cancel := context.WithCancelCause(base)
	stopAfter := context.AfterFunc(caller, func() { cancel(context.Cause(caller)) })
	return c, func() {
		stopAfter()
		cancel(nil)
	}
}
