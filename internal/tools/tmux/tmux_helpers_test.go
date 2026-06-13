package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	// Isolate exec/shell tests from a live foci agent's bridge. When `go test`
	// is run from inside a running agent's Bash session, the process inherits
	// FOCI_SOCK (the production exec-bridge socket) and BASH_ENV (which defines
	// the foci_* shell functions). ExecTool subprocesses inherit os.Environ(),
	// so tests that exec `foci_http_request ... https://example.com` would
	// connect to the PRODUCTION bridge with the real secret store — firing real
	// (host-check-blocked, but log-noisy) requests through the live session.
	// Tests that genuinely need a bridge set FOCI_SOCK explicitly themselves.
	for _, k := range []string{"FOCI_SOCK", "BASH_ENV", "FOCI_GW_SOCK", "FOCI_ADDR"} {
		os.Unsetenv(k)
	}

	dir, _ := os.MkdirTemp(os.TempDir(), "foci-tmux-test-*")
	tmuxSocketPath = filepath.Join(dir, "tmux.sock")

	// Pre-start the tmux server so parallel tests don't race on startup.
	// "start-server" is a no-op if a server is already running.
	if _, err := exec.LookPath("tmux"); err == nil {
		exec.Command("tmux", "-S", tmuxSocketPath, "start-server").Run()
	}

	code := m.Run()
	exec.Command("tmux", "-S", tmuxSocketPath, "kill-server").Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func tmuxAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
}

// tmuxSetup pre-cleans named sessions (from prior crashed runs) and registers
// t.Cleanup to kill them when the test finishes. All operations use the
// test-isolated tmux socket.
func tmuxSetup(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		exec.Command("tmux", "-S", tmuxSocketPath, "kill-session", "-t", name).Run()
		t.Cleanup(func() {
			exec.Command("tmux", "-S", tmuxSocketPath, "kill-session", "-t", name).Run()
		})
	}
}

// testTmuxInstance returns a minimal tmuxInstance using the global test socket.
// Used by tests that call methods like tmuxSessionPIDs or maybeKillTmuxServer
// directly (outside of a full NewTmuxTool).
func testTmuxInstance() *tmuxInstance {
	return &tmuxInstance{socketPath: tmuxSocketPath}
}
