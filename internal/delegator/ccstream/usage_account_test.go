package ccstream

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/ccaccount"
	"foci/internal/delegator"
	"foci/internal/fociroot"
)

// fakeUsageClaude writes a `claude` that records its path and
// CLAUDE_CONFIG_DIR to dump and answers initialize and get_usage (five-hour
// window at pct) in the stream-json control protocol.
func fakeUsageClaude(t *testing.T, dump, pct string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "my-claude")
	script := "#!/bin/sh\n" +
		"echo \"bin=$0 dir=${CLAUDE_CONFIG_DIR-<unset>}\" > '" + dump + "'\n" +
		"while IFS= read -r line; do\n" +
		"  id=$(printf '%s' \"$line\" | sed -n 's/.*\"request_id\":\"\\([^\"]*\\)\".*/\\1/p')\n" +
		"  case \"$line\" in\n" +
		"  *get_usage*) printf '{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":\"%s\",\"response\":{\"rate_limits_available\":true,\"rate_limits\":{\"five_hour\":{\"utilization\":" + pct + "}}}}}\\n' \"$id\" ;;\n" +
		"  *) printf '{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":\"%s\",\"response\":{}}}\\n' \"$id\" ;;\n" +
		"  esac\n" +
		"done\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestQueryUsage_AgentEnvAndBinary (H4, requirement 3, F5's side bug): the
// usage read runs the agent's configured binary with the agent's own
// CLAUDE_CONFIG_DIR, and reports what that process answered.
func TestQueryUsage_AgentEnvAndBinary(t *testing.T) {
	isolateCCEnv(t)
	ccB := filepath.Join(t.TempDir(), "ccB")
	dump := filepath.Join(t.TempDir(), "dump")
	bin := fakeUsageClaude(t, dump, "99")
	info, err := QueryUsage(context.Background(), delegator.UsageTarget{
		AgentID: "beta", Env: map[string]string{fociroot.CCConfigDirEnv: ccB}, Binary: bin,
	})
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	if info.FiveHour.Percent != 99 {
		t.Errorf("five-hour = %d%%, want the agent's process's 99%%", info.FiveHour.Percent)
	}
	got, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	if want := "bin=" + bin + " dir=" + ccB; strings.TrimSpace(string(got)) != want {
		t.Errorf("read ran %q, want %q", strings.TrimSpace(string(got)), want)
	}
}

// TestUsageAccount_PerLogin (requirement 1, Q11): the usage account is the
// login location: one folder spelled two ways is one account, another folder
// another, and an agent with no override is on the host login.
func TestUsageAccount_PerLogin(t *testing.T) {
	isolateCCEnv(t)
	ccB := filepath.Join(t.TempDir(), "ccB")
	acct := func(env map[string]string) string {
		t.Helper()
		a, err := UsageAccount(env)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	alpha, beta, beta2 := acct(nil), acct(map[string]string{fociroot.CCConfigDirEnv: ccB}), acct(map[string]string{fociroot.CCConfigDirEnv: ccB + "/"})
	host, err := ccaccount.ForAgent(nil)
	if err != nil {
		t.Fatal(err)
	}
	if alpha != "claude-code:"+host.Account.Key() {
		t.Errorf("alpha's account %q, want the host login's", alpha)
	}
	if beta != beta2 || beta == alpha {
		t.Errorf("accounts alpha %q beta %q beta2 %q: want beta == beta2 != alpha", alpha, beta, beta2)
	}
}
