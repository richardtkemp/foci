package main

import (
	"context"

	"foci/internal/agent"
	"foci/internal/log"
	"foci/internal/periodic"
	"foci/internal/session"
)

// BranchDoneFunc is called after a branch session completes, receiving the
// branch type and the branch session key.
type BranchDoneFunc func(branchType, branchKey string)

// buildBranchFunc returns a function that creates a branch session from the
// agent's default session and runs a single turn with the given prompt.
// If onDone is non-nil, it is called after the turn completes with the
// branch type and branch session key.
func buildBranchFunc(
	agentID string,
	ag *agent.Agent,
	sessions *session.Store,
	defaultSessionKey func() string,
	orientTemplate string,
	ctx context.Context,
	onDone BranchDoneFunc,
) periodic.BranchFunc {
	return func(branchType, promptText string, noCompact bool) {
		parentKey := defaultSessionKey()
		if parentKey == "" {
			log.Warnf(branchType, "[%s] no default session, skipping", agentID)
			return
		}

		branchKey, err := sessions.CreateBranchWithOptions(parentKey, session.BranchOptions{
			NoResetHook:         true,
			BranchType:          branchType,
			OrientationTemplate: orientTemplate,
		})
		if err != nil {
			log.Errorf(branchType, "[%s] branch error: %v", agentID, err)
			return
		}

		turnCtx := agent.WithTrigger(ctx, branchType)
		if noCompact {
			ag.SetSessionNoCompact(branchKey, true)
		}

		resp, err := ag.HandleMessage(turnCtx, branchKey, promptText)
		if err != nil {
			log.Warnf(branchType, "[%s] session=%s turn error: %v", agentID, branchKey, err)
			return
		}
		_ = resp

		if onDone != nil {
			onDone(branchType, branchKey)
		}
	}
}
