package agent

import (
	"context"
	"time"

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
	if !a.reflection().SessionEndEnabled {
		return "", false
	}

	// No need to reflect twice: skip if a reflection has already run on this
	// session and nothing substantive has happened since (last_activity_at <=
	// last_reflection). last_activity_at excludes memory-formation turns (see
	// isMemoryTrigger), so a prior reflection's own turn doesn't count as
	// activity here. Unknown / never-reflected sessions return false → reflect.
	if a.SessionIndex != nil && a.SessionIndex.ReflectionRedundant(sessionKey) {
		a.taggedLog("session-end-memory").Debugf("skipping for %s: no activity since last reflection", sessionKey)
		return "", false
	}

	if canFire, reason := a.CanFireBackgroundOperation(context.Background(), sessionKey); !canFire {
		a.taggedLog("session-end-memory").Debugf("skipping for %s: %s", sessionKey, reason)
		return "", false
	}

	if !skipMetaCheck {
		meta, err := a.Sessions.GetBranchMeta(sessionKey)
		if err != nil {
			a.taggedLog("session-end-memory").Warnf("check branch meta for %s: %v", sessionKey, err)
		}
		if meta != nil && meta.NoResetHook {
			a.taggedLog("session-end-memory").Debugf("skipping for %s (no_reset_hook set)", sessionKey)
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
//
// parentKey is the session this branch was forked from (the key
// PrepareSessionEndMemory was called with) — needed to stamp last_reflection
// on IT, not the branch, on success (see #1465 below).
func (a *Agent) RunSessionEndMemory(ctx context.Context, parentKey, branchKey string) {
	refl := a.reflection()
	prompt := prompts.ResolvePrompt(refl.SessionEndPrompt, "reflection.md", prompts.Reflection(), a.PromptSearchDirs...)
	if prompt == "" {
		return
	}

	a.taggedLog("session-end-memory").Infof("firing on %s", branchKey)

	var skillBefore skills.SkillSnapshot
	var winStart time.Time
	notifySkills := refl.NotifyOnSkillCreation && len(a.SkillDirs) > 0 && (a.SkillChangeNotify != nil || a.SkillChangeNotifyText != nil)
	if notifySkills {
		skillBefore = skills.Snapshot(a.SkillDirs)
		winStart = time.Now()
	}

	dispatchedAt := time.Now()
	hookCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	hookCtx = WithTrigger(hookCtx, "session_end_memory")
	if err := a.HandleMessage(hookCtx, branchKey, []string{prompt}, nil); err != nil {
		a.taggedLog("session-end-memory").Warnf("failed for %s: %v", branchKey, err)
	} else if a.SessionIndex != nil && parentKey != "" {
		// #1465: this used to be missing — only the unrelated periodic
		// interval-reflection pass (internal/periodic/reflection.go) ever
		// called StampReflection, so ReflectionRedundant (the "reflect-twice
		// guard" PrepareSessionEndMemory checks on every /reset, scheduled
		// reset, and TTL reclaim) could never see that a session-end pass had
		// just covered parentKey. Symptom: "Memories from the previous
		// session are being saved in the background" fired again on the very
		// next reset even with zero activity since the prior reflection
		// completed. Stamp with the pre-turn timestamp (not post-turn) so any
		// activity that arrives while this turn is running is still counted
		// as "since the last reflection" on the next check.
		a.SessionIndex.StampReflection(parentKey, dispatchedAt)
	}

	if notifySkills {
		a.detectAndNotifySkillChanges(ctx, branchKey, skillBefore, winStart, time.Now())
	}

	if a.DelegatedManager != nil {
		a.DelegatedManager.ResetSession(branchKey)
	}
	// The branch is done — drop its metadata rows (no_compact, remapped
	// cc_resume_id, …) so nothing leaks per reset.
	if a.SessionIndex != nil {
		if err := a.SessionIndex.DeleteAllSessionMetadata(branchKey); err != nil {
			a.taggedLog("session-end-memory").Warnf("cleanup metadata for %s: %v", branchKey, err)
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
	a.RunSessionEndMemory(ctx, sessionKey, branchKey)
}

// detectAndNotifySkillChanges diffs the current skill state against before,
// then splits the changes on whether their skill dir is a git repo
// (skills.SplitByGitRepo — #1404, per Dick: a non-git-repo skill dir must
// keep its EXACT pre-#1404 behaviour):
//   - non-git-repo changes fire SkillChangeNotifyText with the plain
//     mtime-diff message (skills.FormatChanges), unconditionally — same as
//     before this fix ever existed.
//   - git-repo changes are gated on a real git commit landing inside
//     [winStart, winEnd] that touches the changed files
//     (skills.AttributeToGit) — the causal check that replaces the old "any
//     mtime moved in the window" attribution for the case that actually can
//     have concurrent writers colliding via a shared directory. Only
//     git-attributed changes fire SkillChangeNotify; anything else (no
//     commit in the window) produces nothing.
//
// Shared by all reflection paths (interval, session-end, compaction).
func (a *Agent) detectAndNotifySkillChanges(ctx context.Context, sessionKey string, before skills.SkillSnapshot, winStart, winEnd time.Time) {
	after := skills.Snapshot(a.SkillDirs)
	changes := skills.Diff(before, after)
	gitDirChanges, nonGitDirChanges := skills.SplitByGitRepo(ctx, changes)

	if a.SkillChangeNotifyText != nil {
		if msg := skills.FormatChanges(nonGitDirChanges); msg != "" {
			a.SkillChangeNotifyText(sessionKey, msg)
		}
	}
	if a.SkillChangeNotify != nil {
		for _, rep := range skills.AttributeToGit(ctx, gitDirChanges, winStart, winEnd) {
			a.SkillChangeNotify(sessionKey, rep.Name, rep.Markdown)
		}
	}
}
