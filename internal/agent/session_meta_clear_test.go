package agent

import (
	"path/filepath"
	"testing"

	"foci/internal/session"
)

func TestClearSessionState(t *testing.T) {
	// Proves that ClearSessionState drops the per-session runtime state for a
	// stable key after its history is reset: in-memory runtime fields, the
	// turn lock, and every session_metadata row that is not a user override
	// (cc_resume_id and the like). Overrides: TestClearSessionState_KeepsUserOverrides.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ag := &Agent{SessionIndex: idx}

	key := "bot/c100"

	// Set up per-session runtime state.
	ag.setMetaLocked(key, func(sm *sessionMeta) { sm.contextLimit = 123456 })
	for k, v := range map[string]string{"cc_resume_id": "uuid-999", "orientation_consumed": "true"} {
		if err := idx.SetSessionMetadata(key, k, v); err != nil {
			t.Fatal(err)
		}
	}
	oldLock := ag.turnLock(key)

	ag.ClearSessionState(key)

	if ag.SessionContextLimitKnown(key) {
		t.Error("backend-reported context limit should be dropped by clear")
	}

	// The turn lock entry is dropped — a fresh lock is created on next use.
	if ag.turnLock(key) == oldLock {
		t.Error("turn lock should be a fresh object after clear")
	}

	// Runtime session_metadata rows are gone.
	for _, k := range []string{"cc_resume_id", "orientation_consumed"} {
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

func TestClearSessionState_KeepsUserOverrides(t *testing.T) {
	// Proves a reset keeps the user's own per-session choices (#1545): the
	// /model tuple, /effort, /display and no_compact outlive ClearSessionState,
	// both persisted and in memory, while runtime state (cc_resume_id) and the
	// runtime-only permission mode are dropped. The scheduled daily reset
	// otherwise put a session switched to haiku back on the agent default.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ag := &Agent{SessionIndex: idx, Model: "opus"}

	key := "bot/c100"

	ag.SetSessionModel(key, "anthropic/haiku", "openrouter", "openai", nil)
	ag.SetSessionEffort(key, "high")
	ag.SetSessionDisplayWidth(key, "80")
	ag.SetSessionNoCompact(key, true)
	ag.SetSessionPermissionMode(key, "plan")
	if err := idx.SetSessionMetadata(key, "cc_resume_id", "uuid-999"); err != nil {
		t.Fatal(err)
	}

	ag.ClearSessionState(key)

	if got := ag.SessionModel(key); got != "anthropic/haiku" {
		t.Errorf("model after reset = %q, want anthropic/haiku", got)
	}
	if got := ag.SessionFormat(key); got != "openai" {
		t.Errorf("format after reset = %q, want openai", got)
	}
	if got := ag.SessionEffort(key); got != "high" {
		t.Errorf("effort after reset = %q, want high", got)
	}
	if got := ag.SessionDisplayWidth(key); got != "80" {
		t.Errorf("display_width after reset = %q, want 80", got)
	}
	if !ag.SessionNoCompact(key) {
		t.Error("no_compact should survive reset")
	}
	if got := ag.SessionPermissionMode(key); got != "" {
		t.Errorf("permission_mode after reset = %q, want empty (a reset relaunches the backend at its default mode)", got)
	}

	want := map[string]string{
		"model": "anthropic/haiku", "model_endpoint": "openrouter", "model_format": "openai",
		"effort": "high", "display_width": "80", "no_compact": "true",
		"permission_mode": "", "cc_resume_id": "",
	}
	for k, w := range want {
		if v, _ := idx.GetSessionMetadata(key, k); v != w {
			t.Errorf("metadata %q after reset = %q, want %q", k, v, w)
		}
	}
}

func TestClearSessionState_BackendObservedModelNotKept(t *testing.T) {
	// Proves only a persisted user override survives a reset: the in-memory
	// model a delegated backend reported (FinalModel / get_context_window,
	// never persisted) is dropped, so the reset session launches on the agent
	// default rather than, say, a model CC fell back to.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ag := &Agent{SessionIndex: idx, Model: "opus"}

	key := "bot/c100"
	ag.setMetaLocked(key, func(sm *sessionMeta) { sm.model = "claude/claude-sonnet-4-6" })

	ag.ClearSessionState(key)

	if got := ag.SessionModel(key); got != "opus" {
		t.Errorf("model after reset = %q, want agent default opus", got)
	}
}
