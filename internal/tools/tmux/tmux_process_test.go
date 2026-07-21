package tmux

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// processHasSigIgn reports whether pid has sig in its /proc/<pid>/status
// SigIgn bitmask (bit N-1 for signal N, per the kernel's sigset encoding) —
// i.e. the process has set sig to be ignored, e.g. via the shell builtin
// trap with an empty action. Lets a test poll for "the trap installed"
// instead of a fixed sleep: no signal needs to be sent to observe it.
func processHasSigIgn(pid int, sig syscall.Signal) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "SigIgn:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return false
		}
		mask, err := strconv.ParseUint(fields[1], 16, 64)
		if err != nil {
			return false
		}
		return mask&(1<<uint(sig-1)) != 0
	}
	return false
}

func TestTmuxKillCleansUpChildProcesses(t *testing.T) {
	// Verifies that killing a tmux session also terminates all descendant processes, not just the pane shell, preventing orphaned processes.
	tmuxAvailable(t)

	// Isolated tmux server so the kill path's maybeKillTmuxServer
	// can't race with other parallel tests on the shared server.
	dir := t.TempDir()
	sock := filepath.Join(dir, "tmux.sock")
	exec.Command("tmux", "-S", sock, "start-server").Run()
	t.Cleanup(func() {
		exec.Command("tmux", "-S", sock, "kill-server").Run()
	})

	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, sock)

	t.Parallel()

	name := "foci-test-killproc"

	// Start a session that spawns a child process
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 300",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Get the pane PID before killing. No wait needed: `start`'s new-session
	// call is synchronous, and tmux assigns+reports the pane's PID as part of
	// that same call (verified: list-panes reflects it with zero measured
	// delay), so it's already queryable the instant tool.Execute returns.
	inst := &tmuxInstance{socketPath: sock}
	pids := inst.tmuxSessionPIDs(name)
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

	// Wait for processes to actually die — poll Signal(0) rather than
	// guessing how long kill-session's SIGHUP takes to propagate.
	pollUntil(t, 5*time.Second, func() bool {
		for _, pid := range allPIDs {
			proc, err := os.FindProcess(pid)
			if err != nil {
				continue
			}
			if proc.Signal(syscall.Signal(0)) == nil {
				return false // still alive
			}
		}
		return true
	})

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

func TestCollectDescendants(t *testing.T) {
	// Verifies that collectDescendants finds child processes of a given PID, using a bash shell that spawns a sleep subprocess.
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

	// Poll until the child has actually spawned rather than guessing a fixed
	// duration for bash to fork the `sleep 300` background job.
	parentPID := cmd.Process.Pid
	var descendants []int
	pollUntil(t, 5*time.Second, func() bool {
		descendants = collectDescendants([]int{parentPID})
		return len(descendants) > 0
	})

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

func TestTerminateProcesses(t *testing.T) {
	// Verifies that terminateProcesses escalates to SIGKILL for processes that ignore SIGHUP and SIGTERM, ensuring they are reliably killed.
	t.Parallel()
	cmd := exec.Command("bash", "-c", "trap '' HUP TERM; sleep 300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid

	// Poll /proc/<pid>/status for the SigIgn bitmask rather than guessing how
	// long bash takes to execute `trap '' HUP TERM` — SIGHUP(1) and
	// SIGTERM(15) both show up as ignored (verified: `trap '' HUP TERM;
	// sleep 300` sets /proc/<pid>/status SigIgn bit 0 and bit 14) the instant
	// the trap builtin runs, no signal needs sending to observe it.
	if !pollUntil(t, 5*time.Second, func() bool {
		return processHasSigIgn(pid, syscall.SIGHUP) && processHasSigIgn(pid, syscall.SIGTERM)
	}) {
		t.Fatal("trap did not install within timeout")
	}

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

func TestMaybeKillTmuxServer_WithSessions(t *testing.T) {
	// Verifies that maybeKillTmuxServer does not kill the server when active sessions still exist, preserving running work.
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
	if testTmuxInstance().maybeKillTmuxServer(context.Background()) {
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

func TestMaybeKillTmuxServer_NoSessions(t *testing.T) {
	// Verifies that maybeKillTmuxServer kills the server when no sessions remain, and handles both "server already exited" and "server still running" cases gracefully.
	tmuxAvailable(t)

	// Isolated tmux server so killing it doesn't affect other parallel tests.
	dir := t.TempDir()
	sock := filepath.Join(dir, "tmux.sock")
	exec.Command("tmux", "-S", sock, "start-server").Run()
	t.Cleanup(func() {
		exec.Command("tmux", "-S", sock, "kill-server").Run()
	})
	inst := &tmuxInstance{socketPath: sock}

	t.Parallel()

	// Start a session and immediately kill it so the server has no sessions.
	name := "foci-test-maybekill-empty"
	_, err := runTmuxWithSocket(context.Background(), sock, "new-session", "-d", "-s", name, "sleep 1")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	_, err = runTmuxWithSocket(context.Background(), sock, "kill-session", "-t", name)
	if err != nil {
		t.Fatalf("kill session: %v", err)
	}

	// Server may have exited already (exit-empty on), or it may linger.
	// maybeKillTmuxServer should handle both cases gracefully.
	inst.maybeKillTmuxServer(context.Background())

	// After this, the server should not be running. Verify by listing.
	out, err := runTmuxWithSocket(context.Background(), sock, "list-sessions", "-F", "#{session_name}")
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if strings.TrimSpace(line) != "" {
				t.Errorf("unexpected session %q after server cleanup", line)
			}
		}
	}
	// err != nil is expected ("no server running") — that's the success case.
}

func TestTmuxKillCleansUpServer(t *testing.T) {
	// Verifies that killing the last session via the tool also shuts down the tmux server, leaving no orphaned server process.
	tmuxAvailable(t)

	// Isolated tmux server so killing it doesn't affect other parallel tests.
	dir := t.TempDir()
	sock := filepath.Join(dir, "tmux.sock")
	exec.Command("tmux", "-S", sock, "start-server").Run()
	t.Cleanup(func() {
		exec.Command("tmux", "-S", sock, "kill-server").Run()
	})

	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, sock)

	t.Parallel()

	name := "foci-test-killserver"

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

	// Poll until the server actually shuts down rather than guessing a fixed
	// cleanup duration.
	pollUntil(t, 5*time.Second, func() bool {
		_, err := runTmuxWithSocket(context.Background(), sock, "list-sessions", "-F", "#{session_name}")
		return err != nil // "no server running" is the success case
	})

	// Verify no tmux server is running
	out, err := runTmuxWithSocket(context.Background(), sock, "list-sessions", "-F", "#{session_name}")
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if strings.TrimSpace(line) != "" {
				t.Errorf("session %q still exists after kill (server should be gone)", line)
			}
		}
	}
	// err != nil ("no server running") is the expected success case.
}

func TestTmuxSessionPIDs(t *testing.T) {
	// Verifies that tmuxSessionPIDs returns valid process IDs for a running session's panes, confirming they are real /proc entries.
	t.Parallel()
	tmuxAvailable(t)

	name := "foci-test-pids"
	tmuxSetup(t, name)

	// Create a session
	_, err := runTmux(context.Background(), "new-session", "-d", "-s", name, "sleep 300")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// No wait needed: new-session -d is synchronous and tmux assigns+reports
	// the pane's PID as part of that same call (verified: list-panes reflects
	// it with zero measured delay), so it's immediately queryable.
	pids := testTmuxInstance().tmuxSessionPIDs(name)
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
