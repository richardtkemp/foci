package agent

import (
	"context"
	"path/filepath"
	"testing"

	"foci/internal/account"
	"foci/internal/delegator"
	"foci/internal/relogin"
	"foci/internal/session"
	"foci/internal/usagehold"
)

// TestForkFollowsItsSourceFreshChildTakesThePick (#3246, Dick 2026-11-21
// 11:47: "if we want to re-use an existing warm cache (i.e. if we fork/copy
// transcript) then we must use the same account"): with the parent stored on
// B and the pick A, a transcript fork of the parent is on B (its clone is in
// B's folder), while a fresh branch and a character spawn branch, which copy
// nothing, are on the pick A.
func TestForkFollowsItsSourceFreshChildTakesThePick(t *testing.T) {
	t.Parallel()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	mk := func(key string) *account.Account {
		return account.New(account.Options{Backend: "claude-code", Holds: usagehold.New(), Login: &relogin.Gate{},
			Spec: delegator.AccountSpec{Key: key, Name: "/cc/" + key}})
	}
	accA, accB := mk("A"), mk("B")
	const forkable, empty = "agent/c1", "agent/c2"
	for _, k := range []string{forkable, empty} {
		if err := idx.SetSessionMetadata(k, session.MetaKeyAccount, "B"); err != nil {
			t.Fatal(err)
		}
	}
	mgr := &DelegatedManager{NewBackend: func() (delegator.Delegator, error) { return &brancherBackend{}, nil }, SessionIndex: idx, Spec: branchSpec}
	mgr.Accounts = account.NewPool(idx, accA, accB)
	a := &Agent{Sessions: session.NewStore(t.TempDir()), DelegatedManager: mgr}
	mgr.saveResumeID(forkable, "parent-cc-session") // what makes the parent forkable

	fork, inherited, err := a.ForkOrFreshBranch(context.Background(), forkable, session.BranchOptions{BranchType: "branch"})
	if err != nil || fork == "" || !inherited {
		t.Fatalf("fork: bk=%q inherited=%v err=%v", fork, inherited, err)
	}
	if got := a.AccountFor(fork); got != accB {
		t.Errorf("transcript fork on %s, want its parent's B", got.Key())
	}
	if v, _ := idx.GetSessionMetadata(fork, session.MetaKeyAccount); v != "B" {
		t.Errorf("fork stored on %q, want B", v)
	}

	fresh, inherited, err := a.ForkOrFreshBranch(context.Background(), empty, session.BranchOptions{BranchType: "branch"})
	if err != nil || fresh == "" || inherited {
		t.Fatalf("fresh: bk=%q inherited=%v err=%v", fresh, inherited, err)
	}
	if got := a.AccountFor(fresh); got != accA {
		t.Errorf("fresh branch on %s, want the pick A", got.Key())
	}

	spawn, err := a.NewCharacterSpawnBranch(forkable, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := a.AccountFor(spawn); got != accA {
		t.Errorf("character spawn branch on %s, want the pick A", got.Key())
	}

	// A fork of the character spawn follows the spawn (A), not the root (B).
	mgr.saveResumeID(spawn, "spawn-cc-session")
	forkOfSpawn, inherited, err := a.ForkOrFreshBranch(context.Background(), spawn, session.BranchOptions{BranchType: "branch"})
	if err != nil || forkOfSpawn == "" || !inherited {
		t.Fatalf("fork of spawn: bk=%q inherited=%v err=%v", forkOfSpawn, inherited, err)
	}
	if got := a.AccountFor(forkOfSpawn); got != accA {
		t.Errorf("fork of the character spawn on %s, want its source's A", got.Key())
	}
}
