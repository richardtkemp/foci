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

// TestFailedExistingForkFallbackByBackend pins who may lose a parent's context
// when a fork of an existing conversation fails, against the real backend Specs
// (#2897). Codex (native ephemeral threads) refuses: its live-only source cannot
// be recovered, and a fresh branch would silently discard context. The backends
// that keep their own transcripts report "no fork", and the caller applies its
// fallback, as they did before the refusal existed.
func TestFailedExistingForkFallbackByBackend(t *testing.T) {
	for _, tc := range []struct {
		backend string
		refuses bool
	}{
		{"codex", true},
		{"claude-code", false},
		{"opencode", false},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			spec, ok := delegator.SpecFor(tc.backend)
			if !ok {
				t.Fatalf("no registered Spec for %q", tc.backend)
			}
			spec.ForkNeedsRunning = false // the failing fork under test needs no started server
			m, _ := newTestManager(t, newTestSessionIndex(t))
			m.Spec = spec
			m.NewBackend = func() (delegator.Delegator, error) { return &rejectingForkBackend{}, nil }
			const parent = "test/c1/b1"
			m.saveResumeID(parent, "known-parent")
			a := &Agent{DelegatedManager: m, Sessions: session.NewStore(t.TempDir())}

			bk, forked, err := a.ForkSession(context.Background(), parent, session.BranchOptions{BranchType: "depth"})
			if bk != "" || forked {
				t.Fatalf("ForkSession: key=%q forked=%v, want no fork", bk, forked)
			}
			if (err != nil) != tc.refuses {
				t.Fatalf("ForkSession: err=%v, refuses=%v", err, tc.refuses)
			}

			bk, inherited, err := a.ForkOrFreshBranch(context.Background(), parent, session.BranchOptions{BranchType: "depth"})
			if inherited {
				t.Fatalf("ForkOrFreshBranch: inherited=true from a failed fork (key=%q)", bk)
			}
			if tc.refuses {
				if err == nil || bk != "" {
					t.Fatalf("ForkOrFreshBranch: key=%q err=%v, want a refusal", bk, err)
				}
				return
			}
			if err != nil || bk == "" {
				t.Fatalf("ForkOrFreshBranch: key=%q err=%v, want a fresh branch", bk, err)
			}
		})
	}
}
