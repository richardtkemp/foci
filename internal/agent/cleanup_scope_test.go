package agent

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/session"
)

// scopingBrancher is a recordingBrancher that also implements
// delegator.RunningBackendCleaner, so a sweep's resource lifetime can be
// asserted: how many times the scope was acquired/released, and — the part
// that actually matters — whether it was OPEN at the moment each delete ran.
type scopingBrancher struct {
	mockBackendDM
	mu         sync.Mutex
	cleaned    []string
	openDuring []bool // scope state observed by each CleanupSession call
	acquires   int
	releases   int
	open       bool
	openErr    error
}

func (s *scopingBrancher) ForkSession(context.Context, delegator.ForkRequest) (delegator.ForkResult, error) {
	return delegator.ForkResult{SessionID: "x"}, nil
}

func (s *scopingBrancher) CleanupSession(_ context.Context, req delegator.CleanupRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleaned = append(s.cleaned, req.SessionID)
	s.openDuring = append(s.openDuring, s.open)
	return nil
}

func (s *scopingBrancher) OpenCleanupScope(_ context.Context, _ delegator.CleanupRequest) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.openErr != nil {
		return nil, s.openErr
	}
	s.acquires++
	s.open = true
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.releases++
		s.open = false
	}, nil
}

// expiredIndex builds a session index holding n expired ephemeral sessions
// (plus one recent session that must never be swept).
func expiredIndex(t *testing.T, n int) *session.SessionIndex {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -60)
	for i := 0; i < n; i++ {
		key := "alpha/c1/b" + string(rune('A'+i))
		idx.Upsert(session.SessionIndexEntry{
			SessionKey: key, CreatedAt: old, LastActivityAt: old,
			SessionType: session.SessionTypeReflection,
		})
		idx.RecordBackendResume(key, "uuid-"+string(rune('A'+i)))
	}
	now := time.Now()
	idx.Upsert(session.SessionIndexEntry{
		SessionKey: "alpha/c1/bRecent", CreatedAt: now, LastActivityAt: now,
		SessionType: session.SessionTypeKeepalive,
	})
	idx.RecordBackendResume("alpha/c1/bRecent", "uuid-recent")
	return idx
}

func agentFor(idx *session.SessionIndex, be delegator.Delegator) *Agent {
	mgr := &DelegatedManager{
		NewBackend:   func() (delegator.Delegator, error) { return be, nil },
		SessionIndex: idx,
	}
	return &Agent{AgentID: "alpha", SessionIndex: idx, DelegatedManager: mgr}
}

// The point of the change (#1707): a sweep of N sessions costs ONE acquire, and
// every delete happens while that scope is held. Per-session acquisition would
// spawn and tear down an opencode server for each expired session.
func TestCleanupEphemeral_ScopeAcquiredOncePerSweep(t *testing.T) {
	const n = 4
	sb := &scopingBrancher{}
	a := agentFor(expiredIndex(t, n), sb)

	if got := a.CleanupEphemeralSessions(context.Background(), 30); got != n {
		t.Fatalf("deleted = %d, want %d", got, n)
	}

	sb.mu.Lock()
	defer sb.mu.Unlock()
	if len(sb.cleaned) != n {
		t.Fatalf("cleaned %d sessions, want %d — test premise broken", len(sb.cleaned), n)
	}
	if sb.acquires != 1 {
		t.Errorf("acquires = %d across %d deletes, want exactly 1", sb.acquires, n)
	}
	if sb.releases != 1 {
		t.Errorf("releases = %d, want exactly 1 (scope leaked or double-released)", sb.releases)
	}
	for i, wasOpen := range sb.openDuring {
		if !wasOpen {
			t.Errorf("delete %d (%s) ran with the scope closed", i, sb.cleaned[i])
		}
	}
}

// A sweep with nothing to delete must not acquire — otherwise the daily GC
// spawns an opencode server on every quiet day for no reason.
func TestCleanupEphemeral_NoScopeWhenNothingExpired(t *testing.T) {
	sb := &scopingBrancher{}
	a := agentFor(expiredIndex(t, 0), sb)

	if got := a.CleanupEphemeralSessions(context.Background(), 30); got != 0 {
		t.Fatalf("deleted = %d, want 0", got)
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.acquires != 0 {
		t.Errorf("acquires = %d with nothing expired, want 0", sb.acquires)
	}
}

// A scope that can't be acquired must not abort the sweep: the deletes still
// run and fail (or succeed) individually, which is the pre-scope behaviour.
func TestCleanupEphemeral_ProceedsWhenScopeUnavailable(t *testing.T) {
	sb := &scopingBrancher{openErr: errors.New("no server")}
	a := agentFor(expiredIndex(t, 2), sb)

	if got := a.CleanupEphemeralSessions(context.Background(), 30); got != 2 {
		t.Fatalf("deleted = %d, want 2 — a failed scope must not abort the sweep", got)
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.releases != 0 {
		t.Errorf("releases = %d after a failed acquire, want 0", sb.releases)
	}
}
