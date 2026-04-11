package agent

import (
	"context"
	"time"

	"foci/internal/log"
	"foci/internal/session"
	"foci/shared/prompts"
)

// FireCompactionMemory runs memory formation on the session immediately before
// compaction summarises and replaces the history. Creates an async branch so
// the memory agent sees the full pre-compaction context.
func (a *Agent) FireCompactionMemory(ctx context.Context, sessionKey, orientTemplate string) {
	if !a.MemoryFormationConfig.CompactionEnabled {
		return
	}

	canFire, reason := a.CanFireBackgroundOperation(ctx, sessionKey)
	if !canFire {
		log.Debugf("compaction-memory", "skipping for %s: %s", sessionKey, reason)
		return
	}

	prompt := prompts.ResolvePrompt(a.MemoryFormationConfig.CompactionPrompt, "memory-formation.md", prompts.MemoryFormation(), a.PromptSearchDirs...)
	if prompt == "" {
		return
	}

	// Delegated agents: inject into existing session (CC has the context).
	// API agents: create a branch so the parent session isn't modified.
	targetKey := sessionKey
	if a.DelegatedManager == nil {
		branchKey, err := a.Sessions.CreateBranchWithOptions(sessionKey, session.BranchOptions{
			NoResetHook:         true,
			BranchType:          "compaction-memory",
			OrientationTemplate: orientTemplate,
		})
		if err != nil {
			log.Errorf("compaction-memory", "branch error for session %s: %v", sessionKey, err)
			return
		}
		a.SetSessionNoCompact(branchKey, true)
		targetKey = branchKey
	}

	log.Infof("compaction-memory", "firing for %s → %s", sessionKey, targetKey)

	hookCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	hookCtx = WithTrigger(hookCtx, "compaction_memory")
	if err := a.HandleMessage(hookCtx, targetKey, []string{prompt}, nil); err != nil {
		log.Warnf("compaction-memory", "failed for %s: %v", targetKey, err)
	}
}
