package agent

import (
	"context"
	"time"

	"foci/internal/log"
	"foci/internal/session"
	"foci/shared/prompts"
)

// FireSessionEndMemory runs the reflection pass on the expiring session.
// Blocks until the turn completes (HandleMessage is synchronous for all transports).
// Checks BranchMeta.NoResetHook and reflection.session_end_enabled.
// If skipMetaCheck is true, the NoResetHook check is skipped (used for background
// work branches which set NoResetHook but should still get reflection).
func (a *Agent) FireSessionEndMemory(ctx context.Context, sessionKey, orientTemplate string, skipMetaCheck bool) {
	if !a.Reflection.SessionEndEnabled {
		return
	}

	canFire, reason := a.CanFireBackgroundOperation(ctx, sessionKey)
	if !canFire {
		log.Debugf("session-end-memory", "skipping for %s: %s", sessionKey, reason)
		return
	}

	prompt := prompts.ResolvePrompt(a.Reflection.SessionEndPrompt, "reflection.md", prompts.Reflection(), a.PromptSearchDirs...)
	if prompt == "" {
		return
	}

	if !skipMetaCheck {
		meta, err := a.Sessions.GetBranchMeta(sessionKey)
		if err != nil {
			log.Warnf("session-end-memory", "check branch meta for %s: %v", sessionKey, err)
		}
		if meta != nil && meta.NoResetHook {
			log.Debugf("session-end-memory", "skipping for %s (no_reset_hook set)", sessionKey)
			return
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
			return
		}
		a.SetSessionNoCompact(branchKey, true)
		targetKey = branchKey
	}

	log.Infof("session-end-memory", "firing for %s → %s", sessionKey, targetKey)

	hookCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	hookCtx = WithTrigger(hookCtx, "session_end_memory")
	if err := a.HandleMessage(hookCtx, targetKey, []string{prompt}, nil); err != nil {
		log.Warnf("session-end-memory", "failed for %s: %v", targetKey, err)
	}
}
