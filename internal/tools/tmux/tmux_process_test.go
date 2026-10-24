package tmux

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

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
		"command":   testSessionCmd,
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

	// Wait for processes to actually die. Signal(0) is NOT a death test: it
	// succeeds on a zombie (killed, exit status not yet reaped by its parent
	// — here the tmux server, or init once the server is gone), and under load
	// reaping can lag arbitrarily. A zombie has executed its last instruction,
	// so it counts as dead; poll for "gone or zombie" (#2421).
	var alive []int
	pollUntil(t, 5*time.Second, func() bool {
		alive = alive[:0]
		for _, pid := range allPIDs {
			if processRunning(pid) {
				alive = append(alive, pid)
			}
		}
		return len(alive) == 0
	})
	for _, pid := range alive {
		st, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		t.Errorf("process %d still running after kill: %s", pid, st)
	}
}

// processRunning reports whether pid exists and is not a zombie (state Z) or
// dead (X). Zombies still answer Signal(0), so that cannot tell them apart.
func processRunning(pid int) bool {
	if runtime.GOOS != "linux" {
		// No procfs: ps prints the state letters, or nothing for a gone pid.
		out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		st := strings.TrimSpace(string(out))
		return st != "" && st[0] != 'Z'
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// Format: "pid (comm) S ..." — comm may contain spaces/parens, so anchor
	// on the LAST ')'.
	i := strings.LastIndexByte(string(data), ')')
	if i < 0 || i+2 >= len(data) {
		return false
	}
	state := data[i+2]
	return state != 'Z' && state != 'X'
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
	// The script writes a marker AFTER the trap builtin has run, so the
	// marker existing means both signals are already ignored. (Polling the
	// /proc SigIgn mask did the same on Linux only; macOS ps has no
	// ignored-signal column.) No fixed sleep, no signal sent to observe it.
	marker := filepath.Join(t.TempDir(), "trapped")
	cmd := exec.Command("bash", "-c", `trap '' HUP TERM; : > "$1"; sleep 300`, "bash", marker)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid

	if !pollUntil(t, 5*time.Second, func() bool {
		_, err := os.Stat(marker)
		return err == nil
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
	sock := tmuxIsolatedSocket(t)

	name := "foci-test-maybekill"

	// Start a session so the server has at least one.
	_, err := runTmuxWithSocket(context.Background(), sock, "new-session", "-d", "-s", name, testSessionCmd)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// maybeKillTmuxServer should NOT kill because sessions exist.
	if testTmuxInstance(sock).maybeKillTmuxServer(context.Background()) {
		t.Error("maybeKillTmuxServer killed server while sessions exist")
	}

	// Verify the session is still there.
	out, err := runTmuxWithSocket(context.Background(), sock, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		t.Fatalf("list-sessions after maybeKillTmuxServer: %v", err)
	}
	if !strings.Contains(out, name) {
		t.Errorf("session %q disappeared after maybeKillTmuxServer", name)
	}
}

func TestMaybeKillTmuxServer_NoSessions(t *testing.T) {
	// Verifies both outcomes maybeKillTmuxServer documents for an empty
	// server: with the server still running and zero sessions it kills it
	// (returns true); with the server already gone it returns false
	// gracefully.
	// Isolated tmux server so killing it doesn't affect other parallel tests.
	sock := tmuxIsolatedSocket(t)
	inst := testTmuxInstance(sock)

	t.Parallel()

	// Create the only session, then kill it to leave a running server with
	// no sessions. The session runs a command that cannot exit by itself:
	// with the old `sleep 1` fixture, under load the session (and with it
	// the setup's premise) could be gone before kill-session ran, failing
	// the setup with "kill session: exit status 1" (#2222) — a fixture
	// race, not the subject. tmuxIsolatedSocket's server has exit-empty
	// off, so after kill-session the server deliberately lingers with zero
	// sessions: the exact state under test.
	name := "foci-test-maybekill-empty"
	if _, err := runTmuxWithSocket(context.Background(), sock, "new-session", "-d", "-s", name, testSessionCmd); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := runTmuxWithSocket(context.Background(), sock, "kill-session", "-t", name); err != nil {
		t.Fatalf("kill session: %v", err)
	}

	// Outcome 1: server still running with no sessions → killed.
	if !inst.maybeKillTmuxServer(context.Background()) {
		t.Fatal("maybeKillTmuxServer should kill a running server with no sessions")
	}
	// "no server running" (the error) is the success case here.
	if _, err := runTmuxWithSocket(context.Background(), sock, "list-sessions", "-F", "#{session_name}"); err == nil {
		t.Error("server still running after maybeKillTmuxServer killed it")
	}

	// Outcome 2: server already gone (the state outcome 1 just produced) →
	// graceful false, no panic, no error.
	if inst.maybeKillTmuxServer(context.Background()) {
		t.Error("maybeKillTmuxServer should return false when the server is already gone")
	}
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
		"command":   testSessionCmd,
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
	// Verifies that tmuxSessionPIDs returns valid process IDs for a running session's panes, confirming they are live processes.
	t.Parallel()
	sock := tmuxIsolatedSocket(t)

	name := "foci-test-pids"

	// Create a session
	_, err := runTmuxWithSocket(context.Background(), sock, "new-session", "-d", "-s", name, testSessionCmd)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// No wait needed: new-session -d is synchronous and tmux assigns+reports
	// the pane's PID as part of that same call (verified: list-panes reflects
	// it with zero measured delay), so it's immediately queryable.
	pids := testTmuxInstance(sock).tmuxSessionPIDs(name)
	if len(pids) == 0 {
		t.Error("expected at least 1 pane PID")
	}
	for _, pid := range pids {
		if pid <= 1 {
			t.Errorf("invalid pane PID: %d", pid)
		}
		// Verify PID exists (signal 0 checks existence and permission only).
		if err := syscall.Kill(pid, 0); err != nil {
			t.Errorf("pane PID %d does not exist: %v", pid, err)
		}
	}
}
