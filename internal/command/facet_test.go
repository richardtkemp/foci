package command

import (
	"context"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/session"
)

// facetBrancherBackend is a delegator.Delegator that also implements
// delegator.BackendBrancher, so a DelegatedManager built with it reports
// BackendCanBranch() == true.
type facetBrancherBackend struct{ delegator.Delegator }

func (facetBrancherBackend) ForkSession(context.Context, delegator.ForkRequest) (delegator.ForkResult, error) {
	return delegator.ForkResult{}, nil
}
func (facetBrancherBackend) CleanupSession(context.Context, delegator.CleanupRequest) error {
	return nil
}

// TestForkFacetBranch covers the two delegated cases a facet must tell apart. A
// facet IS a branch, so there is no degraded form of it: a backend that cannot
// branch must refuse, while a branch-capable backend whose parent simply has
// nothing to clone still produces a real branch.
//
// Both used to take the same path — a plain history-reading branch — so asking
// an unsupportable agent for a facet looked like it had worked.
func TestForkFacetBranch(t *testing.T) {
	newCC := func(t *testing.T, mgr *agent.DelegatedManager) CommandContext {
		t.Helper()
		store := session.NewStore(t.TempDir())
		return CommandContext{
			Agent:       &agent.Agent{Sessions: store, DelegatedManager: mgr},
			Sessions:    store,
			Config:      &config.Config{},
			AgentConfig: config.AgentConfig{},
		}
	}
	opts := session.BranchOptions{BranchType: "facet"}

	t.Run("backend cannot branch — refuses instead of inventing a session", func(t *testing.T) {
		// No NewBackend → BackendCanBranch() == false.
		cc := newCC(t, &agent.DelegatedManager{})
		key, err := forkFacetBranch(context.Background(), cc, "agent/c123", opts)
		if err == nil {
			t.Fatalf("want an error, got branch key %q", key)
		}
		if key != "" {
			t.Errorf("branch key = %q, want empty on refusal", key)
		}
		if !strings.Contains(err.Error(), "cannot branch") {
			t.Errorf("error should say the backend cannot branch, got %q", err)
		}
	})

	t.Run("backend can branch, parent has nothing to clone — real branch", func(t *testing.T) {
		mgr := &agent.DelegatedManager{
			NewBackend: func() (delegator.Delegator, error) { return facetBrancherBackend{}, nil },
		}
		cc := newCC(t, mgr)
		key, err := forkFacetBranch(context.Background(), cc, "agent/c123", opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.HasPrefix(key, "agent/c123/b") {
			t.Errorf("branch key = %q, want a branch of agent/c123", key)
		}
	})
}
