package ccstream

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/fociroot"
)

// The #3104 phase-2 ccstream tests: a CC process foci starts for an agent
// (session, readiness probe) reads the login of the agent's own account.

// separateRootGateway simulates the work Mac's gateway environment: an install
// root apart from HOME, so EnsureCCConfigEnv exports CLAUDE_CONFIG_DIR and
// CLAUDE_SECURESTORAGE_CONFIG_DIR="" in this process.
func separateRootGateway(t *testing.T) {
	t.Helper()
	isolateCCEnv(t)
	t.Setenv(fociroot.EnvVar, filepath.Join(t.TempDir(), "foci"))
	if _, err := fociroot.EnsureCCConfigEnv(); err != nil {
		t.Fatal(err)
	}
}

func lastEnvValue(env []string, key string) (string, bool) {
	val, ok := "", false
	for _, kv := range env {
		if v, found := strings.CutPrefix(kv, key+"="); found {
			val, ok = v, true
		}
	}
	return val, ok
}

// TestBuildEnv_OwnLogin (H1, requirement 1, Q12): the session environment of
// beta (only CLAUDE_CONFIG_DIR) carries no CLAUDE_SECURESTORAGE_CONFIG_DIR;
// alpha (no override) keeps the gateway's empty value; gamma (sets it empty
// itself) keeps its own.
func TestBuildEnv_OwnLogin(t *testing.T) {
	separateRootGateway(t)
	for _, c := range []struct {
		name    string
		agent   map[string]string
		present bool
	}{
		{"alpha", nil, true},
		{"beta", map[string]string{"CLAUDE_CONFIG_DIR": "/x"}, false},
		{"gamma", map[string]string{"CLAUDE_CONFIG_DIR": "/x", "CLAUDE_SECURESTORAGE_CONFIG_DIR": ""}, true},
	} {
		v, ok := lastEnvValue(buildEnv(c.agent, nil), fociroot.CCSecureStorageEnv)
		if ok != c.present || v != "" {
			t.Errorf("%s: CLAUDE_SECURESTORAGE_CONFIG_DIR = %q present %v, want present %v", c.name, v, ok, c.present)
		}
	}
}

// TestCheckReady_UsesAgentAccount (H1, H7, requirement 6, F4): the readiness
// probe runs `claude auth status` with the agent's environment, so it reads
// the agent's account: beta's probe sees beta's CLAUDE_CONFIG_DIR and no
// secure-storage variable; alpha's sees the host values. The probe reports
// the account's email through OnLoginEmail and its own end date.
func TestCheckReady_UsesAgentAccount(t *testing.T) {
	separateRootGateway(t)
	ccB := filepath.Join(t.TempDir(), "ccB")
	dump := filepath.Join(t.TempDir(), "env")
	stub := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\n" +
		"echo \"dir=${CLAUDE_CONFIG_DIR} secure=${CLAUDE_SECURESTORAGE_CONFIG_DIR-<unset>}\" > '" + dump + "'\n" +
		"echo '{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"email\":\"b@example.com\",\"loginExpiresAt\":\"2026-12-01T00:00:00Z\"}'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	host, err := fociroot.CCConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"beta", map[string]string{"CLAUDE_CONFIG_DIR": ccB}, "dir=" + ccB + " secure=<unset>"},
		{"alpha", nil, "dir=" + host + " secure="},
	} {
		var email string
		var expiry bool
		b := &Backend{cfg: map[string]any{"binary": stub, "env": c.env}}
		b.onLoginEmail = func(e string) { email = e }
		b.onLoginExpiry = func(at time.Time) { expiry = !at.IsZero() }
		if ready, err := b.CheckReady(context.Background()); err != nil || !ready {
			t.Fatalf("%s: CheckReady = %v, %v", c.name, ready, err)
		}
		got, err := os.ReadFile(dump)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(got)) != c.want {
			t.Errorf("%s: probe env %q, want %q", c.name, strings.TrimSpace(string(got)), c.want)
		}
		if email != "b@example.com" || !expiry {
			t.Errorf("%s: email %q expiry %v, want the probe's", c.name, email, expiry)
		}
	}
}
