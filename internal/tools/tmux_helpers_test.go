package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"foci/internal/tempdir"
)

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp(tempdir.TestDir(), "foci-tmux-test-*")
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
