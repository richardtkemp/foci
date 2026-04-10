package agent

import (
	"context"
	"time"

	"foci/internal/log"
	"foci/internal/session"
	"foci/shared/prompts"
)

// FireSessionEndMemory runs memory formation on the expiring session before it is cleared.
// Creates an async branch from the session so the caller can proceed immediately.
// Checks BranchMeta.NoResetHook and memory_formation.session_end_enabled.
// If skipMetaCheck is true, the NoResetHook check is skipped (used for background
// work branches which set NoResetHook but should still get memory formation).
//
// Returns a channel that is closed when memory formation completes (or immediately
// if skipped). Callers that need to wait (e.g. reclaim hooks closing a backend)
// can select on it; fire-and-forget callers can ignore the return value.
func (a *Agent) FireSessionEndMemory(ctx context.Context, sessionKey, orientTemplate string, skipMetaCheck bool) <-chan struct{} {
	done := make(chan struct{})

	if !a.MemoryFormationConfig.SessionEndEnabled {
		close(done)
		return done
	}

	canFire, reason := a.CanFireBackgroundOperation(ctx, sessionKey)
	if !canFire {
		log.Debugf("session-end-memory", "skipping for %s: %s", sessionKey, reason)
		close(done)
		return done
	}

	prompt := prompts.ResolvePrompt(a.MemoryFormationConfig.SessionEndPrompt, "memory-session-end.md", prompts.MemoryFormation(), a.PromptSearchDirs...)
	if prompt == "" {
		close(done)
		return done
	}

	if !skipMetaCheck {
		meta, err := a.Sessions.GetBranchMeta(sessionKey)
		if err != nil {
			log.Warnf("session-end-memory", "check branch meta for %s: %v", sessionKey, err)
		}
		if meta != nil && meta.NoResetHook {
			log.Debugf("session-end-memory", "skipping for %s (no_reset_hook set)", sessionKey)
			close(done)
			return done
		}
	}

	// Delegated agents: inject into existing session (CC has the context).
	// API agents: create a branch so the parent session isn't modified.
	targetKey := sessionKey
	if a.DelegatedManager == nil {
		branchKey, err := a.Sessions.CreateBranchWithOptions(sessionKey, session.BranchOptions{
			NoResetHook:         true,
			BranchType:          "session-end-memory",
			OrientationTemplate: orientTemplate,
		})
		if err != nil {
			log.Errorf("session-end-memory", "branch error for session %s: %v", sessionKey, err)
			close(done)
			return done
		}
		a.SetSessionNoCompact(branchKey, true)
		targetKey = branchKey
	}

	log.Infof("session-end-memory", "firing for %s → %s", sessionKey, targetKey)

	go func() {
		defer close(done)
		hookCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
		defer cancel()
		hookCtx = WithTrigger(hookCtx, "session_end_memory")
		if _, err := a.HandleMessage(hookCtx, targetKey, prompt); err != nil {
			log.Warnf("session-end-memory", "failed for %s: %v", targetKey, err)
		}
	}()

	return done
}
