package main

import (
	"context"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/periodic"
	"foci/internal/session"
	"foci/prompts"
)

// buildBranchFunc returns a function that creates a branch session from the
// agent's default session and runs a single turn with the given prompt.
func buildBranchFunc(
	agentID string,
	ag *agent.Agent,
	sessions *session.Store,
	defaultSessionKey func() string,
	buildOrientation func(branchKey, parentKey, branchType string) string,
	ctx context.Context,
) periodic.BranchFunc {
	return func(branchType, promptText string, noCompact bool) {
		parentKey := defaultSessionKey()
		if parentKey == "" {
			log.Warnf(branchType, "[%s] no default session, skipping", agentID)
			return
		}

		branchKey, branchErr := session.BranchFromSession(parentKey)
		if branchErr != nil {
			log.Errorf(branchType, "[%s] branch key error (parent=%s): %v", agentID, parentKey, branchErr)
			return
		}

		orientText := buildOrientation(branchKey, parentKey, branchType)
		err := sessions.CreateBranchWithOptions(parentKey, branchKey, session.BranchOptions{
			NoResetHook:        true,
			OrientationMessage: orientText,
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
	}
}

// fireSessionEndMemory runs memory formation on the expiring session before it is cleared.
// Creates an async branch from the session so the caller can proceed immediately.
// Checks BranchMeta.NoResetHook and memory_formation.session_end_enabled.
func fireSessionEndMemory(ag *agent.Agent, sessions *session.Store, sessionKey string, mfCfg config.MemoryFormationConfig, buildOrientation func(branchKey, parentKey, branchType string) string, searchDirs []string, parentCtx context.Context) {
	if mfCfg.SessionEndEnabled != nil && !*mfCfg.SessionEndEnabled {
		return
	}

	// Check availability before doing any work
	canFire, reason := ag.CanFireBackgroundOperation(parentCtx, sessionKey)
	if !canFire {
		log.Debugf("session-end-memory", "skipping for %s: %s", sessionKey, reason)
		return
	}

	prompt := prompts.ResolvePrompt(mfCfg.SessionEndPrompt, "memory-formation.md", prompts.MemoryFormation(), searchDirs...)
	if prompt == "" {
		return
	}

	// Check branch metadata for NoResetHook
	meta, err := sessions.GetBranchMeta(sessionKey)
	if err != nil {
		log.Warnf("session-end-memory", "check branch meta for %s: %v", sessionKey, err)
	}
	if meta != nil && meta.NoResetHook {
		log.Debugf("session-end-memory", "skipping for %s (no_reset_hook set)", sessionKey)
		return
	}

	// Branch from expiring session so the memory formation job has conversation history.
	// The caller proceeds immediately to clear the main session.
	// Create session-end memory branch
	branchKey, err := session.BranchFromSession(sessionKey)
	if err != nil {
		log.Errorf("session-end-memory", "create branch key for session %s: %v", sessionKey, err)
		return
	}
	orientText := buildOrientation(branchKey, sessionKey, "session-end-memory")
	if err := sessions.CreateBranchWithOptions(sessionKey, branchKey, session.BranchOptions{
		NoResetHook:        true,
		OrientationMessage: orientText,
	}); err != nil {
		log.Errorf("session-end-memory", "branch error for session %s → %s: %v", sessionKey, branchKey, err)
		return
	}

	log.Infof("session-end-memory", "firing for %s → %s", sessionKey, branchKey)

	go func() {
		hookCtx, cancel := context.WithTimeout(parentCtx, 120*time.Second)
		defer cancel()
		hookCtx = agent.WithTrigger(hookCtx, "session_end_memory")
		ag.SetSessionNoCompact(branchKey, true)
		if _, err := ag.HandleMessage(hookCtx, branchKey, prompt); err != nil {
			log.Warnf("session-end-memory", "failed for %s: %v", branchKey, err)
		}
	}()
}
