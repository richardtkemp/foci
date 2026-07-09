package agent

import "testing"

// TestBranchStrategyFor locks the single branching-decision matrix: delegated
// agents inject in place for non-terminal passes, fork (+remap, handled by the
// caller) for session-end, and use an independent session for background work;
// API agents always fork a history-reading branch.
func TestBranchStrategyFor(t *testing.T) {
	// A non-nil DelegatedManager marks the agent as delegated; BranchStrategyFor
	// only inspects whether the field is set.
	delegated := &Agent{DelegatedManager: &DelegatedManager{}}
	api := &Agent{} // no DelegatedManager

	cases := []struct {
		branchType string
		delegated  BranchStrategy
		api        BranchStrategy
	}{
		{"reflection", BranchInPlace, BranchFork},
		{"keepalive", BranchInPlace, BranchFork},
		{"compaction-memory", BranchInPlace, BranchFork},
		{"session-end-memory", BranchFork, BranchFork},
		{"background", BranchIndependent, BranchFork},
		{"consolidation", BranchIndependent, BranchFork},
		{"maintenance", BranchIndependent, BranchFork},
	}

	for _, c := range cases {
		if got := delegated.BranchStrategyFor(c.branchType); got != c.delegated {
			t.Errorf("delegated %q = %v, want %v", c.branchType, got, c.delegated)
		}
		if got := api.BranchStrategyFor(c.branchType); got != c.api {
			t.Errorf("api %q = %v, want %v", c.branchType, got, c.api)
		}
	}
}
