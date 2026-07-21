package agent

import (
	"context"
	"testing"

	"foci/internal/delegator"
	"foci/internal/session"
)

// brancherBackend is a mockBackendDM that also implements
// delegator.BackendBrancher, so a DelegatedManager built with it reports
// BackendCanBranch() == true.
type brancherBackend struct{ mockBackendDM }

func (b *brancherBackend) ForkSession(_ context.Context, _ delegator.ForkRequest) (delegator.ForkResult, error) {
	return delegator.ForkResult{SessionID: "forked"}, nil
}

func (b *brancherBackend) CleanupSession(_ context.Context, _ delegator.CleanupRequest) error {
	return nil
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
// real backend fork EXCEPT session-end-memory, which stays a plain session fork
// so its lifecycle can be remapped onto the successor session (#1120).
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

// TestBranchStrategyForForceInSessionOverride locks #1450: a per-operation
// force_in_session override on a branch-capable backend must fall back to
// EXACTLY the same strategy a non-branch-capable backend already uses for
// that branchType (BranchInPlace for keepalive/reflection, BranchIndependent
// for background/consolidation) — and must NOT affect any other branchType,
// which stays BranchForkBackend (or BranchFork for session-end-memory).
func TestBranchStrategyForForceInSessionOverride(t *testing.T) {
	mgr := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) { return &brancherBackend{}, nil },
	}

	cases := []struct {
		name        string
		configure   func(a *Agent)
		wantForType string // the branchType the override targets
		wantForced  BranchStrategy
	}{
		{
			name:        "keepalive override forces in-place",
			configure:   func(a *Agent) { a.Keepalive.ForceInSession = true },
			wantForType: "keepalive",
			wantForced:  BranchInPlace,
		},
		{
			name:        "reflection override forces in-place",
			configure:   func(a *Agent) { a.Reflection.ForceInSession = true },
			wantForType: "reflection",
			wantForced:  BranchInPlace,
		},
		{
			name:        "background override forces independent",
			configure:   func(a *Agent) { a.Background.ForceInSession = true },
			wantForType: "background",
			wantForced:  BranchIndependent,
		},
		{
			name:        "consolidation override forces independent",
			configure:   func(a *Agent) { a.Maintenance.ConsolidationForceInSession = true },
			wantForType: "consolidation",
			wantForced:  BranchIndependent,
		},
	}

	otherTypes := []string{"keepalive", "reflection", "background", "consolidation", "compaction-memory"}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Agent{DelegatedManager: mgr}
			c.configure(a)

			if got := a.BranchStrategyFor(c.wantForType); got != c.wantForced {
				t.Errorf("%s: BranchStrategyFor(%q) = %v, want %v (forced)", c.name, c.wantForType, got, c.wantForced)
			}

			// Every OTHER operation must be unaffected — still a real backend
			// fork, since the backend can branch and no override applies to it.
			for _, other := range otherTypes {
				if other == c.wantForType {
					continue
				}
				if got := a.BranchStrategyFor(other); got != BranchForkBackend {
					t.Errorf("%s: BranchStrategyFor(%q) = %v, want BranchForkBackend (unaffected by the %q override)", c.name, other, got, c.wantForType)
				}
			}

			// session-end-memory is excluded from backend-forking entirely and
			// must stay BranchFork regardless of any force_in_session override.
			if got := a.BranchStrategyFor("session-end-memory"); got != BranchFork {
				t.Errorf("%s: BranchStrategyFor(session-end-memory) = %v, want BranchFork", c.name, got)
			}
		})
	}

	// Default (unset) behaviour is unchanged: every one of the four ops still
	// forks on a branch-capable backend when no override is set.
	t.Run("unset override changes nothing", func(t *testing.T) {
		a := &Agent{DelegatedManager: mgr}
		for _, bt := range otherTypes {
			if got := a.BranchStrategyFor(bt); got != BranchForkBackend {
				t.Errorf("BranchStrategyFor(%q) = %v, want BranchForkBackend (no override set)", bt, got)
			}
		}
	})
}

func TestForkSession_Routing(t *testing.T) {
	t.Run("api agent branches the session store", func(t *testing.T) {
		a := &Agent{Sessions: session.NewStore(t.TempDir())}
		bk, ok, err := a.ForkSession(context.Background(), "agent/c123", session.BranchOptions{BranchType: "spawn"})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !ok || bk == "" {
			t.Fatalf("api fork: ok=%v bk=%q, want true + nonempty", ok, bk)
		}
	})

	t.Run("delegated can-branch, no backend session yet", func(t *testing.T) {
		mgr := &DelegatedManager{NewBackend: func() (delegator.Delegator, error) { return &brancherBackend{}, nil }}
		a := &Agent{DelegatedManager: mgr}
		bk, ok, err := a.ForkSession(context.Background(), "agent/c123", session.BranchOptions{BranchType: "spawn"})
		if err != nil || ok || bk != "" {
			t.Fatalf("want no fork (ok=false, no err), got bk=%q ok=%v err=%v", bk, ok, err)
		}
	})

	t.Run("delegated cannot branch", func(t *testing.T) {
		a := &Agent{DelegatedManager: &DelegatedManager{}}
		bk, ok, err := a.ForkSession(context.Background(), "agent/c123", session.BranchOptions{BranchType: "spawn"})
		if err != nil || ok || bk != "" {
			t.Fatalf("want no fork (ok=false, no err), got bk=%q ok=%v err=%v", bk, ok, err)
		}
	})
}
