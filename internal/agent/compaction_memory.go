package agent

import (
	"context"
	"sync/atomic"
	"time"

	"foci/internal/session"
	"foci/internal/skills"
	"foci/shared/prompts"
)

// compactionMemoryTrigger is the trigger of the pre-compaction reflection's
// turn. It is a memory trigger (isMemoryTrigger): the turn does not count as
// session activity, and it never compacts the session itself (RunCompaction).
const compactionMemoryTrigger = "compaction_memory"

// compactionMemoryTimeout bounds the pre-compaction reflection: the wait for
// the session's inbox (outside the session's own turn) and the turn itself.
const compactionMemoryTimeout = 120 * time.Second

// FireCompactionMemory runs the pre-compaction reflection on the session
// immediately before compaction summarises and replaces the history. It
// returns when the pass has ended (or was skipped), so the caller compacts
// only after it.
//
// Where the pass runs is BranchStrategyFor("compaction-memory") (Dick,
// 2026-11-13: "Preferred is to fork and reflect in the fork, if forking is
// available. Else must be sequential"):
//   - BranchFork (API): a branch session that reads the parent's history.
//   - BranchForkBackend (a delegated backend that can fork): a real fork of
//     the pre-compaction conversation; the reflection runs in the fork.
//   - RunInPlace (or a fork that could not be made): one more turn in the
//     session itself, and compaction starts only after it ends.
//
// ctx says where the caller is (InSessionTurn). Inside the session's own
// post-turn phase the session is quiescent and the pass runs directly; an
// inbox wait there would deadlock. Outside it (/compact, quiet compaction,
// the usage hold) the fork's clone and the in-place turn run through the
// session's inbox worker, so they never overlap a user turn.
//
// A pass that ran counts as the session's reflection: the session is stamped
// (SessionIndex.StampReflection), so the interval reflection is not due for
// it until one interval later (#3088).
func (a *Agent) FireCompactionMemory(ctx context.Context, sessionKey, orientTemplate string) {
	logger := a.taggedLog("compaction-memory")
	refl := a.reflection()
	if !refl.CompactionEnabled {
		logger.Infof("skipping for %s: compaction_enabled is off", sessionKey)
		return
	}

	canFire, reason := a.CanFireBackgroundOperation(ctx, sessionKey)
	if !canFire {
		logger.Infof("skipping for %s: %s", sessionKey, reason)
		return
	}

	// Nothing happened since the last reflection of this session: a second
	// pass would only repeat it (same guard as the session-end pass).
	if a.SessionIndex != nil && a.SessionIndex.ReflectionRedundant(sessionKey) {
		logger.Infof("skipping for %s: no activity since its last reflection", sessionKey)
		return
	}

	prompt := prompts.ResolvePrompt(refl.CompactionPrompt, "reflection.md", prompts.Reflection(), a.PromptSearchDirs...)
	if prompt == "" {
		logger.Infof("skipping for %s: no reflection prompt resolved", sessionKey)
		return
	}

	inTurn := InSessionTurn(ctx)
	// Keep the caller's values (a usage hold's permit, the cost booking) but
	// not its cancellation: the pass has its own bound.
	hookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compactionMemoryTimeout)
	defer cancel()
	hookCtx = WithTrigger(hookCtx, compactionMemoryTrigger)

	targetKey := sessionKey
	var forkedBranch string
	switch a.BranchStrategyFor("compaction-memory") {
	case BranchFork:
		branchKey, ok := a.createMemoryBranch(sessionKey, "compaction-memory", orientTemplate)
		if !ok {
			logger.Warnf("skipping for %s: branch create failed", sessionKey)
			return
		}
		targetKey = branchKey
	case BranchForkBackend:
		branchKey, ok := a.forkForCompactionMemory(hookCtx, sessionKey, orientTemplate, inTurn)
		if ok {
			targetKey = branchKey
			forkedBranch = branchKey
		} else {
			// The fork could not be made: the pass runs in the session
			// itself, before the compaction.
			logger.Infof("no fork of %s: reflecting in the session", sessionKey)
		}
	}
	if forkedBranch != "" {
		defer a.DelegatedManager.ResetSession(forkedBranch)
	}

	logger.Infof("firing for %s → %s", sessionKey, targetKey)

	var skillBefore skills.SkillSnapshot
	var winStart time.Time
	skillDirs := a.SkillDirs.List() // once: before and after scan the same set (#2657)
	notifySkills := refl.NotifyOnSkillCreation && len(skillDirs) > 0 && (a.SkillChangeNotify != nil || a.SkillChangeNotifyText != nil)
	if notifySkills {
		skillBefore = skills.Snapshot(skillDirs)
		winStart = time.Now()
	}

	dispatchedAt := time.Now()
	var err error
	if targetKey == sessionKey && a.DelegatedManager != nil && !inTurn {
		err = a.runInSessionSequential(hookCtx, sessionKey, prompt)
	} else {
		err = a.HandleMessage(hookCtx, targetKey, []string{prompt}, nil)
	}
	if err != nil {
		logger.Warnf("failed for %s: %v", targetKey, err)
	} else if a.SessionIndex != nil {
		// The pass counts as this session's reflection. Stamp the dispatch
		// time, not the end: activity that arrives while it runs still counts
		// as "since the last reflection".
		a.SessionIndex.StampReflection(sessionKey, dispatchedAt)
	}

	if notifySkills {
		a.detectAndNotifySkillChanges(ctx, targetKey, skillDirs, skillBefore, winStart, time.Now())
	}
}

// forkForCompactionMemory makes the real backend fork the pre-compaction
// reflection runs in. ForkBackendBranch needs the parent quiescent while it
// clones: inside the session's own turn it is; outside, the clone runs in the
// session's inbox worker, behind any user turn.
func (a *Agent) forkForCompactionMemory(ctx context.Context, sessionKey, orientTemplate string, inTurn bool) (string, bool) {
	opts := session.BranchOptions{
		NoResetHook:         true,
		BranchType:          "compaction-memory",
		OrientationTemplate: orientTemplate,
	}
	if inTurn {
		return a.ForkBackendBranch(ctx, sessionKey, opts)
	}
	var branchKey string
	var ok bool
	if err := a.EnqueueInjectWait(ctx, sessionKey, compactionMemoryTrigger, func() {
		branchKey, ok = a.ForkBackendBranch(ctx, sessionKey, opts)
	}); err != nil {
		a.taggedLog("compaction-memory").Warnf("fork of %s not made: %v", sessionKey, err)
		return "", false
	}
	return branchKey, ok
}

// runInSessionSequential runs the pre-compaction reflection as a turn in the
// session itself, through its inbox worker, for a caller outside the
// session's own turn. It returns only once that turn has ended, or once it is
// sure the turn will never start, so the compaction that follows never runs
// under it.
//
// The timeout in ctx bounds the wait for the worker. If it expires before the
// turn starts, the queued closure is disarmed and does nothing. If the turn
// has started, this waits for it to end.
func (a *Agent) runInSessionSequential(ctx context.Context, sessionKey, prompt string) error {
	const (
		pending int32 = iota
		started
		abandoned
	)
	var state atomic.Int32
	finished := make(chan struct{})
	var runErr error
	err := a.EnqueueInjectWait(ctx, sessionKey, compactionMemoryTrigger, func() {
		if !state.CompareAndSwap(pending, started) {
			return // the caller gave up before the worker got here
		}
		defer close(finished)
		runErr = a.HandleMessage(ctx, sessionKey, []string{prompt}, nil)
	})
	if err == nil {
		return runErr
	}
	if state.CompareAndSwap(pending, abandoned) {
		return err // the turn never started and now never will
	}
	if state.Load() == started {
		<-finished
		return runErr
	}
	return err
}
