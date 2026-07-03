package agent

import (
	"path/filepath"
	"testing"

	"foci/internal/session"
)

func TestClearSessionState(t *testing.T) {
	// Proves that ClearSessionState drops ALL per-session state for a stable
	// key after its history is reset: in-memory overrides (effort, no_compact),
	// the turn lock, and every session_metadata row (including cc_resume_id).
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ag := &Agent{SessionIndex: idx}

	key := "bot/c100"

	// Set up per-session state.
	ag.SetSessionEffort(key, "high")
	ag.SetSessionNoCompact(key, true)
	if err := idx.SetSessionMetadata(key, "cc_resume_id", "uuid-999"); err != nil {
		t.Fatal(err)
	}
	oldLock := ag.turnLock(key)

	ag.ClearSessionState(key)

	// In-memory overrides fall back to defaults.
	if got := ag.SessionEffort(key); got != "" {
		t.Errorf("effort after clear = %q, want empty", got)
	}
	if ag.SessionNoCompact(key) {
		t.Error("no_compact should be false after clear")
	}

	// The turn lock entry is dropped — a fresh lock is created on next use.
	if ag.turnLock(key) == oldLock {
		t.Error("turn lock should be a fresh object after clear")
	}

	// ALL session_metadata rows are gone.
	for _, k := range []string{"effort", "no_compact", "cc_resume_id"} {
		if v, _ := idx.GetSessionMetadata(key, k); v != "" {
			t.Errorf("metadata row %q survived clear: %q", k, v)
		}
	}
}

func TestClearSessionState_OtherSessionsUntouched(t *testing.T) {
	// Proves ClearSessionState is scoped to one session key: clearing one
	// session's state must not disturb a sibling session's overrides or
	// metadata rows.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ag := &Agent{SessionIndex: idx}

	victim := "bot/c100"
	bystander := "bot/c200"

	ag.SetSessionEffort(victim, "high")
	ag.SetSessionEffort(bystander, "low")

	ag.ClearSessionState(victim)

	if got := ag.SessionEffort(bystander); got != "low" {
		t.Errorf("bystander effort = %q, want low", got)
	}
	if v, _ := idx.GetSessionMetadata(bystander, "effort"); v != "low" {
		t.Errorf("bystander effort metadata = %q, want low", v)
	}
}
