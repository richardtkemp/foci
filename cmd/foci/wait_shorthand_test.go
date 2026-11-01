package main

import (
	"strings"
	"testing"
)

// waitBodyKeys are the /command wire keys the wait flag set writes.
var waitBodyKeys = []string{"wait_warm", "wait_cold", "wait_user_active", "wait_user_inactive", "wait_timeout", "wait_none"}

// clearWaitEnv blanks every env default the command family reads, so a
// test sees only what it sets itself.
func clearWaitEnv(t *testing.T) {
	t.Helper()
	for _, kv := range []string{
		"FOCI_AGENT", "FOCI_HUMAN", "FOCI_SESSION", "FOCI_NO_GATE",
		"FOCI_WAIT_WARM", "FOCI_WAIT_COLD", "FOCI_WAIT_USER_ACTIVE", "FOCI_WAIT_USER_INACTIVE", "FOCI_WAIT_TIMEOUT",
		"FOCI_IF_WARM", "FOCI_IF_COLD", "FOCI_IF_USER_ACTIVE", "FOCI_IF_USER_INACTIVE",
	} {
		t.Setenv(kv, "")
	}
}

func assertNoWaitFields(t *testing.T, who string, body map[string]interface{}) {
	t.Helper()
	for _, k := range waitBodyKeys {
		if v, ok := body[k]; ok {
			t.Errorf("%s sent %s=%v, want no wait fields (#2220: ping/pair take no wait flags)", who, k, v)
		}
	}
}

// TestPingIgnoresWaitFlag proves `foci ping --wait-cold 5m` defers nothing
// (#2220): the shorthand does not take --wait-*, so no wait field reaches
// the gateway — the words are unrecognised words, as any other unknown flag
// is for ping.
func TestPingIgnoresWaitFlag(t *testing.T) {
	clearWaitEnv(t)
	body := captureBody(t, func(base string) error {
		return cmdPing(base, []string{"--wait-cold", "5m"})
	})
	assertNoWaitFields(t, "foci ping --wait-cold 5m", body)
	if cmd, _ := body["command"].(string); !strings.HasSuffix(cmd, "/ping") {
		t.Errorf("command = %q, want the /ping dispatch", cmd)
	}
}

// TestPingPairIgnoreWaitEnv proves the FOCI_WAIT_* defaults do not defer
// `foci ping` or `foci pair` (#2220), while `foci command` keeps reading
// them (unchanged).
func TestPingPairIgnoreWaitEnv(t *testing.T) {
	clearWaitEnv(t)
	t.Setenv("FOCI_WAIT_COLD", "5m")
	t.Setenv("FOCI_NO_GATE", "1")

	assertNoWaitFields(t, "foci ping", captureBody(t, func(base string) error {
		return cmdPing(base, nil)
	}))
	assertNoWaitFields(t, "foci ping --no-gate", captureBody(t, func(base string) error {
		return cmdPing(base, []string{"--no-gate"})
	}))
	pair := captureBody(t, func(base string) error {
		return cmdPair(base, []string{"example.com"})
	})
	assertNoWaitFields(t, "foci pair", pair)
	if pair["command"] != "/pair example.com" {
		t.Errorf("pair command = %v, want /pair example.com", pair["command"])
	}
}

// TestCommandStillReadsWaitEnv is the characterisation half of
// TestPingPairIgnoreWaitEnv: `foci command` still takes FOCI_WAIT_COLD.
func TestCommandStillReadsWaitEnv(t *testing.T) {
	clearWaitEnv(t)
	t.Setenv("FOCI_WAIT_COLD", "5m")
	body := captureBody(t, func(base string) error {
		return cmdCommand(base, []string{"/reset"})
	})
	if body["wait_cold"] != "5m" {
		t.Errorf("foci command wait_cold = %v, want 5m from FOCI_WAIT_COLD", body["wait_cold"])
	}
}

// TestCommandHelpStatesDeadlineDrop pins Requirement 8 (#2220): the command
// help says the default deadline is 24h and that a deferred command is
// dropped, not run, at its deadline — next to the example and in the
// --wait-timeout text.
func TestCommandHelpStatesDeadlineDrop(t *testing.T) {
	help := captureStderr(t, commandUsage)
	for _, want := range []string{"default 24h", "DROPPED with a notice", "dropped, not run", "COMMAND is dropped"} {
		if !strings.Contains(help, want) {
			t.Errorf("command help missing %q", want)
		}
	}
	if strings.Contains(help, "default 2h") {
		t.Error("command help still states the old 2h default")
	}
}
