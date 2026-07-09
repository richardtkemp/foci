package agent

import (
	"context"
	"time"

	"foci/internal/log"
	"foci/internal/skills"
	"foci/shared/prompts"
)

// FireCompactionMemory runs memory formation on the session immediately before
// compaction summarises and replaces the history. Creates an async branch so
// the memory agent sees the full pre-compaction context.
func (a *Agent) FireCompactionMemory(ctx context.Context, sessionKey, orientTemplate string) {
	if !a.Reflection.CompactionEnabled {
		return
	}

	canFire, reason := a.CanFireBackgroundOperation(ctx, sessionKey)
	if !canFire {
		log.Debugf("compaction-memory", "skipping for %s: %s", sessionKey, reason)
		return
	}

	prompt := prompts.ResolvePrompt(a.Reflection.CompactionPrompt, "reflection.md", prompts.Reflection(), a.PromptSearchDirs...)
	if prompt == "" {
		return
	}

	// Delegated agents inject into the existing session (CC holds the live
	// context); API agents branch so the parent session isn't modified. The
	// choice comes from the single branch authority — see BranchStrategyFor.
	targetKey := sessionKey
	if a.BranchStrategyFor("compaction-memory") == BranchFork {
		branchKey, ok := a.createMemoryBranch(sessionKey, "compaction-memory", orientTemplate)
		if !ok {
			return
		}
		targetKey = branchKey
	}

	log.Infof("compaction-memory", "firing for %s → %s", sessionKey, targetKey)

	var skillBefore skills.SkillSnapshot
	if a.Reflection.NotifyOnSkillCreation && len(a.SkillDirs) > 0 && a.SkillChangeNotify != nil {
		skillBefore = skills.Snapshot(a.SkillDirs)
	}

	hookCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	hookCtx = WithTrigger(hookCtx, "compaction_memory")
	if err := a.HandleMessage(hookCtx, targetKey, []string{prompt}, nil); err != nil {
		log.Warnf("compaction-memory", "failed for %s: %v", targetKey, err)
	}

	if skillBefore != nil {
		a.detectAndNotifySkillChanges(targetKey, skillBefore)
	}
}
