package main

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
)

// gateProbeBackend is the branch-machinery double for the pending-work gate
// tests: noBranchBackend's synchronous turns plus an AwaitingAutonomousRun
// toggle (closing the parent session's gate exactly the way a real
// background-work window does — a started backend reporting pending work)
// and a probe capturing the parent's held state at the moment the turn runs.
type gateProbeBackend struct {
	noBranchBackend
	awaiting atomic.Bool
	probe    func() // called at the top of every ImmediateInject
}

// AwaitingAutonomousRun satisfies delegator.AutonomousRunAwaiter, so the
// manager's pending-work probe (and Agent.SystemInputHeld behind it) consults
// this double.
func (b *gateProbeBackend) AwaitingAutonomousRun() bool { return b.awaiting.Load() }

func (b *gateProbeBackend) ImmediateInject(ctx context.Context, inj delegator.Inject) error {
	if b.probe != nil {
		b.probe()
	}
	return b.noBranchBackend.ImmediateInject(ctx, inj)
}

// gateProbeForkBackend is gateProbeBackend's branch-capable twin: an honest
// delegator.BackendBrancher method set (with SessionID, so a fork of the
// parent finds a live backend session to clone) paired with capBranchSpec.
type gateProbeForkBackend struct {
	gateProbeBackend
	forks atomic.Int32
}

var _ delegator.BackendBrancher = (*gateProbeForkBackend)(nil)

// SessionID names the parent's live backend session, so ForkParentSession
// has something to fork (the plain double returns "" and the fork arm would
// fall back to an in-place inject).
func (b *gateProbeForkBackend) SessionID() string { return "parent-sid" }

func (b *gateProbeForkBackend) ForkSession(_ context.Context, _ delegator.ForkRequest) (delegator.ForkResult, error) {
	b.forks.Add(1)
	return delegator.ForkResult{SessionID: "forked-sid"}, nil
}

func (b *gateProbeForkBackend) CleanupSession(_ context.Context, _ delegator.CleanupRequest) error {
	return nil
}

// startedKeys returns the session keys whose backends were started, so a
// test can prove WHICH session a turn ran on (a delegated turn's content
// lives in the backend's own transcript, not the store).
func (b *gateProbeBackend) startedKeys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, o := range b.started {
		out = append(out, o.SessionKey)
	}
	return out
}

// hasPrefix reports whether any of keys starts with prefix.
func hasPrefix(keys []string, prefix string) bool {
	for _, k := range keys {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// sessionText returns every text block of every message in key's session
// file, so a test can prove the parent's own content is unchanged.
func sessionText(t *testing.T, d httpHandlerDeps, key string) string {
	t.Helper()
	msgs, err := d.sessions.Load(key)
	if err != nil {
		t.Fatalf("load %s: %v", key, err)
	}
	var b strings.Builder
	for _, m := range msgs {
		for _, blk := range m.Content {
			if blk.Text != "" {
				fmt.Fprintf(&b, "%s\n", blk.Text)
			}
		}
	}
	return b.String()
}

// TestBranchTurn_BackendForkRunsUnderParentGate pins requirement 4's branch
// ruling where it actually lives: a periodic pass whose strategy is
// BranchForkBackend runs its turn on a fork of the parent — handleDelegatedBranch
// never enqueues it on the parent's inbox — so the parent's pending-work gate
// (held for the whole test here) neither holds the pass nor sees it. The
// branch turn completes while the parent reads held, the parent's session and
// backend stay untouched, and the fork is real (the backend's ForkSession ran).
func TestBranchTurn_BackendForkRunsUnderParentGate(t *testing.T) {
	t.Parallel()
	be := &gateProbeForkBackend{}
	d := delegatedBranchHarnessWith(t, httpTestOpts{}, be, capBranchSpec)
	ag := d.agents[testAgentID].ag

	parent := testAgentID + "/i" + runToken("gate")
	seedMarker(t, d, parent, "parent marker")
	parentBefore := sessionText(t, d, parent)

	// Start the parent's backend and open its background-work window: the
	// parent's system input must read held for the rest of the test.
	if _, err := ag.DelegatedManager.Get(d.ctx, parent); err != nil {
		t.Fatalf("start parent backend: %v", err)
	}
	be.awaiting.Store(true)
	var heldDuringTurn atomic.Bool
	be.probe = func() { heldDuringTurn.Store(ag.SystemInputHeld(parent)) }
	if !ag.SystemInputHeld(parent) {
		t.Fatal("fixture: the parent must read held before the branch fires")
	}

	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()
	if !handleDelegatedBranch(ag, testAgentID, "reflection", parent, "reflect on the day", "", ctx) {
		t.Fatal("the forked branch turn failed while the parent gate was held — see the log")
	}

	if be.forks.Load() != 1 {
		t.Fatalf("backend forks = %d, want exactly 1 — the pass must take the real-fork arm, not an in-place inject", be.forks.Load())
	}
	if !heldDuringTurn.Load() {
		t.Fatal("the branch turn ran while the parent read unheld — the fixture lost the closed gate")
	}
	if inj := be.injectSnapshot(); len(inj) != 1 || !strings.Contains(inj[0], "reflect on the day") {
		t.Fatalf("backend injects = %q, want exactly the one branch turn carrying the prompt", inj)
	}
	if !hasPrefix(be.startedKeys(), parent+"/b") {
		t.Fatalf("no backend started for a branch of %s (started %q) — the pass did not run on a branch key", parent, be.startedKeys())
	}
	if after := sessionText(t, d, parent); after != parentBefore {
		t.Fatalf("the parent session changed under the branch turn:\nbefore: %q\nafter:  %q", parentBefore, after)
	}
}

// TestBranchTurn_RunIndependentRunsUnderParentGate pins the same ruling for
// the RunIndependent strategy (background passes on a backend that cannot
// fork): the turn runs on a fresh independent key via HandleMessage — never
// the parent's inbox — while the parent's pending-work gate is held.
func TestBranchTurn_RunIndependentRunsUnderParentGate(t *testing.T) {
	t.Parallel()
	be := &gateProbeBackend{}
	d := delegatedBranchHarnessWith(t, httpTestOpts{}, be, delegator.Spec{})
	ag := d.agents[testAgentID].ag

	parent := testAgentID + "/i" + runToken("gate")
	seedMarker(t, d, parent, "parent marker")
	parentBefore := sessionText(t, d, parent)

	if _, err := ag.DelegatedManager.Get(d.ctx, parent); err != nil {
		t.Fatalf("start parent backend: %v", err)
	}
	be.awaiting.Store(true)
	var heldDuringTurn atomic.Bool
	be.probe = func() { heldDuringTurn.Store(ag.SystemInputHeld(parent)) }
	if !ag.SystemInputHeld(parent) {
		t.Fatal("fixture: the parent must read held before the pass fires")
	}

	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()
	if !handleDelegatedBranch(ag, testAgentID, "background", parent, "check the todo list", "", ctx) {
		t.Fatal("the independent branch turn failed while the parent gate was held — see the log")
	}

	if !heldDuringTurn.Load() {
		t.Fatal("the branch turn ran while the parent read unheld — the fixture lost the closed gate")
	}
	if inj := be.injectSnapshot(); len(inj) != 1 || !strings.Contains(inj[0], "check the todo list") {
		t.Fatalf("backend injects = %q, want exactly the one independent turn carrying the prompt", inj)
	}
	if !hasPrefix(be.startedKeys(), testAgentID+"/ibackground-") {
		t.Fatalf("no backend started for a fresh %s/ibackground-* key (started %q) — the pass did not run on an independent key", testAgentID, be.startedKeys())
	}
	if after := sessionText(t, d, parent); after != parentBefore {
		t.Fatalf("the parent session changed under the independent turn:\nbefore: %q\nafter:  %q", parentBefore, after)
	}
}
