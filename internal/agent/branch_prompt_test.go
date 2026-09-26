package agent

import (
	"context"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/session"
)

// TestForkInheritsParentLaunchPrompt locks #2051: a backend fork launches with
// its parent PROCESS's launch prompt, not a prompt rebuilt from disk. The
// prompt cache is prefix-matched and the copied history sits after the system
// prompt, so a fork given a rebuilt prompt shares no cache with its parent once
// any prompt input changed after the parent launched (a reflection editing a
// skill description was enough) — and every keepalive fork then rewrote the
// whole conversation instead of refreshing the parent's entry.
func TestForkInheritsParentLaunchPrompt(t *testing.T) {
	const parent = "test-agent/c1"
	onDisk := "prompt v1"

	setup := func(t *testing.T) (*Agent, *DelegatedManager, *[]*brancherBackend) {
		t.Helper()
		var bes []*brancherBackend
		mgr := &DelegatedManager{
			NewBackend: func() (delegator.Delegator, error) {
				be := &brancherBackend{mockBackendDM{running: true}}
				bes = append(bes, be)
				return be, nil
			},
			StartOpts: delegator.StartOptions{
				WorkDir:          t.TempDir(),
				SystemPromptFunc: func(string) string { return onDisk },
			},
			AgentID:      "test-agent",
			SessionIndex: newTestSessionIndex(t),
			IdleTimeout:  time.Hour,
		}
		t.Cleanup(func() { mgr.Close() })
		mgr.saveResumeID(parent, "parent-uuid")
		a := &Agent{DelegatedManager: mgr, Sessions: session.NewStore(t.TempDir())}
		return a, mgr, &bes
	}
	fork := func(t *testing.T, a *Agent) string {
		t.Helper()
		bk, ok, err := a.ForkSession(context.Background(), parent, session.BranchOptions{BranchType: "spawn"})
		if err != nil || !ok {
			t.Fatalf("ForkSession: ok=%v err=%v", ok, err)
		}
		return bk
	}
	launched := func(t *testing.T, mgr *DelegatedManager, bes *[]*brancherBackend, key string) delegator.StartOptions {
		t.Helper()
		if _, err := mgr.Get(context.Background(), key); err != nil {
			t.Fatalf("Get %s: %v", key, err)
		}
		return (*bes)[len(*bes)-1].startOpts
	}

	t.Run("live parent: fork gets the parent's launch prompt after the disk prompt changed", func(t *testing.T) {
		onDisk = "prompt v1"
		a, mgr, bes := setup(t)
		launched(t, mgr, bes, parent)
		onDisk = "prompt v2 (a skill description was edited)"

		bk := fork(t, a)
		opts := launched(t, mgr, bes, bk)
		if opts.SystemPrompt != "prompt v1" {
			t.Errorf("fork launched with %q, want the parent's launch prompt %q", opts.SystemPrompt, "prompt v1")
		}
		if opts.SystemPromptFunc != nil {
			t.Errorf("SystemPromptFunc left set: a backend that resolves it itself (opencode) would override the inherited prompt")
		}

		// Inherited once: a later relaunch of the branch is an ordinary start.
		mgr.closeManaged(bk, false)
		if got := launched(t, mgr, bes, bk).SystemPrompt; got != onDisk {
			t.Errorf("branch relaunch got %q, want the rebuilt prompt %q", got, onDisk)
		}
	})

	t.Run("no live parent process: fork rebuilds, as the parent's own next turn will", func(t *testing.T) {
		onDisk = "prompt v1"
		a, mgr, bes := setup(t)
		onDisk = "prompt v2"

		bk := fork(t, a)
		if got := launched(t, mgr, bes, bk).SystemPrompt; got != "prompt v2" {
			t.Errorf("fork launched with %q, want the rebuilt prompt %q", got, "prompt v2")
		}
	})
}
