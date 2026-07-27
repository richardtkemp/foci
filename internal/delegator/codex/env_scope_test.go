package codex

import (
	"os"
	"strings"
	"testing"

	"foci/internal/delegator"
)

// TestBuildEnv_NoCCBashTimeoutVars is the negative half of the CC-only
// BASH_MAX_TIMEOUT_MS injection (internal/delegator/ccstream/env.go). That
// var raises Claude Code's per-call Bash ceiling to work around a CC-subagent
// wakeup bug; it means nothing to codex and must not leak here. If someone
// later moves the injection up into DelegatedManager's shared per-session env
// (where FOCI_SOCK/BASH_ENV live), this test fails.
func TestBuildEnv_NoCCBashTimeoutVars(t *testing.T) {
	for _, k := range []string{"BASH_MAX_TIMEOUT_MS", "BASH_DEFAULT_TIMEOUT_MS", "CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS"} {
		if old, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() { _ = os.Setenv(k, old) })
			_ = os.Unsetenv(k)
		}
	}

	b := &Backend{cfg: map[string]any{}}
	b.startOpts = delegator.StartOptions{Env: map[string]string{"FOCI_SESSION_KEY": "codex/main"}}

	env := b.buildEnv()

	// Sanity: this really is the codex subprocess env.
	if !hasEnvKey(env, "FOCI_SESSION_KEY") {
		t.Fatal("FOCI_SESSION_KEY missing — buildEnv did not apply StartOptions.Env")
	}
	for _, k := range []string{"BASH_MAX_TIMEOUT_MS", "BASH_DEFAULT_TIMEOUT_MS", "CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS"} {
		if hasEnvKey(env, k) {
			t.Errorf("%s leaked into the codex subprocess env — it is Claude Code-only", k)
		}
	}
}

func hasEnvKey(env []string, key string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}
