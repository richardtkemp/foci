// Package resources provides system resource monitoring.
package resources

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"foci/internal/log"
)

// defaultProcDir is the real procfs mount point. Tests substitute a fake tree.
const defaultProcDir = "/proc"

// MemoryGuardConfig holds parsed configuration for the memory guard.
type MemoryGuardConfig struct {
	Interval          time.Duration
	WarnPercent       int     // percent of total RAM
	KillPercent       int     // percent of total RAM
	PressureThreshold float64 // PSI avg10 minimum to act
}

// WarnFunc is called when the memory warn threshold is exceeded.
// The message should be injected into the agent's warning queue.
type WarnFunc func(msg string)

// MemoryGuard monitors total memory usage by the current user and takes
// action when thresholds are exceeded under memory pressure.
type MemoryGuard struct {
	cfg    MemoryGuardConfig
	warnFn WarnFunc
	uid    int

	mu        sync.Mutex
	warnFired bool

	cancel context.CancelFunc
	done   chan struct{}

	// Overridable for testing.
	getMemTotalFn func() (int64, error)                              // returns total RAM in kB
	getUserRSSFn  func(uid int) (int64, error)                       // returns total RSS for user in kB
	getSelfRSSFn  func() (int64, error)                              // returns RSS of this process (foci-gw) in kB
	getPressureFn func() (float64, error)                            // returns PSI memory avg10
	findLargestFn func(uid, selfPid int) (int, string, int64, error) // returns pid, comm, rssKB of largest non-self process
	killProcessFn func(pid int) error                                // SIGTERM then SIGKILL
}

// NewMemoryGuard creates a memory guard. warnFn is called for warning
// injection into the agent session.
func NewMemoryGuard(cfg MemoryGuardConfig, warnFn WarnFunc) *MemoryGuard {
	return &MemoryGuard{
		cfg:    cfg,
		warnFn: warnFn,
		uid:    os.Getuid(),
	}
}

// Start launches the background check goroutine.
func (g *MemoryGuard) Start(ctx context.Context) {
	if g.cfg.Interval <= 0 {
		log.Warnf("resources", "memory guard not started: interval=%v (<=0, disabled)", g.cfg.Interval)
		return
	}
	monCtx, cancel := context.WithCancel(ctx)
	g.cancel = cancel
	g.done = make(chan struct{})

	go func() {
		defer close(g.done)
		ticker := time.NewTicker(g.cfg.Interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				g.checkOnce()
			case <-monCtx.Done():
				return
			}
		}
	}()
	log.Infof("memory_guard", "started (interval=%v, warn=%d%%, kill=%d%%, pressure_threshold=%.1f)",
		g.cfg.Interval, g.cfg.WarnPercent, g.cfg.KillPercent, g.cfg.PressureThreshold)
}

// Stop stops the guard and waits for the goroutine to exit.
func (g *MemoryGuard) Stop() {
	if g.cancel != nil {
		g.cancel()
	}
	if g.done != nil {
		<-g.done
	}
}

// checkOnce runs a single check cycle.
func (g *MemoryGuard) checkOnce() {
	getMemTotal := g.getMemTotalFn
	if getMemTotal == nil {
		getMemTotal = func() (int64, error) { return readMemTotal(defaultProcDir) }
	}
	getUserRSS := g.getUserRSSFn
	if getUserRSS == nil {
		getUserRSS = func(uid int) (int64, error) { return readUserRSS(defaultProcDir, uid) }
	}
	getSelfRSS := g.getSelfRSSFn
	if getSelfRSS == nil {
		getSelfRSS = func() (int64, error) { return readSelfRSS(defaultProcDir) }
	}
	getPressure := g.getPressureFn
	if getPressure == nil {
		getPressure = func() (float64, error) { return readMemoryPressure(defaultProcDir) }
	}

	memTotalKB, err := getMemTotal()
	if err != nil {
		log.Warnf("memory_guard", "read memtotal: %v", err)
		return
	}

	userRSSKB, err := getUserRSS(g.uid)
	if err != nil {
		log.Debugf("memory_guard", "read user RSS: %v", err)
		return
	}

	// Self (foci-gw) RSS. Read best-effort: a failure shouldn't gate the
	// guard. -1 is the sentinel for "unknown", surfaced in log lines so the
	// value isn't mistaken for a real zero.
	selfRSSKB, selfErr := getSelfRSS()
	selfMB := int64(-1)
	if selfErr == nil {
		selfMB = selfRSSKB / 1024
	} else {
		log.Debugf("memory_guard", "read self RSS: %v", selfErr)
	}

	pct := float64(userRSSKB) / float64(memTotalKB) * 100
	userMB := userRSSKB / 1024
	totalMB := memTotalKB / 1024

	// Below warn — reset dedup
	if int(pct) < g.cfg.WarnPercent {
		g.mu.Lock()
		if g.warnFired {
			log.Infof("memory_guard", "user RSS %dMB (%.1f%%, self %dMB) back below warn threshold", userMB, pct, selfMB)
		}
		g.warnFired = false
		g.mu.Unlock()
		return
	}

	// Above warn threshold — check pressure before acting
	pressure, err := getPressure()
	if err != nil {
		log.Debugf("memory_guard", "read pressure: %v (skipping action)", err)
		return
	}

	if pressure < g.cfg.PressureThreshold {
		log.Debugf("memory_guard", "user RSS %dMB (%.1f%%, self %dMB) above threshold but pressure %.2f < %.1f, no action",
			userMB, pct, selfMB, pressure, g.cfg.PressureThreshold)
		return
	}

	// Kill threshold
	if int(pct) >= g.cfg.KillPercent {
		g.mu.Lock()
		g.warnFired = true // skip separate warn
		g.mu.Unlock()

		g.doKill(userMB, totalMB, selfMB, pct, pressure)
		return
	}

	// Warn threshold
	g.mu.Lock()
	alreadyWarned := g.warnFired
	g.warnFired = true
	g.mu.Unlock()

	if !alreadyWarned {
		msg := fmt.Sprintf("system memory WARNING: foci user RSS %dMB / %dMB (%.1f%%, self %dMB) exceeds %d%% threshold (pressure=%.1f)",
			userMB, totalMB, pct, selfMB, g.cfg.WarnPercent, pressure)
		log.Warnf("memory_guard", "%s", msg)
		if g.warnFn != nil {
			g.warnFn(msg)
		}
	}
}

// doKill finds and kills the largest non-foci process owned by this user.
func (g *MemoryGuard) doKill(userMB, totalMB, selfMB int64, pct, pressure float64) {
	findLargest := g.findLargestFn
	if findLargest == nil {
		findLargest = func(uid, selfPid int) (int, string, int64, error) {
			return findLargestProcess(defaultProcDir, uid, selfPid)
		}
	}
	killProc := g.killProcessFn
	if killProc == nil {
		killProc = killProcess
	}

	pid, comm, rssKB, err := findLargest(g.uid, os.Getpid())
	if err != nil {
		log.Errorf("memory_guard", "find largest process: %v", err)
		if g.warnFn != nil {
			g.warnFn(fmt.Sprintf("system memory CRITICAL: user RSS %dMB / %dMB (%.1f%%, self %dMB), pressure=%.1f (threshold %.1f) — could not find process to kill: %v",
				userMB, totalMB, pct, selfMB, pressure, g.cfg.PressureThreshold, err))
		}
		return
	}

	rssMB := rssKB / 1024
	msg := fmt.Sprintf("system memory KILL: user RSS %dMB / %dMB (%.1f%%, self %dMB), pressure=%.1f (threshold %.1f) — killing %s (pid %d, %dMB RSS)",
		userMB, totalMB, pct, selfMB, pressure, g.cfg.PressureThreshold, comm, pid, rssMB)
	log.Errorf("memory_guard", "%s", msg)
	if g.warnFn != nil {
		g.warnFn(msg)
	}

	if err := killProc(pid); err != nil {
		log.Errorf("memory_guard", "kill pid %d: %v", pid, err)
	}
}

// readMemTotal reads MemTotal from procDir/meminfo in kB.
func readMemTotal(procDir string) (int64, error) {
	f, err := os.Open(filepath.Join(procDir, "meminfo"))
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, fmt.Errorf("unexpected MemTotal line: %s", line)
			}
			return strconv.ParseInt(fields[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}

// readUserRSS sums VmRSS (in kB) for all processes owned by the given UID.
// Reads /proc/[pid]/status directly — no external commands.
func readUserRSS(procDir string, uid int) (int64, error) {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", procDir, err)
	}

	var totalRSS int64
	uidStr := strconv.Itoa(uid)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Only numeric directories (PIDs)
		if len(e.Name()) == 0 || e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}

		statusPath := filepath.Join(procDir, e.Name(), "status")
		rss, owned := readStatusRSS(statusPath, uidStr)
		if owned {
			totalRSS += rss
		}
	}
	return totalRSS, nil
}

// readSelfRSS reads VmRSS (in kB) for the calling process (foci-gw) from
// procDir/<pid>/status. Returns -1 in the error path is left to callers; here
// we surface the read error. The status file is the calling process's own, so
// the UID ownership check is skipped.
func readSelfRSS(procDir string) (int64, error) {
	pidStr := strconv.Itoa(os.Getpid())
	statusPath := filepath.Join(procDir, pidStr, "status")
	f, err := os.Open(statusPath)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", statusPath, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				return strconv.ParseInt(fields[1], 10, 64)
			}
		}
	}
	return 0, fmt.Errorf("VmRSS not found in %s", statusPath)
}

// readStatusRSS reads a /proc/[pid]/status file and returns (VmRSS in kB, isOwnedByUID).
func readStatusRSS(path, uidStr string) (rssKB int64, owned bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[1] == uidStr {
				owned = true
			}
		}
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				rssKB, _ = strconv.ParseInt(fields[1], 10, 64)
			}
		}
	}
	return
}

// readMemoryPressure reads PSI memory avg10 from procDir/pressure/memory.
// Format: "some avg10=X.XX avg60=... avg300=... total=..."
func readMemoryPressure(procDir string) (float64, error) {
	data, err := os.ReadFile(filepath.Join(procDir, "pressure", "memory"))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "avg10=") {
				return strconv.ParseFloat(field[6:], 64)
			}
		}
	}
	return 0, fmt.Errorf("avg10 not found in /proc/pressure/memory")
}

// findLargestProcess finds the process with the largest RSS owned by uid,
// excluding selfPid (the foci process itself). Returns pid, comm, rssKB.
func findLargestProcess(procDir string, uid, selfPid int) (int, string, int64, error) {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return 0, "", 0, fmt.Errorf("read %s: %w", procDir, err)
	}

	var bestPid int
	var bestComm string
	var bestRSS int64
	uidStr := strconv.Itoa(uid)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if pid == selfPid {
			continue
		}

		statusPath := filepath.Join(procDir, e.Name(), "status")
		rss, owned, comm := readStatusFull(statusPath, uidStr)
		if owned && rss > bestRSS {
			bestPid = pid
			bestComm = comm
			bestRSS = rss
		}
	}

	if bestPid == 0 {
		return 0, "", 0, fmt.Errorf("no eligible process found for uid %d", uid)
	}
	return bestPid, bestComm, bestRSS, nil
}

// readStatusFull reads a /proc/[pid]/status file and returns (VmRSS in kB, isOwnedByUID, comm).
func readStatusFull(path, uidStr string) (rssKB int64, owned bool, comm string) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, ""
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Name:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				comm = fields[1]
			}
		}
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[1] == uidStr {
				owned = true
			}
		}
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				rssKB, _ = strconv.ParseInt(fields[1], 10, 64)
			}
		}
	}
	return
}

// killProcess sends SIGTERM, waits 5s, then SIGKILL if the process still exists.
func killProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}

	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("SIGTERM pid %d: %w", pid, err)
	}
	log.Infof("memory_guard", "sent SIGTERM to pid %d", pid)

	// Wait up to 5s for the process to exit
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			// Process is gone
			log.Infof("memory_guard", "pid %d exited after SIGTERM", pid)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Still alive — SIGKILL
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		// May already be gone
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			log.Infof("memory_guard", "pid %d exited before SIGKILL", pid)
			return nil
		}
		return fmt.Errorf("SIGKILL pid %d: %w", pid, err)
	}
	log.Warnf("memory_guard", "sent SIGKILL to pid %d (did not exit within 5s)", pid)
	return nil
}
