package opencode

import (
	"os"
	"strings"
	"testing"
)

// TestBuildCmdEnv_NoCCBashTimeoutVars is the negative half of the CC-only
// BASH_MAX_TIMEOUT_MS injection (internal/delegator/ccstream/env.go). That
// var raises Claude Code's per-call Bash ceiling to work around a CC-subagent
// wakeup bug; opencode has its own tool runner and must not inherit it. If
// someone later moves the injection up into DelegatedManager's shared
// per-session env (where FOCI_SOCK/BASH_ENV live), this test fails.
func TestBuildCmdEnv_NoCCBashTimeoutVars(t *testing.T) {
	keys := []string{"BASH_MAX_TIMEOUT_MS", "BASH_DEFAULT_TIMEOUT_MS", "CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS"}
	for _, k := range keys {
		if old, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() { _ = os.Setenv(k, old) })
			_ = os.Unsetenv(k)
		}
	}

	s := &Server{extraEnv: map[string]string{"FOCI_SESSION_KEY": "opencode/main"}}
	env := s.buildCmdEnv()

	// Sanity: this really is the opencode subprocess env.
	if !hasEnvKey(env, "FOCI_SESSION_KEY") {
		t.Fatal("FOCI_SESSION_KEY missing — buildCmdEnv did not apply extraEnv")
	}
	for _, k := range keys {
		if hasEnvKey(env, k) {
			t.Errorf("%s leaked into the opencode subprocess env — it is Claude Code-only", k)
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
