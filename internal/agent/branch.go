package agent

import (
	"foci/internal/log"
	"foci/internal/session"
)

// BranchStrategy is how a memory / reflection turn is realised for an agent.
// It is the SINGLE source for the "should this branch?" decision that used to
// be duplicated — and inconsistent — across the periodic scheduler, the
// compaction-memory hook, and the session-end / reset path. All three now route
// through BranchStrategyFor; do not re-derive the decision inline anywhere.
type BranchStrategy int

const (
	// BranchInPlace injects the turn into the existing session. Used for
	// DELEGATED agents on non-terminal passes (reflection, keepalive,
	// compaction-memory): the live backend already holds the conversation
	// context and the session continues afterwards, so there is nothing to
	// branch — we just run one more turn on the same key.
	BranchInPlace BranchStrategy = iota

	// BranchFork creates a separate branch session that reads the parent's
	// history. Used for every API-agent pass (they keep no live backend to
	// inject into) and for session-end on any agent — see the session-end case
	// in BranchStrategyFor for why a delegated agent "branches" here even though
	// its backend does not support real conversation forks.
	BranchFork

	// BranchIndependent spins a fresh, isolated session (its own key, reset when
	// the turn completes). Used for DELEGATED background / consolidation work
	// that must not touch the main conversation at all.
	BranchIndependent
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
	switch branchType {
	case "session-end-memory":
		// A delegated backend cannot fork a conversation — so why "branch" on
		// session end? The branch key is NOT a conversation fork; it is a
		// bookkeeping handle. On /reset (and reclaim) we need two things at
		// once: (1) the dying session must still write its memories from the
		// FULL live context, and (2) the user must get a fresh session
		// immediately, without blocking on that memory pass. We get both by
		// creating a branch session key and REMAPPING the live backend onto it
		// (see DelegatedManager.RemapSession): the old CC process keeps running
		// under the branch key and finishes memory formation in the background,
		// while a brand-new CC process takes over the original key for the next
		// message. Without the separate key the fresh session would collide with
		// the still-reflecting old one on the same key — they would share, and
		// then tear down, a single backend + exec bridge (the outage in #1120).
		// So the "branch" buys the old and new sessions independent lifecycles;
		// it is deliberately a fork of the SESSION KEY, not of the model history.
		return BranchFork
	case "reflection", "keepalive", "compaction-memory":
		return BranchInPlace
	default:
		// background / consolidation / maintenance: isolated one-off sessions.
		return BranchIndependent
	}
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
		log.Errorf(branchType, "branch error for session %s: %v", parentKey, err)
		return "", false
	}
	a.SetSessionNoCompact(branchKey, true)
	a.TouchRootCacheForBranch(branchKey) // branching warms root's shared prefix once
	return branchKey, true
}
