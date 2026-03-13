package agent

import (
	"context"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/session"
	"foci/prompts"
)

// FireSessionEndMemory runs memory formation on the expiring session before it is cleared.
// Creates an async branch from the session so the caller can proceed immediately.
// Checks BranchMeta.NoResetHook and memory_formation.session_end_enabled.
// If skipMetaCheck is true, the NoResetHook check is skipped (used for background
// work branches which set NoResetHook but should still get memory formation).
func FireSessionEndMemory(ag *Agent, sessions *session.Store, sessionKey string, mfCfg config.MemoryFormationConfig, buildOrientation func(branchKey, parentKey, branchType string) string, searchDirs []string, parentCtx context.Context, skipMetaCheck bool) {
	if mfCfg.SessionEndEnabled != nil && !*mfCfg.SessionEndEnabled {
		return
	}

	canFire, reason := ag.CanFireBackgroundOperation(parentCtx, sessionKey)
	if !canFire {
		log.Debugf("session-end-memory", "skipping for %s: %s", sessionKey, reason)
		return
	}

	prompt := prompts.ResolvePrompt(mfCfg.SessionEndPrompt, "memory-formation.md", prompts.MemoryFormation(), searchDirs...)
	if prompt == "" {
		return
	}

	if !skipMetaCheck {
		meta, err := sessions.GetBranchMeta(sessionKey)
		if err != nil {
			log.Warnf("session-end-memory", "check branch meta for %s: %v", sessionKey, err)
		}
		if meta != nil && meta.NoResetHook {
			log.Debugf("session-end-memory", "skipping for %s (no_reset_hook set)", sessionKey)
			return
		}
	}

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
		hookCtx = WithTrigger(hookCtx, "session_end_memory")
		ag.SetSessionNoCompact(branchKey, true)
		if _, err := ag.HandleMessage(hookCtx, branchKey, prompt); err != nil {
			log.Warnf("session-end-memory", "failed for %s: %v", branchKey, err)
		}
	}()
}
