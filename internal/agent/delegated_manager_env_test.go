package agent

import (
	"context"
	"testing"
)

// TestGet_SessionEnvIsBackendAgnostic pins WHERE the CC-only
// BASH_MAX_TIMEOUT_MS injection lives. DelegatedManager builds one
// per-session env (FOCI_SESSION_KEY, FOCI_SOCK, BASH_ENV) that EVERY
// delegated backend receives — ccstream, cctmux, codex and opencode alike —
// so a Claude Code-specific var placed here would reach all of them. The var
// is therefore injected inside ccstream's own buildEnv
// (internal/delegator/ccstream/env.go) instead, and this test fails if it
// ever migrates up to the shared layer.
//
// The api backend needs no counterpart test: an API agent has no
// DelegatedManager and spawns no subprocess at all.
func TestGet_SessionEnvIsBackendAgnostic(t *testing.T) {
	mgr, mocks := newTestManager(t, nil)

	if _, err := mgr.Get(context.Background(), "test-agent/c1"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(*mocks) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(*mocks))
	}

	be := (*mocks)[0]
	be.mu.Lock()
	env := be.startOpts.Env
	be.mu.Unlock()

	// Sanity: the manager really did build a per-session env.
	if env["FOCI_SESSION_KEY"] != "test-agent/c1" {
		t.Fatalf("FOCI_SESSION_KEY = %q, want %q", env["FOCI_SESSION_KEY"], "test-agent/c1")
	}
	for _, k := range []string{"BASH_MAX_TIMEOUT_MS", "BASH_DEFAULT_TIMEOUT_MS", "CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS"} {
		if v, ok := env[k]; ok {
			t.Errorf("%s=%q in the shared per-session env — it would reach codex/opencode too; keep it in ccstream", k, v)
		}
	}
}
