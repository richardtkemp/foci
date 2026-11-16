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

// failingBrancher fails every delete, so the sweep's error path can be
// asserted: a transcript that did not actually go must remain eligible.
type failingBrancher struct {
	mockBackendDM
	mu       sync.Mutex
	attempts int
}

func (f *failingBrancher) ForkSession(context.Context, delegator.ForkRequest) (delegator.ForkResult, error) {
	return delegator.ForkResult{SessionID: "x"}, nil
}

func (f *failingBrancher) CleanupSession(_ context.Context, _ delegator.CleanupRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	return errors.New("backend refused")
}

func TestCleanupEphemeralSessions(t *testing.T) {
	t.Parallel()
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
	idx.RecordBackendResume("alpha/c1/b100", "uuid-reflection")
	idx.Upsert(session.SessionIndexEntry{
		SessionKey: "alpha/c1/b200", CreatedAt: old, LastActivityAt: old,
		SessionType: session.SessionTypeSpawn,
	})
	idx.RecordBackendResume("alpha/c1/b200", "uuid-spawn")

	// Ephemeral type but recent → NOT cleaned (age gate).
	idx.Upsert(session.SessionIndexEntry{
		SessionKey: "alpha/c1/b300", CreatedAt: recent, LastActivityAt: recent,
		SessionType: session.SessionTypeKeepalive,
	})
	idx.RecordBackendResume("alpha/c1/b300", "uuid-recent")

	// Old but conversational type → NOT cleaned (type gate).
	idx.Upsert(session.SessionIndexEntry{
		SessionKey: "alpha/c2", CreatedAt: old, LastActivityAt: old,
		SessionType: session.SessionTypeChat,
	})
	idx.RecordBackendResume("alpha/c2", "uuid-chat")

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
	t.Parallel()
	a := &Agent{AgentID: "alpha"}
	if n := a.CleanupEphemeralSessions(context.Background(), 0); n != 0 {
		t.Errorf("disabled (0 days) returned %d, want 0", n)
	}
}

// TestCleanupEphemeral_DoesNotResweep is the #1801 defect. The sweep selects by
// row age and the rows are kept forever as a historical record, so the same
// expired sessions were re-selected every night. Every backend reports an
// already-absent transcript as success (ccstream: os.Remove + !IsNotExist;
// codex: "no rollout found for thread id"), so each re-attempt was counted
// again. The nightly figure was therefore "rows attempted", not "transcripts
// reclaimed", and could never answer the one question it exists to answer.
//
// Observed in production over three nights: codex logged "deleted 24" each
// night with 43 resume rows in total, and clutch logged 706 -> 757 -> 807 while
// holding ZERO transcripts older than 30 days on disk. A count that truly
// reclaimed could not re-report the previous night's total.
func TestCleanupEphemeral_DoesNotResweep(t *testing.T) {
	t.Parallel()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -60)
	for _, k := range []string{"alpha/c1/b100", "alpha/c1/b200"} {
		idx.Upsert(session.SessionIndexEntry{
			SessionKey: k, CreatedAt: old, LastActivityAt: old,
			SessionType: session.SessionTypeReflection,
		})
		idx.RecordBackendResume(k, "uuid-"+k)
	}

	rec := &recordingBrancher{}
	a := &Agent{AgentID: "alpha", SessionIndex: idx, DelegatedManager: &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) { return rec, nil }, SessionIndex: idx,
	}}

	// Night one does the real work.
	if n := a.CleanupEphemeralSessions(context.Background(), 30); n != 2 {
		t.Fatalf("first sweep deleted %d, want 2 — test premise broken", n)
	}
	rec.mu.Lock()
	first := len(rec.cleaned)
	rec.cleaned = nil
	rec.mu.Unlock()
	if first != 2 {
		t.Fatalf("first sweep cleaned %d ids, want 2 — test premise broken", first)
	}

	// Night two must find nothing left to do.
	if n := a.CleanupEphemeralSessions(context.Background(), 30); n != 0 {
		t.Errorf("second sweep deleted %d, want 0 — expired rows are being re-attempted", n)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.cleaned) != 0 {
		t.Errorf("second sweep re-cleaned %v, want nothing", rec.cleaned)
	}
}

// A transcript that FAILED to delete must stay eligible: marking it swept
// anyway would exempt a real, still-present file from collection forever, and
// silently — nothing downstream would ever look at it again.
func TestCleanupEphemeral_FailedDeleteStaysEligible(t *testing.T) {
	t.Parallel()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -60)
	idx.Upsert(session.SessionIndexEntry{
		SessionKey: "alpha/c1/b100", CreatedAt: old, LastActivityAt: old,
		SessionType: session.SessionTypeReflection,
	})
	idx.RecordBackendResume("alpha/c1/b100", "uuid-doomed")

	fb := &failingBrancher{}
	a := &Agent{AgentID: "alpha", SessionIndex: idx, DelegatedManager: &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) { return fb, nil }, SessionIndex: idx,
	}}

	if n := a.CleanupEphemeralSessions(context.Background(), 30); n != 0 {
		t.Fatalf("sweep counted %d despite the delete failing, want 0", n)
	}
	// Second sweep must try again rather than treat it as done.
	if n := a.CleanupEphemeralSessions(context.Background(), 30); n != 0 {
		t.Fatalf("second sweep counted %d, want 0", n)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if fb.attempts != 2 {
		t.Errorf("attempts = %d, want 2 — a failed delete must remain eligible", fb.attempts)
	}
}

// TestCleanupEphemeral_KeepsOpenChatTranscripts is #3134's ruling (Dick,
// 2026-11-16): "preserve transcripts for any open chat". A leaf bound to an
// open app chat keeps its transcript however old; a leaf whose chat is closed,
// in the same sweep, is reclaimed as before.
func TestCleanupEphemeral_KeepsOpenChatTranscripts(t *testing.T) {
	t.Parallel()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -60)
	for k, id := range map[string]string{"alpha/c1/b1": "uuid-b1", "alpha/c1/b2": "uuid-b2"} {
		idx.Upsert(session.SessionIndexEntry{
			SessionKey: k, CreatedAt: old, LastActivityAt: old,
			SessionType: session.SessionTypeDepth,
		})
		idx.RecordBackendResume(k, id)
	}
	// Two leaf conversations, as the app hub persists them; only b1's is open.
	for chatID, v := range map[int64][2]string{11: {"conv-b1", "alpha/c1/b1"}, 12: {"conv-b2", "alpha/c1/b2"}} {
		if err := idx.SetChatMetadata("alpha", "app", chatID, "conv_id", v[0]); err != nil {
			t.Fatal(err)
		}
		if err := idx.SetChatMetadata("alpha", "app", chatID, "session_key", v[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := idx.SetSystemState(session.SystemStateOpenChats, `["conv-b1"]`); err != nil {
		t.Fatal(err)
	}

	rec := &recordingBrancher{}
	mgr := &DelegatedManager{
		NewBackend:   func() (delegator.Delegator, error) { return rec, nil },
		SessionIndex: idx,
	}
	a := &Agent{AgentID: "alpha", SessionIndex: idx, DelegatedManager: mgr}

	if n := a.CleanupEphemeralSessions(context.Background(), 3); n != 1 {
		t.Errorf("deleted = %d, want 1 (the closed chat's leaf)", n)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.cleaned) != 1 || rec.cleaned[0] != "uuid-b2" {
		t.Errorf("cleaned = %v, want [uuid-b2]", rec.cleaned)
	}
	if got := idx.UnsweptResumes("alpha/c1/b1"); len(got) != 1 {
		t.Errorf("open chat's resume swept: unswept = %v", got)
	}
}

// TestCleanupEphemeral_UnreadableOpenSetDeletesNothing: a delete cannot be
// undone, so an open set that cannot be parsed skips the sweep rather than
// reading as "nothing open".
func TestCleanupEphemeral_UnreadableOpenSetDeletesNothing(t *testing.T) {
	t.Parallel()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -60)
	idx.Upsert(session.SessionIndexEntry{SessionKey: "alpha/c1/b1", CreatedAt: old, LastActivityAt: old, SessionType: session.SessionTypeDepth})
	idx.RecordBackendResume("alpha/c1/b1", "uuid-b1")
	if err := idx.SetSystemState(session.SystemStateOpenChats, `not json`); err != nil {
		t.Fatal(err)
	}
	rec := &recordingBrancher{}
	mgr := &DelegatedManager{
		NewBackend:   func() (delegator.Delegator, error) { return rec, nil },
		SessionIndex: idx,
	}
	a := &Agent{AgentID: "alpha", SessionIndex: idx, DelegatedManager: mgr}
	if n := a.CleanupEphemeralSessions(context.Background(), 3); n != 0 {
		t.Errorf("deleted = %d, want 0", n)
	}
	if got := idx.UnsweptResumes("alpha/c1/b1"); len(got) != 1 {
		t.Errorf("resume swept on an unreadable open set: unswept = %v", got)
	}
}

// TestCleanupEphemeral_KeepsTranscriptAnotherSessionUses: a branch that
// recorded its parent chat's resume id (live 2026-11-16, helen and polaris
// reflection branches) must not delete the chat's transcript. The branch's
// claim is released (swept) so it is not re-selected nightly; the chat's row
// stays unswept. A control branch with its own id is deleted as before.
func TestCleanupEphemeral_KeepsTranscriptAnotherSessionUses(t *testing.T) {
	t.Parallel()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -60)
	idx.Upsert(session.SessionIndexEntry{SessionKey: "alpha/c1", CreatedAt: old, LastActivityAt: old, SessionType: session.SessionTypeChat})
	idx.RecordBackendResume("alpha/c1", "uuid-chat")
	for k, id := range map[string]string{"alpha/c1/b1": "uuid-chat", "alpha/c1/b2": "uuid-own"} {
		idx.Upsert(session.SessionIndexEntry{SessionKey: k, CreatedAt: old, LastActivityAt: old, SessionType: session.SessionTypeReflection})
		idx.RecordBackendResume(k, id)
	}

	rec := &recordingBrancher{}
	mgr := &DelegatedManager{
		NewBackend:   func() (delegator.Delegator, error) { return rec, nil },
		SessionIndex: idx,
	}
	a := &Agent{AgentID: "alpha", SessionIndex: idx, DelegatedManager: mgr}

	if n := a.CleanupEphemeralSessions(context.Background(), 3); n != 1 {
		t.Errorf("deleted = %d, want 1 (the branch's own transcript)", n)
	}
	rec.mu.Lock()
	cleaned := append([]string(nil), rec.cleaned...)
	rec.mu.Unlock()
	if len(cleaned) != 1 || cleaned[0] != "uuid-own" {
		t.Errorf("cleaned = %v, want [uuid-own]", cleaned)
	}
	if got := idx.UnsweptResumes("alpha/c1/b1"); len(got) != 0 {
		t.Errorf("sharing branch not released: unswept = %v", got)
	}
	if got := idx.UnsweptResumes("alpha/c1"); len(got) != 1 {
		t.Errorf("chat's row swept: unswept = %v", got)
	}
}
