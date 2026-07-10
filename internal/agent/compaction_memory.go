package agent

import (
	"context"
	"time"

	"foci/internal/log"
	"foci/internal/session"
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

	// The target depends on the single branch authority (BranchStrategyFor):
	//   - BranchFork (API): a history-reading branch, parent unmodified.
	//   - BranchForkBackend (delegated + branch-capable): a REAL transcript fork.
	//     This hook already runs inside the parent's inbox worker (post-turn
	//     phase), so the parent is exclusive — clone directly, no EnqueueInjectWait
	//     (which would deadlock). Reset the forked backend after the turn.
	//   - otherwise (BranchInPlace): inject into the live session.
	targetKey := sessionKey
	var forkedBranch string
	switch a.BranchStrategyFor("compaction-memory") {
	case BranchFork:
		branchKey, ok := a.createMemoryBranch(sessionKey, "compaction-memory", orientTemplate)
		if !ok {
			return
		}
		targetKey = branchKey
	case BranchForkBackend:
		branchKey, ok := a.ForkBackendBranch(ctx, sessionKey, session.BranchOptions{
			NoResetHook:         true,
			BranchType:          "compaction-memory",
			OrientationTemplate: orientTemplate,
		})
		if ok {
			targetKey = branchKey
			forkedBranch = branchKey
		}
		// !ok → fall back to in-place (targetKey stays sessionKey).
	}
	if forkedBranch != "" {
		defer a.DelegatedManager.ResetSession(forkedBranch)
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
