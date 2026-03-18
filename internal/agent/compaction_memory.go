package agent

import (
	"context"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/session"
	"foci/shared/prompts"
)

// FireCompactionMemory runs memory formation on the session immediately before
// compaction summarises and replaces the history. Creates an async branch so
// the memory agent sees the full pre-compaction context.
func FireCompactionMemory(ag *Agent, sessions *session.Store, sessionKey string, mfCfg config.MemoryFormationConfig, buildOrientation func(branchKey, parentKey, branchType string) string, searchDirs []string, parentCtx context.Context) {
	if mfCfg.CompactionEnabled != nil && !*mfCfg.CompactionEnabled {
		return
	}

	canFire, reason := ag.CanFireBackgroundOperation(parentCtx, sessionKey)
	if !canFire {
		log.Debugf("compaction-memory", "skipping for %s: %s", sessionKey, reason)
		return
	}

	prompt := prompts.ResolvePrompt(mfCfg.CompactionPrompt, "memory-formation.md", prompts.MemoryFormation(), searchDirs...)
	if prompt == "" {
		return
	}

	branchKey, err := session.BranchFromSession(sessionKey)
	if err != nil {
		log.Errorf("compaction-memory", "create branch key for session %s: %v", sessionKey, err)
		return
	}
	orientText := buildOrientation(branchKey, sessionKey, "compaction-memory")
	if err := sessions.CreateBranchWithOptions(sessionKey, branchKey, session.BranchOptions{
		NoResetHook:        true,
		OrientationMessage: orientText,
	}); err != nil {
		log.Errorf("compaction-memory", "branch error for session %s → %s: %v", sessionKey, branchKey, err)
		return
	}

	ag.SetSessionNoCompact(branchKey, true)

	log.Infof("compaction-memory", "firing for %s → %s", sessionKey, branchKey)

	go func() {
		hookCtx, cancel := context.WithTimeout(parentCtx, 120*time.Second)
		defer cancel()
		hookCtx = WithTrigger(hookCtx, "compaction_memory")
		if _, err := ag.HandleMessage(hookCtx, branchKey, prompt); err != nil {
			log.Warnf("compaction-memory", "failed for %s: %v", branchKey, err)
		}
	}()
}
