package agent

import (
	"context"
	"testing"

	"foci/internal/delegator"
)

// brancherBackend is a mockBackendDM that also implements
// delegator.BackendBrancher, so a DelegatedManager built with it reports
// BackendCanBranch() == true.
type brancherBackend struct{ mockBackendDM }

func (b *brancherBackend) ForkSession(_ context.Context, _ delegator.ForkRequest) (delegator.ForkResult, error) {
	return delegator.ForkResult{SessionID: "forked"}, nil
}

// TestBranchStrategyFor locks the branching-decision matrix for a delegated
// agent whose backend CANNOT fork its conversation (NewBackend nil): inject in
// place for non-terminal passes, fork (+remap, handled by the caller) for
// session-end, independent session for background work; API agents always fork
// a history-reading branch.
func TestBranchStrategyFor(t *testing.T) {
	// A non-nil DelegatedManager marks the agent as delegated. This manager has
	// no NewBackend, so BackendCanBranch() is false → legacy matrix.
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

// TestBranchStrategyForBranchCapable locks the matrix for a delegated agent
// whose backend CAN fork (BackendBrancher): the payoff — every branch becomes a
// real backend fork EXCEPT keepalive and compaction-memory, which stay bound to
// the live session's process/cache/compaction lifecycle.
func TestBranchStrategyForBranchCapable(t *testing.T) {
	mgr := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) { return &brancherBackend{}, nil },
	}
	if !mgr.BackendCanBranch() {
		t.Fatal("BackendCanBranch() = false, want true for brancherBackend")
	}
	agent := &Agent{DelegatedManager: mgr}

	cases := []struct {
		branchType string
		want       BranchStrategy
	}{
		{"session-end-memory", BranchFork}, // excluded: remap lifecycle (#1120)
		{"reflection", BranchForkBackend},
		{"keepalive", BranchForkBackend},
		{"compaction-memory", BranchForkBackend},
		{"background", BranchForkBackend},
		{"consolidation", BranchForkBackend},
		{"maintenance", BranchForkBackend},
		{"branch", BranchForkBackend},
	}
	for _, c := range cases {
		if got := agent.BranchStrategyFor(c.branchType); got != c.want {
			t.Errorf("branch-capable %q = %v, want %v", c.branchType, got, c.want)
		}
	}
}
