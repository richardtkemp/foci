package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/session"
	"foci/internal/workspace"
)

// bgWorkBackend is a mock backend whose background work (running subagents,
// awaiting an autonomous run) can be changed while a test runs.
type bgWorkBackend struct {
	*mockBackendDM
	bgMu     sync.Mutex
	running  []delegator.RunningSubagent
	awaiting bool
}

func (b *bgWorkBackend) RunningSubagents() []delegator.RunningSubagent {
	b.bgMu.Lock()
	defer b.bgMu.Unlock()
	return b.running
}

func (b *bgWorkBackend) AwaitingAutonomousRun() bool {
	b.bgMu.Lock()
	defer b.bgMu.Unlock()
	return b.awaiting
}

func (b *bgWorkBackend) set(running []delegator.RunningSubagent, awaiting bool) {
	b.bgMu.Lock()
	b.running, b.awaiting = running, awaiting
	b.bgMu.Unlock()
}

// newBgWorkManager returns a manager whose single session sk is live on a
// bgWorkBackend, and whose on-disk prompt has changed since launch (so the
// post-compaction reload gate wants to restart CC).
func newBgWorkManager(t *testing.T, sk string) (*DelegatedManager, *bgWorkBackend) {
	t.Helper()
	var be *bgWorkBackend
	mgr, _ := newTestManager(t, newTestSessionIndex(t))
	mgr.NewBackend = func() (delegator.Delegator, error) {
		be = &bgWorkBackend{mockBackendDM: &mockBackendDM{running: true}}
		return be, nil
	}
	mgr.DeferredBouncePoll = 5 * time.Millisecond
	prompt := "v1"
	mgr.StartOpts.SystemPromptFunc = func(string) string { return prompt }
	if _, err := mgr.Get(context.Background(), sk); err != nil {
		t.Fatalf("Get: %v", err)
	}
	prompt = "v2 — a skill was added"
	return mgr, be
}

// waitClosed waits for be to be closed; the deadline is only a hang guard.
func waitClosed(t *testing.T, be *bgWorkBackend) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !be.wasClosed() {
		if time.Now().After(deadline) {
			t.Fatal("deferred bounce never closed the backend after background work finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestBounceAfterCompaction_DefersWhileSubagentsRun: the post-compaction
// reload restart kills CC's in-process background subagents (#1539), so it
// waits until they finish, then restarts.
func TestBounceAfterCompaction_DefersWhileSubagentsRun(t *testing.T) {
	const sk = "test-agent/c1"
	mgr, be := newBgWorkManager(t, sk)
	be.set([]delegator.RunningSubagent{{ID: "t1", Description: "review", Kind: delegator.SubagentKindAgent}}, false)

	if mgr.BounceAfterCompaction(sk, false) {
		t.Fatal("bounced immediately while a subagent was running")
	}
	if be.wasClosed() {
		t.Fatal("backend closed while a subagent was running")
	}
	if _, ok := mgr.getManaged(sk); !ok {
		t.Fatal("backend unmapped while a subagent was running")
	}

	be.set(nil, false)
	waitClosed(t, be)
	if _, ok := mgr.getManaged(sk); ok {
		t.Error("backend still mapped after the deferred bounce")
	}
}

// TestBounceAfterCompaction_DefersWhileAwaitingAutonomousRun: the
// background-work window also covers a pending/live autonomous run.
func TestBounceAfterCompaction_DefersWhileAwaitingAutonomousRun(t *testing.T) {
	const sk = "test-agent/c1"
	mgr, be := newBgWorkManager(t, sk)
	be.set(nil, true)

	if mgr.BounceAfterCompaction(sk, false) {
		t.Fatal("bounced immediately while an autonomous run was pending")
	}
	if be.wasClosed() {
		t.Fatal("backend closed while an autonomous run was pending")
	}
	be.set(nil, false)
	waitClosed(t, be)
}

// TestBounceAfterCompaction_ForceIgnoresSubagents: /compact force restarts CC
// at once even though that kills the running subagents (Dick's correction).
func TestBounceAfterCompaction_ForceIgnoresSubagents(t *testing.T) {
	const sk = "test-agent/c1"
	mgr, be := newBgWorkManager(t, sk)
	be.set([]delegator.RunningSubagent{{ID: "t1", Kind: delegator.SubagentKindAgent}}, true)

	if !mgr.BounceAfterCompaction(sk, true) {
		t.Fatal("forced bounce did not restart despite a changed prompt")
	}
	if !be.wasClosed() {
		t.Error("forced bounce left the backend open")
	}
}

// TestBounceAfterCompaction_IdleBouncesNow: with no background work the gate
// behaves exactly as before.
func TestBounceAfterCompaction_IdleBouncesNow(t *testing.T) {
	const sk = "test-agent/c1"
	mgr, be := newBgWorkManager(t, sk)
	if !mgr.BounceAfterCompaction(sk, false) {
		t.Fatal("idle session with a changed prompt did not bounce")
	}
	if !be.wasClosed() {
		t.Error("bounce left the backend open")
	}
}

// TestCloseManagedExact_SkipsReplacedBackend: the deferred bounce must not
// close a backend that replaced the one it was waiting on.
func TestCloseManagedExact_SkipsReplacedBackend(t *testing.T) {
	const sk = "test-agent/c1"
	mgr, old := newBgWorkManager(t, sk)
	oldMB, _ := mgr.getManaged(sk)
	mgr.BounceSession(sk)
	if _, err := mgr.Get(context.Background(), sk); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if mgr.closeManagedExact(sk, oldMB, false) {
		t.Fatal("closed the replacement backend")
	}
	cur, ok := mgr.getManaged(sk)
	if !ok || cur == oldMB || cur.be.(*bgWorkBackend) == old {
		t.Fatal("replacement backend was unmapped")
	}
}

// bgCompactAgent is a delegated agent whose session sk runs on a
// bgWorkBackend with one running subagent, a changed on-disk prompt, and
// reload_on_compact on. sent records commands sent to the backend.
func bgCompactAgent(t *testing.T, sk string) (*Agent, *bgWorkBackend, *[]string) {
	t.Helper()
	mgr, be := newBgWorkManager(t, sk)
	var sent []string
	var mu sync.Mutex
	be.sendCommandFn = func(_ context.Context, cmd string) error {
		mu.Lock()
		sent = append(sent, cmd)
		mu.Unlock()
		return nil
	}
	be.set([]delegator.RunningSubagent{{ID: "t1", Description: "review the parser", Kind: delegator.SubagentKindAgent, Started: time.Now()}}, false)
	ag := &Agent{
		Sessions:         session.NewStore(t.TempDir()),
		Bootstrap:        workspace.NewBootstrap(t.TempDir(), []string{}),
		DelegatedManager: mgr,
		ReloadOnCompact:  true,
	}
	return ag, be, &sent
}

// TestCompactSession_RefusesWhileSubagentsRun: a plain manual /compact is
// refused while background work runs, naming it, and sends nothing to CC.
func TestCompactSession_RefusesWhileSubagentsRun(t *testing.T) {
	const sk = "test-agent/c1"
	ag, be, sent := bgCompactAgent(t, sk)

	_, err := ag.CompactSession(context.Background(), sk, false)
	var bw *BackgroundWorkError
	if !errors.As(err, &bw) {
		t.Fatalf("CompactSession err = %v, want *BackgroundWorkError", err)
	}
	if !strings.Contains(bw.Running, "subagents: 1 (review the parser") {
		t.Errorf("refusal names %q, want the running subagent", bw.Running)
	}
	if len(*sent) != 0 {
		t.Errorf("refused compaction still sent %q", *sent)
	}
	if be.wasClosed() {
		t.Error("refused compaction closed the backend")
	}
}

// TestCompactSession_AutonomousRunOnlyIsRefused: the pending autonomous run
// alone (no listed subagent) still refuses, with a description.
func TestCompactSession_AutonomousRunOnlyIsRefused(t *testing.T) {
	const sk = "test-agent/c1"
	ag, be, _ := bgCompactAgent(t, sk)
	be.set(nil, true)
	_, err := ag.CompactSession(context.Background(), sk, false)
	var bw *BackgroundWorkError
	if !errors.As(err, &bw) || bw.Running == "" {
		t.Fatalf("CompactSession err = %v, want a described *BackgroundWorkError", err)
	}
}

// TestCompactSessionForce_CompactsAndRestartsNow: /compact force ignores the
// subagents: it compacts and restarts CC at once (killing them).
func TestCompactSessionForce_CompactsAndRestartsNow(t *testing.T) {
	const sk = "test-agent/c1"
	ag, be, sent := bgCompactAgent(t, sk)

	if _, err := ag.CompactSessionForce(context.Background(), sk); err != nil {
		t.Fatalf("CompactSessionForce: %v", err)
	}
	if len(*sent) != 1 || !strings.HasPrefix((*sent)[0], "/compact ") {
		t.Errorf("sent %q, want one /compact", *sent)
	}
	if !be.wasClosed() {
		t.Error("forced compaction did not restart CC despite a changed prompt")
	}
}

// TestBounceAfterCompaction_UnchangedPromptRearmsDeferral: a deferred bounce
// that finds the prompt unchanged once work finishes must clear its
// "already waiting" mark, or every later compaction on that backend is
// skipped as "already waiting" and never restarts CC (#1539 review).
func TestBounceAfterCompaction_UnchangedPromptRearmsDeferral(t *testing.T) {
	const sk = "test-agent/c1"
	mgr, be := newBgWorkManager(t, sk)
	var live atomic.Value
	live.Store("v1") // same as launch: the first deferred bounce finds nothing to reload
	mgr.StartOpts.SystemPromptFunc = func(string) string { return live.Load().(string) }
	mb, ok := mgr.getManaged(sk)
	if !ok {
		t.Fatal("no managed backend")
	}

	be.set([]delegator.RunningSubagent{{ID: "t1", Kind: delegator.SubagentKindAgent}}, false)
	if mgr.BounceAfterCompaction(sk, false) {
		t.Fatal("bounced immediately while a subagent was running")
	}
	be.set(nil, false)
	deadline := time.Now().Add(10 * time.Second)
	for mb.bounceDeferred.Load() {
		if time.Now().After(deadline) {
			t.Fatal("deferred bounce with an unchanged prompt never cleared its waiting mark")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if be.wasClosed() {
		t.Fatal("closed the backend although the prompt was unchanged")
	}

	// A later compaction, with the prompt now changed and work running again,
	// must defer anew and restart once the work finishes.
	live.Store("v2 — a skill was added")
	be.set([]delegator.RunningSubagent{{ID: "t2", Kind: delegator.SubagentKindAgent}}, false)
	if mgr.BounceAfterCompaction(sk, false) {
		t.Fatal("bounced immediately while a subagent was running")
	}
	be.set(nil, false)
	waitClosed(t, be)
}
