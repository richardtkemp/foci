package codex

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"foci/internal/delegator"
	"foci/internal/delegator/sessionenv"
	"foci/internal/log"
)

// The hook entry and its trust state are the app-server's own `-c` session
// flags. If either ever became a write into ~/.codex/config.toml it would leak
// foci's settings into the user's codex CLI and race every other codex agent
// on the host, so this pins the flag form.
func TestHookConfigArgs_AreSessionFlags(t *testing.T) {
	args := hookConfigArgs(`/opt/foci/bin/foci-codex-hook --env-dir /tmp/foci/session-env`)
	if len(args) != 2 || args[0] != "-c" {
		t.Fatalf("want a single -c flag, got %v", args)
	}
	v := args[1]
	for _, want := range []string{
		"hooks.PreToolUse=[{matcher=",
		`"^Bash$"`,
		`type="command"`,
		"foci-codex-hook --env-dir /tmp/foci/session-env",
		"timeout=10",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("config value missing %q:\n%s", want, v)
		}
	}
}

func TestHookTrustArgs(t *testing.T) {
	args := hookTrustArgs(hookTrust{Key: `/<session-flags>/config.toml:pre_tool_use:0:0`, Hash: "sha256:abc"})
	if len(args) != 2 || args[0] != "-c" {
		t.Fatalf("want a single -c flag, got %v", args)
	}
	want := `hooks.state={"/<session-flags>/config.toml:pre_tool_use:0:0"={enabled=true,trusted_hash="sha256:abc"}}`
	if args[1] != want {
		t.Errorf("\n got  %s\n want %s", args[1], want)
	}
	// An unresolved trust must produce nothing rather than a half-formed
	// entry: codex would load the hook and silently never run it either way,
	// but a malformed hooks.state can reject the whole config.
	if got := hookTrustArgs(hookTrust{Key: "k"}); got != nil {
		t.Errorf("incomplete trust must yield no args, got %v", got)
	}
	if got := hookTrustArgs(hookTrust{Hash: "h"}); got != nil {
		t.Errorf("incomplete trust must yield no args, got %v", got)
	}
}

func TestBuildHookCommand_QuotesPaths(t *testing.T) {
	got := buildHookCommand("/opt/my foci/bin/foci-codex-hook", "/tmp/foci/session-env")
	want := `'/opt/my foci/bin/foci-codex-hook' --env-dir /tmp/foci/session-env`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestHookConfigArgs_EmptyCommand(t *testing.T) {
	if got := hookConfigArgs(""); got != nil {
		t.Errorf("want nil for an unresolved hook binary, got %v", got)
	}
}
func TestBindThreadEnv_KeyedByThreadID(t *testing.T) {
	threadID := "test-thread-" + t.Name()
	t.Cleanup(func() { _ = sessionenv.Remove(threadID) })
	b := &Backend{lg: log.NewComponentLogger("codex")}
	b.startOpts = delegator.StartOptions{
		SessionKey: "agent/c9",
		Env: map[string]string{
			"FOCI_SOCK":        "/tmp/foci/exec-agent-c9.sock",
			"BASH_ENV":         "/tmp/foci/exec-agent-c9-funcs.sh",
			"FOCI_SESSION_KEY": "agent/c9",
			"UNRELATED":        "x",
		},
	}

	b.bindThreadEnv(threadID)

	data, err := os.ReadFile(sessionenv.Path(threadID))
	if err != nil {
		t.Fatalf("binding not written under the thread id: %v", err)
	}
	var entry sessionenv.Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	if entry.SessionKey != "agent/c9" || entry.FociSock != "/tmp/foci/exec-agent-c9.sock" {
		t.Errorf("unexpected binding: %+v", entry)
	}
	if strings.Contains(string(data), "UNRELATED") {
		t.Error("only the bridge vars belong in the binding")
	}

	b.unbindThreadEnv(threadID)
	if _, err := os.Stat(sessionenv.Path(threadID)); !os.IsNotExist(err) {
		t.Error("binding must be removed when the thread goes away")
	}
}

func TestBindThreadEnv_NoBridgeWritesNothing(t *testing.T) {
	threadID := "test-thread-" + t.Name()
	t.Cleanup(func() { _ = sessionenv.Remove(threadID) })
	b := &Backend{lg: log.NewComponentLogger("codex")}
	b.startOpts = delegator.StartOptions{Env: map[string]string{"HOME": "/tmp"}}

	b.bindThreadEnv(threadID)

	if _, err := os.Stat(sessionenv.Path(threadID)); !os.IsNotExist(err) {
		t.Error("a session with no exec bridge must not create a binding")
	}
	b.unbindThreadEnv(threadID) // must not panic on a missing file
}
