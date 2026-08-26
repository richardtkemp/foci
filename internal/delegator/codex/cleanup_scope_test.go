package codex

import (
	"context"
	"testing"

	"foci/internal/delegator"
)

// The whole of this bug was an absent interface. 1df68081 (2026-08-15) gave
// opencode a cleanup scope so an idle agent's expired sessions could still be
// collected; codex has the IDENTICAL requirement — it deletes a thread by RPC to
// a live app-server — but was left out, because docs/WIRING.md recorded codex as
// deleting straight from disk. It does not. The daily sweep therefore failed once
// per expired session, forever, for exactly the agents whose sessions had expired
// (24 such warnings in a single sweep on 2026-08-26).
//
// A compile-time assertion is the right shape here: the defect was never a wrong
// value, it was a type that did not satisfy a capability nobody checked.
func TestCodexAdvertisesCleanupCapabilities(t *testing.T) {
	var b any = &Backend{}
	if _, ok := b.(delegator.BackendBrancher); !ok {
		t.Fatal("codex must implement BackendBrancher, or CleanupSession is never reached at all")
	}
	if _, ok := b.(delegator.RunningBackendCleaner); !ok {
		t.Error("codex must implement RunningBackendCleaner: its CleanupSession is an RPC to a live app-server, " +
			"so without a scope the sweep builds a connectionless backend and fails once per session, permanently")
	}
}

// An empty agent id cannot address the pool, so it must fail loudly rather than
// silently acquire nothing and let every later delete fail on its own.
func TestOpenCleanupScope_RejectsEmptyAgentID(t *testing.T) {
	b := &Backend{}
	release, err := b.OpenCleanupScope(context.Background(), delegator.CleanupRequest{})
	if err == nil {
		t.Error("empty agent id was accepted — the scope would acquire nothing and report success")
	}
	if release != nil {
		t.Error("a failed scope returned a non-nil release")
	}
}

// pooledOwner is the half that makes the scope observable. The sweep builds a
// FRESH Backend per session (DelegatedManager.NewBackend), which has never run
// Start, so its shared pointer is nil and process() returns itself with no
// writer. Without this lookup a scope that successfully launched an app-server
// would still be invisible to every CleanupSession after it, and the fix would
// pass its own scope test while changing nothing in production.
func TestPooledOwner_NilWhenNothingPooledOrNotRunning(t *testing.T) {
	if got := pooledOwner(""); got != nil {
		t.Error("empty agent id resolved to an owner")
	}
	if got := pooledOwner("no-such-agent-" + t.Name()); got != nil {
		t.Error("unpooled agent resolved to an owner")
	}

	// A pooled entry that is NOT running must not be handed out: a dead owner's
	// writer is exactly the nil that produced the original error message.
	const agent = "cleanup-scope-test-agent"
	dead := &Backend{}
	sharedPool.Lock()
	sharedPool.servers[agent] = dead
	sharedPool.Unlock()
	t.Cleanup(func() {
		sharedPool.Lock()
		delete(sharedPool.servers, agent)
		sharedPool.Unlock()
	})

	if got := pooledOwner(agent); got != nil {
		t.Error("a pooled but non-running owner was handed out — CleanupSession would find a nil writer anyway")
	}
}
