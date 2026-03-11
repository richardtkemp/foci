package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestTmuxKillCleansUpChildProcesses verifies that killing a session terminates all descendants.
func TestTmuxKillCleansUpChildProcesses(t *testing.T) {
	t.Parallel()
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

	name := "foci-test-killproc"
	tmuxSetup(t, name)

	// Start a session that spawns a child process
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 300",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Give the process a moment to start
	time.Sleep(200 * time.Millisecond)

	// Get the pane PID before killing
	pids := tmuxSessionPIDs(name)
	if len(pids) == 0 {
		t.Fatal("no pane PIDs found before kill")
	}

	// Collect descendants (the sleep process is a child of the shell)
	children := collectDescendants(pids)
	allPIDs := append(pids, children...)

	// Kill the session via the tool
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "kill",
		"name":      name,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("kill: %v", err)
	}
	if !strings.Contains(result.Text, name) {
		t.Errorf("kill result = %q, want session name", result.Text)
	}

	// Wait for processes to actually die
	time.Sleep(500 * time.Millisecond)

	// Verify all processes are gone
	for _, pid := range allPIDs {
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue // process doesn't exist, good
		}
		if err := proc.Signal(syscall.Signal(0)); err == nil {
			t.Errorf("process %d still alive after kill", pid)
		}
	}
}

// TestCollectDescendants verifies that descendant processes are found.
func TestCollectDescendants(t *testing.T) {
	// Spawn a process with a known child
	t.Parallel()
	cmd := exec.Command("bash", "-c", "sleep 300 & echo $!; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		cmd.Process.Signal(syscall.SIGTERM)
		cmd.Wait()
	}()

	// Give it a moment to spawn the child
	time.Sleep(200 * time.Millisecond)

	parentPID := cmd.Process.Pid
	descendants := collectDescendants([]int{parentPID})

	if len(descendants) == 0 {
		t.Error("expected at least 1 descendant (the sleep process)")
	}

	// Verify descendants are real PIDs
	for _, pid := range descendants {
		if pid <= 1 {
			t.Errorf("invalid descendant PID: %d", pid)
		}
	}

	// Clean up
	for _, pid := range descendants {
		if proc, err := os.FindProcess(pid); err == nil {
			proc.Signal(syscall.SIGKILL)
		}
	}
}

// TestTerminateProcesses verifies that stubborn processes are killed.
func TestTerminateProcesses(t *testing.T) {
	// Spawn a process that ignores SIGHUP and SIGTERM (like OpenCode does).
	t.Parallel()
	// terminateProcesses should escalate to SIGKILL.
	cmd := exec.Command("bash", "-c", "trap '' HUP TERM; sleep 300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid

	// Give trap time to install
	time.Sleep(100 * time.Millisecond)

	// Verify process is alive
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("process not alive after start: %v", err)
	}

	// terminateProcesses should SIGTERM, wait, then SIGKILL
	killed := terminateProcesses([]int{pid})
	if killed == 0 {
		t.Error("expected terminateProcesses to signal at least 1 process")
	}

	// Wait for SIGKILL to take effect
	cmd.Wait()

	// Verify dead (Signal(0) should fail)
	if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
		t.Errorf("process %d still alive after terminateProcesses", pid)
		cmd.Process.Kill()
	}
}

// TestMaybeKillTmuxServer_WithSessions verifies server stays alive when sessions exist.
func TestMaybeKillTmuxServer_WithSessions(t *testing.T) {
	t.Parallel()
	tmuxAvailable(t)

	name := "foci-test-maybekill"
	tmuxSetup(t, name)

	// Start a session so the server has at least one.
	_, err := runTmux(context.Background(), "new-session", "-d", "-s", name, "sleep 300")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// maybeKillTmuxServer should NOT kill because sessions exist.
	if maybeKillTmuxServer(context.Background()) {
		t.Error("maybeKillTmuxServer killed server while sessions exist")
	}

	// Verify the session is still there.
	out, err := runTmux(context.Background(), "list-sessions", "-F", "#{session_name}")
	if err != nil {
		t.Fatalf("list-sessions after maybeKillTmuxServer: %v", err)
	}
	if !strings.Contains(out, name) {
		t.Errorf("session %q disappeared after maybeKillTmuxServer", name)
	}
}

// TestMaybeKillTmuxServer_NoSessions verifies server is killed when no sessions.
func TestMaybeKillTmuxServer_NoSessions(t *testing.T) {
	// NOT parallel: kills the shared tmux server.
	tmuxAvailable(t)

	// Start a session and immediately kill it so the server has no sessions.
	name := "foci-test-maybekill-empty"
	_, err := runTmux(context.Background(), "new-session", "-d", "-s", name, "sleep 1")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	_, err = runTmux(context.Background(), "kill-session", "-t", name)
	if err != nil {
		t.Fatalf("kill session: %v", err)
	}

	// Server may have exited already (exit-empty on), or it may linger.
	// maybeKillTmuxServer should handle both cases gracefully.
	maybeKillTmuxServer(context.Background())

	// After this, the server should not be running. Verify by listing.
	out, err := runTmux(context.Background(), "list-sessions", "-F", "#{session_name}")
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if strings.TrimSpace(line) != "" {
				t.Errorf("unexpected session %q after server cleanup", line)
			}
		}
	}
	// err != nil is expected ("no server running") — that's the success case.
}

// TestTmuxKillCleansUpServer verifies that last session kill also kills server.
func TestTmuxKillCleansUpServer(t *testing.T) {
	// NOT parallel: kills the shared tmux server.
	tmuxAvailable(t)

	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)
	name := "foci-test-killserver"
	tmuxSetup(t, name)

	// Start a single session
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Kill it — should also clean up the server
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "kill",
		"name":      name,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("kill: %v", err)
	}

	// Give a moment for cleanup
	time.Sleep(100 * time.Millisecond)

	// Verify no tmux server is running
	out, err := runTmux(context.Background(), "list-sessions", "-F", "#{session_name}")
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if strings.TrimSpace(line) != "" {
				t.Errorf("session %q still exists after kill (server should be gone)", line)
			}
		}
	}
	// err != nil ("no server running") is the expected success case.
}

// TestTmuxSessionPIDs verifies that session pane PIDs can be extracted.
func TestTmuxSessionPIDs(t *testing.T) {
	t.Parallel()
	tmuxAvailable(t)

	name := "foci-test-pids"
	tmuxSetup(t, name)

	// Create a session
	_, err := runTmux(context.Background(), "new-session", "-d", "-s", name, "sleep 300")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	pids := tmuxSessionPIDs(name)
	if len(pids) == 0 {
		t.Error("expected at least 1 pane PID")
	}
	for _, pid := range pids {
		if pid <= 1 {
			t.Errorf("invalid pane PID: %d", pid)
		}
		// Verify PID exists
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); os.IsNotExist(err) {
			t.Errorf("pane PID %d does not exist in /proc", pid)
		}
	}
}
