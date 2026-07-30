package agent

import (
	"context"

	"foci/internal/session"
)

// BranchStrategy answers WHERE an extra turn (a memory pass, a reflection, a
// /branch) runs for a given agent. Two of the four answers are not branches at
// all — RunInPlace takes another turn on the existing key and RunIndependent
// spins an unrelated session — so read this as "how is the pass realised",
// not "should it branch".
//
// It is the SINGLE source for that decision, which used to be duplicated — and
// inconsistent — across the periodic scheduler, the compaction-memory hook, and
// the session-end / reset path. All three now route through BranchStrategyFor;
// do not re-derive the decision inline anywhere.
type BranchStrategy int

const (
	// RunInPlace injects the turn into the existing session. Used for
	// DELEGATED agents on non-terminal passes (reflection, keepalive,
	// compaction-memory): the live backend already holds the conversation
	// context and the session continues afterwards, so there is nothing to
	// branch — we just run one more turn on the same key.
	RunInPlace BranchStrategy = iota

	// BranchFork creates a separate branch session that reads the parent's
	// history. Used for every API-agent pass (they keep no live backend to
	// inject into) and for session-end on any agent — see the session-end case
	// in BranchStrategyFor for why a delegated agent "branches" here even though
	// its backend does not support real conversation forks.
	BranchFork

	// RunIndependent spins a fresh, isolated session (its own key, reset when
	// the turn completes). Used for DELEGATED background / consolidation work
	// that must not touch the main conversation at all.
	RunIndependent

	// BranchForkBackend creates a branch session whose BACKEND conversation is a
	// REAL fork of the parent — the backend implements delegator.BackendBrancher
	// and clones its own conversation. The branch starts with the parent's full
	// context in an isolated session and the parent keeps running untouched.
	// This is the payoff of backend branching: chosen for every delegated branch
	// (reflection, background, session-end, the /branch endpoint) whose backend
	// can fork, replacing both RunInPlace (which polluted the main thread) and
	// RunIndependent (which started empty).
	//
	// Whether a given backend can fork is a CAPABILITY, asked at runtime via
	// BackendCanBranch. Never encode the answer as a list of backend names,
	// here or anywhere: that list has already gone stale once and mislabelled a
	// backend that had since gained the capability.
	BranchForkBackend
)

// BranchStrategyFor returns how a memory/reflection turn of branchType should
// run for this agent. THE decision authority for branching — see BranchStrategy.
func (a *Agent) BranchStrategyFor(branchType string) BranchStrategy {
	// API agents keep no live backend between turns, so every pass runs on a
	// branch session created from the on-disk history.
	if a.DelegatedManager == nil {
		return BranchFork
	}

	// Delegated (backend-managed, e.g. Claude Code) agents.

	// session-end-memory is excluded from the backend-fork generalisation: it
	// remaps the live backend onto a bookkeeping branch key so it finishes
	// memory while a fresh session takes the original key (#1120).
	// RunSessionEndMemory owns that flow and resets the branch backend itself; it
	// does NOT use a transcript fork. Keep returning BranchFork so the
	// single-authority contract matches what that code actually does.
	if branchType == "session-end-memory" {
		return BranchFork
	}

	// The payoff of backend branching: for every other delegated branch
	// (reflection, keepalive, compaction-memory, background, consolidation, the
	// /branch endpoint), when the backend can fork its conversation we make a
	// REAL isolated fork that starts from the parent's full context and leaves
	// the parent running — replacing the old compromises (in-place polluted the
	// main thread; independent started empty). Callers must quiesce the parent
	// while the fork clones the transcript (see ForkBackendBranch).
	//
	// An operator override can force ONE of the four periodic operations
	// (background/keepalive/reflection/consolidation) to skip the fork and take
	// the same fallback path a non-branch-capable backend already uses below —
	// e.g. [keepalive] force_in_session=true keeps keepalive in-place even on a
	// backend that can otherwise branch. Checked before BackendCanBranch so the
	// override always wins for that operation; every other branchType
	// (compaction-memory, session-end-memory, the /branch endpoint, spawn, …) is
	// unaffected. Unset (the default) preserves prior behaviour exactly.
	if !a.forceInSessionOverride(branchType) && a.DelegatedManager.BackendCanBranch() {
		return BranchForkBackend
	}

	// Backend can't fork (or an override forced this operation into the same
	// path) — legacy per-type behaviour.
	switch branchType {
	case "reflection", "keepalive", "compaction-memory":
		return RunInPlace
	default:
		// background / consolidation / maintenance: isolated one-off sessions.
		return RunIndependent
	}
}

// forceInSessionOverride reports whether an operator override forces
// branchType — one of the four periodic operations "keepalive", "background",
// "reflection", or "consolidation" — to skip a real backend fork and run
// through the same fallback path a non-branch-capable backend already uses.
// Read live from each operation's own config section (`force_in_session` /
// `consolidation_force_in_session`), so an edit applies without a restart —
// see keepalive()/backgroundConfig()/reflection()/maintenance(). Any other
// branchType (compaction-memory, session-end-memory, spawn, the /branch
// endpoint, …) is not covered by this override and always returns false.
func (a *Agent) forceInSessionOverride(branchType string) bool {
	switch branchType {
	case "keepalive":
		return a.keepalive().ForceInSession
	case "background":
		return a.backgroundConfig().ForceInSession
	case "reflection":
		return a.reflection().ForceInSession
	case "consolidation":
		return a.maintenance().ConsolidationForceInSession
	default:
		return false
	}
}

// ForkBackendBranch realises the BranchForkBackend strategy: it creates a branch
// session and forks the parent's BACKEND conversation into it, returning the
// branch key ready to run a turn on. The parent is left untouched.
//
// ok=false means the fork couldn't be performed — the backend can't branch, or
// the parent has no started backend session to fork yet. No branch session is
// created in that case (nothing to clean up); the caller should fall back to
// its in-place / independent path.
//
// PRECONDITION: the parent must be quiescent (no in-flight turn) for the
// duration of this call — the fork copies the parent's transcript on disk, and
// a concurrent turn writing to it would produce a torn copy. Callers that are
// NOT already running inside the parent's inbox worker MUST wrap this in
// a.EnqueueInjectWait(ctx, parentKey, …). Callers already in the worker (e.g.
// the post-turn compaction-memory hook) hold that exclusivity already.
func (a *Agent) ForkBackendBranch(ctx context.Context, parentKey string, opts session.BranchOptions) (string, bool) {
	if a.DelegatedManager == nil {
		return "", false
	}
	// Fork the backend conversation FIRST — only mint the branch key if it
	// succeeds, so a failed/impossible fork leaves no orphan branch session.
	forkedID, err := a.DelegatedManager.ForkParentSession(ctx, parentKey)
	if err != nil {
		a.taggedLog(opts.BranchType).Warnf("backend fork of %s failed: %v", parentKey, err)
		return "", false
	}
	if forkedID == "" {
		return "", false // nothing to fork (caller falls back)
	}
	branchKey, err := a.Sessions.CreateBranchWithOptions(parentKey, opts)
	if err != nil {
		a.taggedLog(opts.BranchType).Errorf("fork branch key create for %s: %v", parentKey, err)
		return "", false
	}
	// Point the branch key at the forked backend session so the next turn on it
	// resumes the clone (full parent context) via the normal getOrCreate path.
	a.DelegatedManager.saveResumeID(branchKey, forkedID)
	a.TouchRootCacheForBranch(branchKey) // branching warms root's shared prefix once
	a.taggedLog(opts.BranchType).Infof("backend fork %s → %s (%s)", parentKey, branchKey, forkedID)
	return branchKey, true
}

// ForkSession creates a branch of parentKey ready to run a turn on, using the
// fork implementation appropriate to the agent. It is THE single entry point
// every forker routes through — spawn, the /branch endpoint, and the periodic
// reflection/keepalive/background branches — so the API-vs-backend routing lives
// in exactly one place:
//
//   - Delegated backend that can fork (BranchStrategyFor == BranchForkBackend):
//     a REAL transcript fork (ForkBackendBranch) — the branch starts with the
//     parent's full context and the parent keeps running untouched.
//   - API agent: an API-style session branch reading the parent's on-disk history.
//
// ok=false with err==nil means no fork was produced — a delegated backend that
// can't fork, or one whose backend session hasn't started yet — and the caller
// applies its own fallback (send-to-parent, an in-place turn, or an error).
//
// The clone runs WITHOUT quiescing the parent: the transcript copy is race-safe
// by construction (ccstream.forkTranscript), so ForkSession is safe to call
// mid-turn — e.g. from the spawn tool, where routing the fork through the parent's
// inbox worker (as the periodic path once did) would deadlock.
func (a *Agent) ForkSession(ctx context.Context, parentKey string, opts session.BranchOptions) (branchKey string, ok bool, err error) {
	if a.DelegatedManager != nil {
		if a.BranchStrategyFor(opts.BranchType) != BranchForkBackend {
			return "", false, nil
		}
		bk, forked := a.ForkBackendBranch(ctx, parentKey, opts)
		return bk, forked, nil
	}
	bk, err := a.Sessions.CreateBranchWithOptions(parentKey, opts)
	if err != nil {
		return "", false, err
	}
	a.TouchRootCacheForBranch(bk) // branching warms root's shared prefix once (ForkBackendBranch does its own)
	return bk, true, nil
}

// ForkOrFreshBranch is ForkSession for a caller that wants a branch whenever
// branching is possible AT ALL, rather than only when the parent's conversation
// can be cloned. It exists because ForkSession returns ok=false for two
// unrelated reasons and callers were treating them as one:
//
//   - the backend cannot branch (opencode) — no branch of any kind exists, so
//     this returns branchKey=="" and the caller must fall back to something
//     that isn't a branch;
//   - the backend CAN branch but the parent has no backend session to clone
//     (never started, or /reset) — that is not a failure. A branch of an empty
//     parent is well-defined: a fresh session, exactly what the API path
//     already produces. There is simply nothing to inherit.
//
// branchKey=="" with err==nil means the first case. inherited reports whether
// the branch carries the parent's conversation, so a caller can log honestly.
//
// A caller that REQUIRES inherited context (spawn's clone mode, whose whole
// promise is the parent's history; compaction-memory, which summarises a
// transcript that must exist) must keep using ForkSession/ForkBackendBranch and
// treat ok=false as the failure it genuinely is for them.
func (a *Agent) ForkOrFreshBranch(ctx context.Context, parentKey string, opts session.BranchOptions) (branchKey string, inherited bool, err error) {
	bk, forked, err := a.ForkSession(ctx, parentKey, opts)
	if err != nil {
		return "", false, err
	}
	if forked {
		return bk, true, nil
	}
	if a.DelegatedManager == nil || !a.DelegatedManager.BackendCanBranch() {
		return "", false, nil
	}
	bk, err = a.Sessions.CreateBranchWithOptions(parentKey, opts)
	if err != nil {
		return "", false, err
	}
	a.TouchRootCacheForBranch(bk)
	return bk, false, nil
}

// createMemoryBranch creates the branch session used by the BranchFork strategy:
// a NoResetHook child of parentKey that reads its history, marked no-compact so
// it can't summarise itself mid-pass. Shared by the compaction-memory and
// session-end paths. Returns ok=false on error (already logged).
func (a *Agent) createMemoryBranch(parentKey, branchType, orientTemplate string) (string, bool) {
	branchKey, err := a.Sessions.CreateBranchWithOptions(parentKey, session.BranchOptions{
		NoResetHook:         true,
		BranchType:          branchType,
		OrientationTemplate: orientTemplate,
	})
	if err != nil {
		a.taggedLog(branchType).Errorf("branch error for session %s: %v", parentKey, err)
		return "", false
	}
	a.SetSessionNoCompact(branchKey, true)
	a.TouchRootCacheForBranch(branchKey) // branching warms root's shared prefix once
	return branchKey, true
}
