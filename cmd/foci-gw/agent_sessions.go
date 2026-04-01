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

// branchTypesForMainSession lists branch types that should be injected into
// the existing CC session rather than creating a new one. These need the
// parent's conversation context and are short-lived.
var branchTypesForMainSession = map[string]bool{
	"memory-formation":    true,
	"session-end-memory":  true,
	"compaction-memory":   true,
}

// buildBranchFunc returns a function that creates a branch session from a
// caller-specified parent session and runs a single turn with the given prompt.
// If onDone is non-nil, it is called after the turn completes with the
// branch type and branch session key.
//
// For delegated agents (CC), branching is handled differently:
//   - Types in branchTypesForMainSession → inject into existing CC session
//   - Other types → spin up a new CC session with an independent key
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

		// Delegated agents: CC can't branch. Either inject into the main
		// session or spin up a new independent CC session.
		if ag.DelegatedManager != nil {
			return handleDelegatedBranch(ag, agentID, branchType, parentKey, promptText, ctx)
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

		turnCtx := agent.WithTrigger(ctx, branchType)
		if noCompact {
			ag.SetSessionNoCompact(branchKey, true)
		}

		resp, err := ag.HandleMessage(turnCtx, branchKey, promptText)
		if err != nil {
			log.Warnf(branchType, "[%s] session=%s turn error: %v", agentID, branchKey, err)
			return false
		}
		_ = resp

		if onDone != nil {
			onDone(branchType, branchKey)
		}
		return true
	}
}

// handleDelegatedBranch handles branch operations for delegated (CC) agents.
// For types that need conversation context, injects into the existing session.
// For independent work, creates a new CC session.
func handleDelegatedBranch(ag *agent.Agent, agentID, branchType, parentKey, promptText string, ctx context.Context) bool {
	turnCtx := agent.WithTrigger(ctx, branchType)

	var sessionKey string
	if branchTypesForMainSession[branchType] {
		// Inject into existing CC session — it has the conversation context.
		sessionKey = parentKey
		log.Infof(branchType, "[%s] delegated: injecting into main session %s", agentID, sessionKey)
	} else {
		// New independent CC session for isolated work.
		sessionKey = session.SessionKey{
			AgentID:   agentID,
			Type:      'i',
			ID:        fmt.Sprintf("%s-%d", branchType, time.Now().Unix()),
			VersionTS: time.Now().Unix(),
		}.String()
		log.Infof(branchType, "[%s] delegated: new session %s", agentID, sessionKey)
	}

	_, err := ag.HandleMessage(turnCtx, sessionKey, promptText)
	if err != nil {
		log.Warnf(branchType, "[%s] session=%s turn error: %v", agentID, sessionKey, err)
		return false
	}

	// Wait for CC to complete the turn (HandleMessage returns immediately
	// for delegated agents).
	waitCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if err := ag.DelegatedManager.WaitForTurn(waitCtx, sessionKey); err != nil {
		log.Warnf(branchType, "[%s] session=%s wait error: %v", agentID, sessionKey, err)
		// Don't return false — the turn may still complete, we just can't wait.
	}

	return true
}
