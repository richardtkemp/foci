// reap.go — Startup orphan reaper for `opencode` subprocesses left behind by
// a previous foci-gw instance that crashed or was killed before reaching the
// clean-shutdown path (CloseAllServers in shutdown.go).
//
// When foci-gw dies uncleanly (SIGKILL, OOM, panic), its `opencode serve`
// children are reparented to PID 1 and survive the restart. The serve process
// itself holds ports and RSS; its LSP children (`opencode run ... language-
// server`, `gopls`, etc.) are likewise orphaned and linger. Observed: 10+
// leaked serve processes (~2 GB RSS) plus stray LSP workers, across a few
// non-clean restarts.
//
// This scanner runs early in startup — before any new servers are spawned —
// so every `opencode` process it finds with PPID 1 is definitively an orphan.

package opencode

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"foci/internal/log"
)

// procDir is the procfs mount point. Tests override this to point at a
// fake /proc tree.
var procDir = "/proc"

// sigtermGrace is how long we wait after SIGTERM before SIGKILL.
var sigtermGrace = 2 * time.Second

// ReapOrphanedServers scans /proc for `opencode` subprocesses (serve or LSP
// children like `opencode run ... language-server`) whose parent is PID 1
// (reparented to init after the spawning foci-gw died uncleanly). Sends
// SIGTERM, waits briefly, then SIGKills survivors. Only processes owned by
// the same UID as the caller are targeted.
//
// Called early in foci-gw startup, before any new servers spawn, so any
// `opencode` process found with PPID 1 IS an orphan from a previous
// instance. Returns the number of processes reaped.
func ReapOrphanedServers() int {
	myUID := os.Getuid()
	orphans := findOrphanedServers(procDir, myUID)
	if len(orphans) == 0 {
		return 0
	}

	pids := make([]string, len(orphans))
	for i, pid := range orphans {
		pids[i] = strconv.Itoa(pid)
	}
	log.Warnf("opencode", "found %d orphaned opencode process(es) (pids: %s); reaping", len(orphans), strings.Join(pids, ", "))

	killed := terminatePids(orphans, sigtermGrace)
	log.Infof("opencode", "reaped %d/%d orphaned opencode process(es)", killed, len(orphans))
	return killed
}

// findOrphanedServers scans procDir for `opencode` processes with PPID 1
// owned by myUID. Returns their PIDs.
func findOrphanedServers(dir string, myUID int) []int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	uidStr := strconv.Itoa(myUID)
	selfPID := os.Getpid()
	var orphans []int

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) == 0 || name[0] < '0' || name[0] > '9' {
			continue
		}

		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 1 || pid == selfPID {
			continue
		}

		// Check cmdline is an opencode process (serve, run, etc.).
		cmdline, err := os.ReadFile(filepath.Join(dir, name, "cmdline"))
		if err != nil {
			continue
		}
		if !isOpencodeProcess(cmdline) {
			continue
		}

		// Verify ownership (same UID) and PPID 1 via /proc/<pid>/status.
		ppid, owned := readStatusPPID(filepath.Join(dir, name, "status"), uidStr)
		if !owned || ppid != 1 {
			continue
		}

		orphans = append(orphans, pid)
	}

	return orphans
}

// isOpencodeProcess checks whether a null-separated cmdline is an
// `opencode ...` invocation (first arg basename "opencode"). Matches
// `opencode serve`, `opencode run ... language-server`, etc.
func isOpencodeProcess(cmdline []byte) bool {
	args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	if len(args) < 2 {
		return false
	}
	return filepath.Base(args[0]) == "opencode"
}

// readStatusPPID reads /proc/<pid>/status and returns (ppid, isOwnedByUID).
func readStatusPPID(path, uidStr string) (ppid int, owned bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}

	var ppidFound bool
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "PPid:"):
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				ppid, _ = strconv.Atoi(fields[1])
				ppidFound = true
			}
		case strings.HasPrefix(line, "Uid:"):
			fields := strings.Fields(line)
			// "Uid:\t<real>\t<effective>\t<saved>\t<fs>" — check real UID.
			if len(fields) >= 2 && fields[1] == uidStr {
				owned = true
			}
		}
	}
	return ppid, owned && ppidFound
}

// terminatePids sends SIGTERM to all pids, waits up to grace for graceful
// exit, then SIGKills survivors. Returns the number of processes signaled.
// Mirrors tmux's terminateProcesses kill ladder.
func terminatePids(pids []int, grace time.Duration) int {
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

	// Wait for graceful shutdown.
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		var stillAlive []int
		for _, pid := range alive {
			proc, _ := os.FindProcess(pid)
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

	// SIGKILL stragglers.
	for _, pid := range alive {
		proc, _ := os.FindProcess(pid)
		if err := proc.Signal(syscall.SIGKILL); err == nil {
			log.Warnf("opencode", "SIGKILL orphaned opencode process (pid=%d) — did not exit after SIGTERM", pid)
		}
	}

	return len(pids)
}
