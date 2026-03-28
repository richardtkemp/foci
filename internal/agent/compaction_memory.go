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
func FireCompactionMemory(ag *Agent, sessions *session.Store, sessionKey string, mfCfg config.MemoryFormationConfig, orientTemplate string, searchDirs []string, parentCtx context.Context) {
	if mfCfg.CompactionEnabled != nil && !*mfCfg.CompactionEnabled {
		return
	}

	canFire, reason := ag.CanFireBackgroundOperation(parentCtx, sessionKey)
	if !canFire {
		log.Debugf("compaction-memory", "skipping for %s: %s", sessionKey, reason)
		return
	}

	prompt := prompts.ResolvePrompt(config.DerefStr(mfCfg.CompactionPrompt), "memory-formation.md", prompts.MemoryFormation(), searchDirs...)
	if prompt == "" {
		return
	}

	// Backend agents: inject into existing session (CC has the context).
	// API agents: create a branch so the parent session isn't modified.
	targetKey := sessionKey
	if ag.BackendManager == nil {
		branchKey, err := sessions.CreateBranchWithOptions(sessionKey, session.BranchOptions{
			NoResetHook:         true,
			BranchType:          "compaction-memory",
			OrientationTemplate: orientTemplate,
		})
		if err != nil {
			log.Errorf("compaction-memory", "branch error for session %s: %v", sessionKey, err)
			return
		}
		ag.SetSessionNoCompact(branchKey, true)
		targetKey = branchKey
	}

	log.Infof("compaction-memory", "firing for %s → %s", sessionKey, targetKey)

	go func() {
		hookCtx, cancel := context.WithTimeout(parentCtx, 120*time.Second)
		defer cancel()
		hookCtx = WithTrigger(hookCtx, "compaction_memory")
		if _, err := ag.HandleMessage(hookCtx, targetKey, prompt); err != nil {
			log.Warnf("compaction-memory", "failed for %s: %v", targetKey, err)
		}
	}()
}
