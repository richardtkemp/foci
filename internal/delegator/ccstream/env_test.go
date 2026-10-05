package ccstream

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

// envValue returns the LAST value for key in a KEY=VALUE list, matching
// execve semantics (later entries win), plus how many times it appeared.
func envValue(env []string, key string) (string, int) {
	prefix := key + "="
	val, n := "", 0
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			val = strings.TrimPrefix(e, prefix)
			n++
		}
	}
	return val, n
}

// clearEnvVar removes key from the test process's environment for the
// duration of the test, so an inherited value can't be mistaken for one the
// code under test injected (buildEnv starts from os.Environ()).
func clearEnvVar(t *testing.T, key string) {
	t.Helper()
	if old, ok := os.LookupEnv(key); ok {
		t.Cleanup(func() { _ = os.Setenv(key, old) })
		_ = os.Unsetenv(key)
	}
}

// TestBuildEnv_RaisesBashMaxTimeout pins the ceiling foci hands the CC
// subprocess. CC's built-in max for a model-requested Bash timeout is
// 600000ms; a CC subagent can't be woken by a background-task completion
// (upstream #78782/#77578/#76594), so it must finish long work inside one
// foreground Bash call. 20 minutes is what makes that possible.
func TestBuildEnv_RaisesBashMaxTimeout(t *testing.T) {
	clearEnvVar(t, "BASH_MAX_TIMEOUT_MS")

	env := buildEnv(nil)

	got, n := envValue(env, "BASH_MAX_TIMEOUT_MS")
	if n == 0 {
		t.Fatal("BASH_MAX_TIMEOUT_MS missing from CC subprocess env — subagents stay capped at CC's 10min ceiling")
	}
	if got != "1200000" {
		t.Errorf("BASH_MAX_TIMEOUT_MS = %q, want %q (20 minutes)", got, "1200000")
	}
}

// TestBuildEnv_RaisesMaxRetries pins CC's API retry budget at its ceiling
// (#2186). CC 2.1.289 retries 529 Overloaded for a subagent (query source
// "agent:*" counts as foreground) only CLAUDE_CODE_MAX_RETRIES times, default
// 10 — about 3 minutes of 0.5s-doubling backoff capped at 32s. A longer
// overload ends the subagent with "API Error: 529 Overloaded" and the
// orchestrator has to resume it by hand. 15 is the highest value CC honours
// without CLAUDE_CODE_RETRY_WATCHDOG (it clamps above that), roughly doubling
// the window to ~6 minutes while keeping it bounded.
func TestBuildEnv_RaisesMaxRetries(t *testing.T) {
	clearEnvVar(t, "CLAUDE_CODE_MAX_RETRIES")

	got, n := envValue(buildEnv(nil), "CLAUDE_CODE_MAX_RETRIES")
	if n == 0 {
		t.Fatal("CLAUDE_CODE_MAX_RETRIES missing from CC subprocess env — subagents die after CC's default 10 retries of a 529")
	}
	if got != "15" {
		t.Errorf("CLAUDE_CODE_MAX_RETRIES = %q, want %q (CC's no-watchdog ceiling)", got, "15")
	}
}

// TestBuildEnv_SetsRetryWatchdog — Dick 2026-10-05: agents wait out 529
// Overloaded and 429 with no attempt cap rather than die and need a manual
// resume. CLAUDE_CODE_RETRY_WATCHDOG=1 is CC's switch for that.
func TestBuildEnv_SetsRetryWatchdog(t *testing.T) {
	clearEnvVar(t, "CLAUDE_CODE_RETRY_WATCHDOG")

	if got, n := envValue(buildEnv(nil), "CLAUDE_CODE_RETRY_WATCHDOG"); n == 0 || got != "1" {
		t.Errorf("CLAUDE_CODE_RETRY_WATCHDOG = %q (n=%d), want %q — 529s would kill subagents after the bounded budget", got, n, "1")
	}
}

// TestBuildEnv_RetryWatchdogOverridable — a per-agent backend_config.env
// value comes later in the env and wins.
func TestBuildEnv_RetryWatchdogOverridable(t *testing.T) {
	clearEnvVar(t, "CLAUDE_CODE_RETRY_WATCHDOG")

	if got, _ := envValue(buildEnv(map[string]string{"CLAUDE_CODE_RETRY_WATCHDOG": ""}), "CLAUDE_CODE_RETRY_WATCHDOG"); got != "" {
		t.Errorf("per-agent override lost: CLAUDE_CODE_RETRY_WATCHDOG = %q, want empty", got)
	}
}

// TestBuildEnv_LeavesBashDefaultTimeoutAlone — we raise only the ceiling the
// model MAY request. Setting BASH_DEFAULT_TIMEOUT_MS would slow every plain
// Bash call's failure mode, which is not what we want.
func TestBuildEnv_LeavesBashDefaultTimeoutAlone(t *testing.T) {
	clearEnvVar(t, "BASH_DEFAULT_TIMEOUT_MS")

	if _, n := envValue(buildEnv(nil), "BASH_DEFAULT_TIMEOUT_MS"); n != 0 {
		t.Error("BASH_DEFAULT_TIMEOUT_MS set — only the MAX should be raised")
	}
}

// TestBuildEnv_PerAgentEnvOverrides is the escape hatch that justifies
// keeping the value a constant rather than a [cc_backend] config field:
// StartOptions.Env (which carries per-agent backend_config.env) is applied
// last, so an operator can still override it per agent.
func TestBuildEnv_PerAgentEnvOverrides(t *testing.T) {
	clearEnvVar(t, "BASH_MAX_TIMEOUT_MS")

	env := buildEnv(map[string]string{"BASH_MAX_TIMEOUT_MS": "300000"})

	if got, _ := envValue(env, "BASH_MAX_TIMEOUT_MS"); got != "300000" {
		t.Errorf("effective BASH_MAX_TIMEOUT_MS = %q, want per-agent override %q", got, "300000")
	}
}

// TestBuildEnv_KeepsSessionExtras guards the pre-existing contract the
// refactor touched: the exec-bridge and session vars must survive.
func TestBuildEnv_KeepsSessionExtras(t *testing.T) {
	env := buildEnv(map[string]string{
		"BASH_ENV":          "/tmp/funcs.sh",
		"FOCI_SOCK":         "/tmp/foci.sock",
		"FOCI_SESSION_KEY":  "agent/chat",
		"CCSTUB_RESPONSE":   "hi",
		"SOMETHING_PERAGNT": "1",
	})
	for k, want := range map[string]string{
		"BASH_ENV":                              "/tmp/funcs.sh",
		"FOCI_SOCK":                             "/tmp/foci.sock",
		"FOCI_SESSION_KEY":                      "agent/chat",
		"CCSTUB_RESPONSE":                       "hi",
		"SOMETHING_PERAGNT":                     "1",
		"CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS": "1",
	} {
		if got, _ := envValue(env, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// TestStart_SpawnedProcessReceivesBashMaxTimeout is the decisive artifact:
// it does not inspect a slice foci built, it spawns a REAL process through
// Backend.Start (the same code path production uses, with the `binary` knob
// pointed at a recorder script instead of `claude`) and reads back the
// environment that process actually observed via /proc-independent `env`.
func TestStart_SpawnedProcessReceivesBashMaxTimeout(t *testing.T) {
	clearEnvVar(t, "BASH_MAX_TIMEOUT_MS")
	clearEnvVar(t, "CLAUDE_CODE_MAX_RETRIES")
	clearEnvVar(t, "CLAUDE_CODE_RETRY_WATCHDOG")

	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	script := filepath.Join(dir, "fake-claude")
	// Dump the inherited environment, then exit. Start only needs the process
	// to spawn; the reader goroutine handles the immediate EOF.
	body := "#!/bin/sh\nenv > " + out + "\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write recorder script: %v", err)
	}

	b, err := newFromConfig(map[string]any{"binary": script})
	if err != nil {
		t.Fatalf("newFromConfig: %v", err)
	}
	be := b.(*Backend)
	if err := be.Start(context.Background(), delegator.StartOptions{
		WorkDir: dir,
		AgentID: "envtest",
		Env:     map[string]string{"FOCI_SESSION_KEY": "envtest/main"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })

	// The recorder exits immediately; poll briefly for its output.
	var data []byte
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err = os.ReadFile(out); err == nil && len(data) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(data) == 0 {
		t.Fatal("recorder script produced no env dump — subprocess never ran")
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if got, n := envValue(lines, "BASH_MAX_TIMEOUT_MS"); n == 0 || got != "1200000" {
		t.Errorf("spawned CC process saw BASH_MAX_TIMEOUT_MS=%q (n=%d), want %q", got, n, "1200000")
	}
	if got, n := envValue(lines, "CLAUDE_CODE_MAX_RETRIES"); n == 0 || got != "15" {
		t.Errorf("spawned CC process saw CLAUDE_CODE_MAX_RETRIES=%q (n=%d), want %q", got, n, "15")
	}
	if got, n := envValue(lines, "CLAUDE_CODE_RETRY_WATCHDOG"); n == 0 || got != "1" {
		t.Errorf("spawned CC process saw CLAUDE_CODE_RETRY_WATCHDOG=%q (n=%d), want %q", got, n, "1")
	}
	// Sanity: the recorder really is the process foci launched for this session.
	if got, _ := envValue(lines, "FOCI_SESSION_KEY"); got != "envtest/main" {
		t.Errorf("FOCI_SESSION_KEY = %q, want %q — wrong process observed", got, "envtest/main")
	}
}
