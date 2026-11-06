package agent

import (
	"context"
	"errors"
	"foci/internal/delegator"
	"foci/internal/session"
	"testing"
	"time"
)

// ephemeralManager uses the normal manager fixture with native ephemeral support declared.
func ephemeralManager(t *testing.T, idx *session.SessionIndex) (*DelegatedManager, *[]*mockBackendDM) {
	t.Helper()
	m, mocks := newTestManager(t, idx)
	m.Spec = delegator.Spec{Caps: map[delegator.Capability]delegator.Support{delegator.CapEphemeralSessions: delegator.Yes()}}
	return m, mocks
}

// TestEphemeralIDsNeverPromiseColdResume checks durable lifetime, in-memory IDs and restart refusal.
func TestEphemeralIDsNeverPromiseColdResume(t *testing.T) {
	idx := newTestSessionIndex(t)
	m, _ := ephemeralManager(t, idx)
	const key = "test/c123/b456"
	if err := m.markEphemeralSession(key, "live-fork"); err != nil {
		t.Fatal(err)
	}
	m.saveResumeID(key, "live-fork")
	if id := m.loadResumeID(key); id != "live-fork" {
		t.Fatal(id)
	}
	if id, _ := idx.GetSessionMetadata(key, session.MetaKeyCCResumeID); id != "" {
		t.Fatalf("persisted live-only ID %q", id)
	}
	restarted, mocks := ephemeralManager(t, idx)
	if _, err := restarted.Get(context.Background(), key); !errors.Is(err, ErrEphemeralSessionLost) {
		t.Fatalf("restart: %v", err)
	}
	if len(*mocks) != 1 {
		t.Fatalf("unexpected retry count %d", len(*mocks))
	}
}

// TestEphemeralManagerPinsIdleButClosesFinished keeps depth context alive without leaking completed workers.
func TestEphemeralManagerPinsIdleButClosesFinished(t *testing.T) {
	m, _ := ephemeralManager(t, newTestSessionIndex(t))
	const key = "test/c123/b789"
	if err := m.markEphemeralSession(key, "live-fork"); err != nil {
		t.Fatal(err)
	}
	be, err := m.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	m.backends[key].lastActive = time.Now().Add(-24 * time.Hour)
	m.closeIdle(time.Second)
	if be.(*mockBackendDM).closed || len(m.idleCandidates()) != 0 {
		t.Fatal("ephemeral context was evicted")
	}
	if !m.CloseFinishedSession(key, "test complete") {
		t.Fatal("finished ephemeral worker did not close")
	}
}

// TestEphemeralAdoptionFailureDoesNotStartEmpty rejects the durable-resume fallback for live-only forks.
func TestEphemeralAdoptionFailureDoesNotStartEmpty(t *testing.T) {
	m, _ := ephemeralManager(t, newTestSessionIndex(t))
	const key = "test/c123/b987"
	if err := m.markEphemeralSession(key, "lost-fork"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m.NewBackend = func() (delegator.Delegator, error) {
		calls++
		return &mockBackendDM{startErr: errors.New("fork gone")}, nil
	}
	if _, err := m.Get(context.Background(), key); err == nil {
		t.Fatal("missing fork succeeded")
	}
	if calls != 1 {
		t.Fatalf("retried missing ephemeral fork %d times", calls)
	}
}

// TestBatchBackendLifetimes checks utility vs fresh spawn policy at the real Start boundary.
func TestBatchBackendLifetimes(t *testing.T) {
	for _, purpose := range []string{delegator.BatchPurposeSummary, delegator.BatchPurposeSpawnCharacter, delegator.BatchPurposeSpawnRaw, delegator.BatchPurposeSpawnExplore} {
		t.Run(purpose, func(t *testing.T) {
			m, _ := ephemeralManager(t, newTestSessionIndex(t))
			m.RunBatchTurn = func(ctx context.Context, key, _, _ string) (string, error) {
				be, err := m.Get(ctx, key)
				if err != nil {
					return "", err
				}
				if got := be.(*mockBackendDM).startOpts.Ephemeral; got != !isSpawnPurpose(purpose) {
					t.Errorf("ephemeral=%v for %s", got, purpose)
				}
				return "ok", nil
			}
			if _, err := m.RunBatch(context.Background(), delegator.BatchRequest{Purpose: purpose, AgentID: "test", OwnerSessionKey: "test/c1", Prompt: "test"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestRemapPreservesEphemeralLifetime prevents session-end memory persisting a live-only resume ID.
func TestRemapPreservesEphemeralLifetime(t *testing.T) {
	idx := newTestSessionIndex(t)
	m, _ := ephemeralManager(t, idx)
	if err := m.markEphemeralSession("test/c1/b1", "live-only"); err != nil {
		t.Fatal(err)
	}
	if !m.RemapSession("test/c1/b1", "test/c1/b2") {
		t.Fatal("remap failed")
	}
	if !m.isEphemeralSession("test/c1/b2") || m.loadResumeID("test/c1/b2") != "live-only" {
		t.Fatal("lifetime/identity lost")
	}
	if id, _ := idx.GetSessionMetadata("test/c1/b2", session.MetaKeyCCResumeID); id != "" {
		t.Fatalf("persisted %s", id)
	}
}

type rejectingForkBackend struct{ brancherBackend }

func (*rejectingForkBackend) ForkSession(context.Context, delegator.ForkRequest) (delegator.ForkResult, error) {
	return delegator.ForkResult{}, errors.New("no rollout found for live-only source")
}

// TestFailedExistingForkNeverBecomesFresh prevents unsupported nesting silently discarding context.
func TestFailedExistingForkNeverBecomesFresh(t *testing.T) {
	m, _ := newTestManager(t, newTestSessionIndex(t))
	m.Spec = branchSpec
	m.NewBackend = func() (delegator.Delegator, error) { return &rejectingForkBackend{}, nil }
	m.saveResumeID("test/c1/b1", "known-live-only")
	a := &Agent{DelegatedManager: m}
	key, inherited, err := a.ForkOrFreshBranch(context.Background(), "test/c1/b1", session.BranchOptions{BranchType: "depth"})
	if err == nil || key != "" || inherited {
		t.Fatalf("key=%q inherited=%v err=%v", key, inherited, err)
	}
}
