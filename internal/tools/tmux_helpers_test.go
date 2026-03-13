package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "foci-tmux-test-*")
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

// tmuxSetupWithSentinel is like tmuxSetup but also creates a long-lived
// sentinel session on the shared tmux server. Use this in parallel tests that
// call the "kill" operation: kill triggers maybeKillTmuxServer which nukes the
// server when it finds zero sessions. The sentinel ensures at least one session
// always exists, protecting other parallel tests' sessions.
func tmuxSetupWithSentinel(t *testing.T, names ...string) {
	t.Helper()
	// Sanitise test name for use as a tmux session name (no dots/slashes).
	safe := strings.NewReplacer("/", "-", ".", "-").Replace(t.Name())
	sentinel := fmt.Sprintf("foci-sentinel-%s", safe)
	allNames := append([]string{sentinel}, names...)
	tmuxSetup(t, allNames...)
	exec.Command("tmux", "-S", tmuxSocketPath, "new-session", "-d", "-s", sentinel, "sleep", "3600").Run()
}
