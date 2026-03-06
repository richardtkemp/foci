package tools

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"foci/internal/log"
)

// killSessionWithChildren kills a tmux session and terminates any child
// processes that survive SIGHUP. Returns the number of child processes killed.
func killSessionWithChildren(ctx context.Context, name string) (childrenKilled int, err error) {
	pids := tmuxSessionPIDs(name)
	children := collectDescendants(pids)
	allPIDs := append(pids, children...)

	out, err := runTmux(ctx, "kill-session", "-t", name)
	if err != nil {
		return 0, fmt.Errorf("tmux kill-session: %s %w", strings.TrimSpace(out), err)
	}

	return terminateProcesses(allPIDs), nil
}

// maybeKillTmuxServer kills the tmux server if no sessions remain.
// Returns true if the server was killed.
func maybeKillTmuxServer(ctx context.Context) bool {
	out, err := runTmux(ctx, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		return false // server gone or unknown error — leave it alone
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) != "" {
			return false
		}
	}
	// No sessions remain — kill the server.
	if _, err := runTmux(ctx, "kill-server"); err == nil {
		return true
	}
	return false
}

// tmuxSessionPIDs returns the PID of each pane's shell in the given tmux session.
func tmuxSessionPIDs(session string) []int {
	out, err := runTmux(context.Background(), "list-panes", "-t", session, "-F", "#{pane_pid}")
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if pid, err := strconv.Atoi(line); err == nil && pid > 1 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// collectDescendants returns all descendant PIDs for the given parent PIDs
// by walking /proc/<pid>/task/*/children recursively.
func collectDescendants(pids []int) []int {
	seen := make(map[int]bool)
	var result []int

	var walk func(pid int)
	walk = func(pid int) {
		if seen[pid] {
			return
		}
		seen[pid] = true

		// Read children from /proc/<pid>/task/<pid>/children
		childrenFile := fmt.Sprintf("/proc/%d/task/%d/children", pid, pid)
		data, err := os.ReadFile(childrenFile)
		if err != nil {
			return
		}
		for _, field := range strings.Fields(string(data)) {
			childPID, err := strconv.Atoi(field)
			if err != nil || childPID <= 1 {
				continue
			}
			result = append(result, childPID)
			walk(childPID)
		}
	}

	for _, pid := range pids {
		walk(pid)
	}
	return result
}

// terminateProcesses sends SIGTERM, waits up to 2 seconds, then SIGKILLs
// any survivors. Returns the number of processes that were signaled.
func terminateProcesses(pids []int) int {
	if len(pids) == 0 {
		return 0
	}

	// Send SIGTERM to all
	var alive []int
	for _, pid := range pids {
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			continue // already dead
		}
		alive = append(alive, pid)
	}

	if len(alive) == 0 {
		return 0
	}

	// Wait up to 2 seconds for graceful shutdown
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var stillAlive []int
		for _, pid := range alive {
			proc, err := os.FindProcess(pid)
			if err != nil {
				continue
			}
			// Signal 0 checks if process exists
			if proc.Signal(syscall.Signal(0)) == nil {
				stillAlive = append(stillAlive, pid)
			}
		}
		if len(stillAlive) == 0 {
			return len(alive)
		}
		alive = stillAlive
		time.Sleep(100 * time.Millisecond)
	}

	// SIGKILL survivors
	for _, pid := range alive {
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if err := proc.Signal(syscall.SIGKILL); err == nil {
			log.Debugf("tmux", "SIGKILL pid %d (did not exit after SIGTERM)", pid)
		}
	}

	return len(pids)
}
