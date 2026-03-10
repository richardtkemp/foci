package tools

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"foci/internal/log"
)

// TmuxMemoryConfig holds the parsed configuration for the tmux memory monitor.
type TmuxMemoryConfig struct {
	CheckInterval time.Duration
	WarnStr       string // raw threshold string, e.g. "10%", "512mb", "2gb"
	CriticalStr   string
	KillStr       string
}

// TmuxMemoryMonitor checks the RSS of the tmux server process at regular
// intervals and fires notifications (or kills tmux) when thresholds are
// exceeded. Thresholds are resolved against physical RAM at check time.
type TmuxMemoryMonitor struct {
	cfg       TmuxMemoryConfig
	notifyFn  func(string) // send notification to user
	cleanupFn func()       // called after tmux kill-server

	mu            sync.Mutex
	warnFired     bool
	criticalFired bool

	cancel context.CancelFunc
	done   chan struct{}

	// Overridable for testing.
	getTmuxRSSFn func() (int64, error)
	getMemTotalFn func() (int64, error)
	killTmuxFn   func() ([]string, error)
}

// NewTmuxMemoryMonitor creates a monitor. notifyFn is called for user
// alerts, cleanupFn is called after tmux kill-server so tool instances can
// clear their state.
func NewTmuxMemoryMonitor(cfg TmuxMemoryConfig, notifyFn func(string), cleanupFn func()) *TmuxMemoryMonitor {
	return &TmuxMemoryMonitor{
		cfg:       cfg,
		notifyFn:  notifyFn,
		cleanupFn: cleanupFn,
	}
}

// Start launches the background check goroutine. Cancels on ctx.Done().
func (m *TmuxMemoryMonitor) Start(ctx context.Context) {
	if m.cfg.CheckInterval <= 0 {
		return
	}
	monCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.done = make(chan struct{})

	go func() {
		defer close(m.done)
		ticker := time.NewTicker(m.cfg.CheckInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				m.checkOnce()
			case <-monCtx.Done():
				return
			}
		}
	}()
	log.Infof("tmux_memory", "monitor started (interval=%v, warn=%s, critical=%s, kill=%s)",
		m.cfg.CheckInterval, m.cfg.WarnStr, m.cfg.CriticalStr, m.cfg.KillStr)
}

// Stop stops the monitor and waits for the goroutine to exit.
func (m *TmuxMemoryMonitor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	if m.done != nil {
		<-m.done
	}
}

// checkOnce runs a single check cycle. Exported for testing.
func (m *TmuxMemoryMonitor) checkOnce() {
	getRSS := m.getTmuxRSSFn
	if getRSS == nil {
		getRSS = getTmuxRSS
	}
	getMemTotal := m.getMemTotalFn
	if getMemTotal == nil {
		getMemTotal = getMemTotal_
	}

	rssKB, err := getRSS()
	if err != nil {
		log.Debugf("tmux_memory", "get tmux RSS: %v", err)
		return
	}

	memTotalKB, err := getMemTotal()
	if err != nil {
		log.Warnf("tmux_memory", "get memtotal: %v", err)
		return
	}

	killKB, err := ParseThreshold(m.cfg.KillStr, memTotalKB)
	if err != nil {
		log.Warnf("tmux_memory", "parse kill threshold: %v", err)
		return
	}
	criticalKB, err := ParseThreshold(m.cfg.CriticalStr, memTotalKB)
	if err != nil {
		log.Warnf("tmux_memory", "parse critical threshold: %v", err)
		return
	}
	warnKB, err := ParseThreshold(m.cfg.WarnStr, memTotalKB)
	if err != nil {
		log.Warnf("tmux_memory", "parse warn threshold: %v", err)
		return
	}

	rssMB := rssKB / 1024
	totalMB := memTotalKB / 1024
	pct := float64(rssKB) / float64(memTotalKB) * 100

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check thresholds from highest to lowest
	if rssKB >= killKB {
		if !m.criticalFired {
			// Fire critical first if it hasn't fired
			m.criticalFired = true
			m.notifyFn(fmt.Sprintf("🔴 tmux memory CRITICAL: %dMB / %dMB (%.1f%%) — exceeds %s threshold", rssMB, totalMB, pct, m.cfg.CriticalStr))
		}
		log.Errorf("tmux_memory", "tmux RSS %dMB (%.1f%%) exceeds kill threshold %s — killing tmux server", rssMB, pct, m.cfg.KillStr)
		m.doKill(rssMB, totalMB, pct)
		return
	}

	if rssKB >= criticalKB {
		if !m.criticalFired {
			m.criticalFired = true
			log.Warnf("tmux_memory", "tmux RSS %dMB (%.1f%%) exceeds critical threshold %s", rssMB, pct, m.cfg.CriticalStr)
			m.notifyFn(fmt.Sprintf("🔴 tmux memory CRITICAL: %dMB / %dMB (%.1f%%) — exceeds %s threshold", rssMB, totalMB, pct, m.cfg.CriticalStr))
		}
		// Also ensure warn is marked as fired
		m.warnFired = true
		return
	}

	if rssKB >= warnKB {
		if !m.warnFired {
			m.warnFired = true
			log.Warnf("tmux_memory", "tmux RSS %dMB (%.1f%%) exceeds warn threshold %s", rssMB, pct, m.cfg.WarnStr)
			m.notifyFn(fmt.Sprintf("⚠️ tmux memory WARNING: %dMB / %dMB (%.1f%%) — exceeds %s threshold", rssMB, totalMB, pct, m.cfg.WarnStr))
		}
		// Clear critical if we dropped below it
		if m.criticalFired {
			m.criticalFired = false
		}
		return
	}

	// Below all thresholds — reset dedup state
	if m.warnFired || m.criticalFired {
		log.Infof("tmux_memory", "tmux RSS %dMB (%.1f%%) back below thresholds", rssMB, pct)
	}
	m.warnFired = false
	m.criticalFired = false
}

// doKill kills the tmux server and fires cleanup. Must be called with m.mu held.
func (m *TmuxMemoryMonitor) doKill(rssMB, totalMB int64, pct float64) {
	killFn := m.killTmuxFn
	if killFn == nil {
		killFn = killTmuxServer
	}

	sessions, err := killFn()
	if err != nil {
		log.Errorf("tmux_memory", "kill-server failed: %v", err)
		m.notifyFn(fmt.Sprintf("🔴 tmux memory KILL: %dMB / %dMB (%.1f%%) — kill-server FAILED: %v", rssMB, totalMB, pct, err))
	} else {
		sessionList := "none"
		if len(sessions) > 0 {
			sessionList = strings.Join(sessions, ", ")
		}
		m.notifyFn(fmt.Sprintf("🔴 tmux memory KILL: %dMB / %dMB (%.1f%%) — killed tmux server. Sessions destroyed: %s", rssMB, totalMB, pct, sessionList))
	}

	// Reset all dedup state
	m.warnFired = false
	m.criticalFired = false

	// Fire cleanup callback
	if m.cleanupFn != nil {
		m.cleanupFn()
	}
}

// ParseThreshold parses a threshold string into kB. Formats:
//   - "10%" — percentage of memTotalKB
//   - "512mb" — absolute megabytes → kB
//   - "2gb" — absolute gigabytes → kB
func ParseThreshold(s string, memTotalKB int64) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty threshold")
	}

	if strings.HasSuffix(s, "%") {
		v, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid percentage %q: %w", s, err)
		}
		if v <= 0 || v > 100 {
			return 0, fmt.Errorf("percentage must be between 0 and 100, got %v", v)
		}
		return int64(float64(memTotalKB) * v / 100), nil
	}

	if strings.HasSuffix(s, "gb") {
		v, err := strconv.ParseFloat(s[:len(s)-2], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid gigabytes %q: %w", s, err)
		}
		if v <= 0 {
			return 0, fmt.Errorf("gigabytes must be positive, got %v", v)
		}
		return int64(v * 1024 * 1024), nil // GB → kB
	}

	if strings.HasSuffix(s, "mb") {
		v, err := strconv.ParseFloat(s[:len(s)-2], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid megabytes %q: %w", s, err)
		}
		if v <= 0 {
			return 0, fmt.Errorf("megabytes must be positive, got %v", v)
		}
		return int64(v * 1024), nil // MB → kB
	}

	return 0, fmt.Errorf("unknown format %q: use \"N%%\", \"Nmb\", or \"Ngb\"", s)
}

// getTmuxRSS returns the RSS (in kB) of the tmux server process.
// Finds PID via `tmux display-message -p '#{pid}'`, then reads /proc/{pid}/status.
func getTmuxRSS() (int64, error) {
	out, err := runTmux(context.Background(), "display-message", "-p", "#{pid}")
	if err != nil {
		return 0, fmt.Errorf("tmux display-message: %w", err)
	}
	pidStr := strings.TrimSpace(out)
	if pidStr == "" {
		return 0, fmt.Errorf("tmux returned empty PID")
	}
	return readProcVmRSS(pidStr)
}

// readProcVmRSS reads VmRSS from /proc/{pid}/status and returns it in kB.
func readProcVmRSS(pid string) (int64, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%s/status", pid))
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, fmt.Errorf("unexpected VmRSS line: %s", line)
			}
			return strconv.ParseInt(fields[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("VmRSS not found in /proc/%s/status", pid)
}

// getMemTotal_ reads MemTotal from /proc/meminfo in kB.
func getMemTotal_() (int64, error) {
	f, err := os.Open("/proc/meminfo")
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

// killTmuxServer lists active sessions and then kills the tmux server.
// Returns the list of session names that were destroyed.
func killTmuxServer() ([]string, error) {
	// List sessions first for the notification
	out, err := runTmux(context.Background(), "list-sessions", "-F", "#{session_name}")
	var sessions []string
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				sessions = append(sessions, line)
			}
		}
	}
	log.Infof("tmux_memory", "killing tmux server; active sessions: %v", sessions)

	// Kill server
	if _, err := runTmux(context.Background(), "kill-server"); err != nil {
		return sessions, fmt.Errorf("tmux kill-server: %w", err)
	}
	return sessions, nil
}
