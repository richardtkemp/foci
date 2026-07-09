package agent

import (
	"context"
	"time"

	"foci/internal/log"
	"foci/internal/skills"
	"foci/shared/prompts"
)

// PrepareSessionEndMemory decides whether the reflection pass should run for
// an expiring session and creates the reflection branch. Returns the branch
// key and true when reflection should proceed via RunSessionEndMemory.
//
// Synchronous, and MUST run before the parent session is reset/archived: the
// branch's branch_point is taken against the live history (an archived parent
// is later recovered through the branch loader's archive fallback, P2-5).
//
// For delegated agents the live backend (if any) is remapped to the branch
// key, so reflection drives the existing CC session — which holds the
// conversation context in-process — while the parent key is left clean for a
// fresh backend on next contact.
//
// If skipMetaCheck is true, the NoResetHook check is skipped (used for
// background work branches which set NoResetHook but should still get
// reflection).
func (a *Agent) PrepareSessionEndMemory(sessionKey, orientTemplate string, skipMetaCheck bool) (string, bool) {
	if !a.Reflection.SessionEndEnabled {
		return "", false
	}

	// No need to reflect twice: skip if a reflection has already run on this
	// session and nothing substantive has happened since (last_activity_at <=
	// last_reflection). last_activity_at excludes memory-formation turns (see
	// isMemoryTrigger), so a prior reflection's own turn doesn't count as
	// activity here. Unknown / never-reflected sessions return false → reflect.
	if a.SessionIndex != nil && a.SessionIndex.ReflectionRedundant(sessionKey) {
		log.Debugf("session-end-memory", "skipping for %s: no activity since last reflection", sessionKey)
		return "", false
	}

	if canFire, reason := a.CanFireBackgroundOperation(context.Background(), sessionKey); !canFire {
		log.Debugf("session-end-memory", "skipping for %s: %s", sessionKey, reason)
		return "", false
	}

	if !skipMetaCheck {
		meta, err := a.Sessions.GetBranchMeta(sessionKey)
		if err != nil {
			log.Warnf("session-end-memory", "check branch meta for %s: %v", sessionKey, err)
		}
		if meta != nil && meta.NoResetHook {
			log.Debugf("session-end-memory", "skipping for %s (no_reset_hook set)", sessionKey)
			return "", false
		}
	}

	// Session-end always forks a branch (BranchStrategyFor returns BranchFork for
	// session-end-memory on every agent) — even for delegated agents, whose
	// backend can't fork a conversation. See the session-end case in
	// BranchStrategyFor for the full rationale: the branch key is a bookkeeping
	// handle we remap the live backend onto so it finishes memory in the
	// background while a fresh session takes over the original key.
	branchKey, ok := a.createMemoryBranch(sessionKey, "session-end-memory", orientTemplate)
	if !ok {
		return "", false
	}

	// Hand the live delegated backend (and its resume ID) to the branch so
	// reflection reaches the CC session that holds the context. (No-op for API
	// agents, which have no live backend.)
	if a.DelegatedManager != nil {
		a.DelegatedManager.RemapSession(sessionKey, branchKey)
	}

	return branchKey, true
}

// RunSessionEndMemory runs the reflection turn on a prepared branch.
// Blocks until the turn completes (HandleMessage is synchronous for all
// transports). For delegated agents the branch's backend is destroyed
// afterwards — reflection is the branch's last act.
func (a *Agent) RunSessionEndMemory(ctx context.Context, branchKey string) {
	prompt := prompts.ResolvePrompt(a.Reflection.SessionEndPrompt, "reflection.md", prompts.Reflection(), a.PromptSearchDirs...)
	if prompt == "" {
		return
	}

	log.Infof("session-end-memory", "firing on %s", branchKey)

	var skillBefore skills.SkillSnapshot
	if a.Reflection.NotifyOnSkillCreation && len(a.SkillDirs) > 0 && a.SkillChangeNotify != nil {
		skillBefore = skills.Snapshot(a.SkillDirs)
	}

	hookCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	hookCtx = WithTrigger(hookCtx, "session_end_memory")
	if err := a.HandleMessage(hookCtx, branchKey, []string{prompt}, nil); err != nil {
		log.Warnf("session-end-memory", "failed for %s: %v", branchKey, err)
	}

	if skillBefore != nil {
		a.detectAndNotifySkillChanges(branchKey, skillBefore)
	}

	if a.DelegatedManager != nil {
		a.DelegatedManager.ResetSession(branchKey)
	}
	// The branch is done — drop its metadata rows (no_compact, remapped
	// cc_resume_id, …) so nothing leaks per reset.
	if a.SessionIndex != nil {
		if err := a.SessionIndex.DeleteAllSessionMetadata(branchKey); err != nil {
			log.Warnf("session-end-memory", "cleanup metadata for %s: %v", branchKey, err)
		}
	}
}

// FireSessionEndMemory runs the reflection pass on the expiring session:
// PrepareSessionEndMemory + RunSessionEndMemory. Blocks until the turn
// completes. Callers that reset/archive the parent themselves should call the
// two phases separately, with the reset between them.
func (a *Agent) FireSessionEndMemory(ctx context.Context, sessionKey, orientTemplate string, skipMetaCheck bool) {
	branchKey, ok := a.PrepareSessionEndMemory(sessionKey, orientTemplate, skipMetaCheck)
	if !ok {
		return
	}
	a.RunSessionEndMemory(ctx, branchKey)
}

// detectAndNotifySkillChanges diffs the current skill state against before and
// fires the SkillChangeNotify callback if anything changed. Shared by all
// reflection paths (interval, session-end, compaction).
func (a *Agent) detectAndNotifySkillChanges(sessionKey string, before skills.SkillSnapshot) {
	after := skills.Snapshot(a.SkillDirs)
	changes := skills.Diff(before, after)
	if msg := skills.FormatChanges(changes); msg != "" {
		a.SkillChangeNotify(sessionKey, msg)
	}
}
