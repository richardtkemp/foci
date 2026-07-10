package agent

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/session"
)

// recordingBrancher is a BackendBrancher that records the session ids passed to
// CleanupSession, so the cleanup orchestration can be asserted without touching
// the filesystem (ccstream's real delete is covered in its own package test).
type recordingBrancher struct {
	mockBackendDM
	mu      sync.Mutex
	cleaned []string
}

func (r *recordingBrancher) ForkSession(context.Context, delegator.ForkRequest) (delegator.ForkResult, error) {
	return delegator.ForkResult{SessionID: "x"}, nil
}

func (r *recordingBrancher) CleanupSession(_ context.Context, req delegator.CleanupRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleaned = append(r.cleaned, req.SessionID)
	return nil
}

func TestCleanupEphemeralSessions(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	old := time.Now().AddDate(0, 0, -60)
	recent := time.Now()

	// Old + ephemeral type → SHOULD be cleaned.
	idx.Upsert(session.SessionIndexEntry{
		SessionKey: "alpha/c1/b100", CreatedAt: old, LastActivityAt: old,
		SessionType: session.SessionTypeReflection,
	})
	idx.RecordCCResume("alpha/c1/b100", "uuid-reflection")
	idx.Upsert(session.SessionIndexEntry{
		SessionKey: "alpha/c1/b200", CreatedAt: old, LastActivityAt: old,
		SessionType: session.SessionTypeSpawn,
	})
	idx.RecordCCResume("alpha/c1/b200", "uuid-spawn")

	// Ephemeral type but recent → NOT cleaned (age gate).
	idx.Upsert(session.SessionIndexEntry{
		SessionKey: "alpha/c1/b300", CreatedAt: recent, LastActivityAt: recent,
		SessionType: session.SessionTypeKeepalive,
	})
	idx.RecordCCResume("alpha/c1/b300", "uuid-recent")

	// Old but conversational type → NOT cleaned (type gate).
	idx.Upsert(session.SessionIndexEntry{
		SessionKey: "alpha/c2", CreatedAt: old, LastActivityAt: old,
		SessionType: session.SessionTypeChat,
	})
	idx.RecordCCResume("alpha/c2", "uuid-chat")

	rec := &recordingBrancher{}
	mgr := &DelegatedManager{
		NewBackend:   func() (delegator.Delegator, error) { return rec, nil },
		SessionIndex: idx,
	}
	a := &Agent{AgentID: "alpha", SessionIndex: idx, DelegatedManager: mgr}

	n := a.CleanupEphemeralSessions(context.Background(), 30)
	if n != 2 {
		t.Errorf("deleted count = %d, want 2", n)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	got := map[string]bool{}
	for _, id := range rec.cleaned {
		got[id] = true
	}
	if !got["uuid-reflection"] || !got["uuid-spawn"] || len(rec.cleaned) != 2 {
		t.Errorf("cleaned = %v, want {uuid-reflection, uuid-spawn}", rec.cleaned)
	}
}

func TestCleanupEphemeralSessionsDisabled(t *testing.T) {
	a := &Agent{AgentID: "alpha"}
	if n := a.CleanupEphemeralSessions(context.Background(), 0); n != 0 {
		t.Errorf("disabled (0 days) returned %d, want 0", n)
	}
}
