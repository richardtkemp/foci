package startup

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"foci/internal/log"
	"foci/internal/session"
)

const (
	systemStateKeyLastCleanShutdown = "last_clean_shutdown"
	systemStateKeyLastStartup       = "last_startup"
	systemStateKeyLastAlive         = "last_alive"
	cleanShutdownWindow             = 5 * time.Minute
	maxDiagnosticLines              = 10

	// HeartbeatInterval is how often the running process records a liveness
	// timestamp so restart diagnosis can measure actual downtime rather than
	// time-since-startup.
	HeartbeatInterval = 15 * time.Minute
)

type RestartClass string

const (
	ClassClean   RestartClass = "clean"
	ClassCrash   RestartClass = "crash"
	ClassReboot  RestartClass = "reboot"
	ClassUnknown RestartClass = "unknown"
)

type DiagnosisResult struct {
	Class         RestartClass
	LastAliveTime time.Time
	Diagnostics   []string
	Summary       string
}

// GetSystemUptime returns the system uptime. Replaceable for testing.
var GetSystemUptime = getSystemUptime

func DiagnoseRestart(idx *session.SessionIndex, startTime time.Time, logsDir string) *DiagnosisResult {
	// Read all proof-of-life timestamps to find the most recent.
	// After a crash, last_clean_shutdown is stale, but last_startup
	// reflects when the crashed process started and last_alive (the
	// periodic heartbeat) reflects roughly when it stopped running —
	// so the measured gap approximates actual downtime, not the full
	// time since the process started.
	var shutdownUnix, startupUnix, aliveUnix int64
	if raw, err := idx.GetSystemState(systemStateKeyLastCleanShutdown); err == nil && raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
			shutdownUnix = v
		}
	}
	if raw, err := idx.GetSystemState(systemStateKeyLastStartup); err == nil && raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
			startupUnix = v
		}
	}
	if raw, err := idx.GetSystemState(systemStateKeyLastAlive); err == nil && raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
			aliveUnix = v
		}
	}

	// Record this startup for the next restart diagnosis.
	if err := idx.SetSystemState(systemStateKeyLastStartup, strconv.FormatInt(startTime.Unix(), 10)); err != nil {
		log.Warnf("startup", "record startup time: %v", err)
	}

	systemUptime, err := GetSystemUptime()
	if err != nil {
		log.Debugf("startup", "could not read system uptime: %v", err)
	}

	// Use the most recent of shutdown/startup/heartbeat as the reference
	// point. A clean shutdown only counts as clean if the shutdown record
	// is the most recent — a later startup or heartbeat means the process
	// ran (and stopped) after that shutdown was recorded.
	lastAliveUnix := shutdownUnix
	wasCleanShutdown := true
	if startupUnix > lastAliveUnix {
		lastAliveUnix = startupUnix
		wasCleanShutdown = false
	}
	if aliveUnix > lastAliveUnix {
		lastAliveUnix = aliveUnix
		wasCleanShutdown = false
	}

	result := &DiagnosisResult{
		Class: ClassUnknown,
	}

	if lastAliveUnix == 0 {
		// No prior record at all.
		if systemUptime > 0 && systemUptime < 5*time.Minute {
			result.Class = ClassReboot
			result.Summary = "system recently rebooted (no prior shutdown record)"
		} else {
			result.Summary = "no prior shutdown record"
		}
		result.Diagnostics = gatherDiagnostics(logsDir, startTime.Add(-1*time.Hour))
		return result
	}

	result.LastAliveTime = time.Unix(lastAliveUnix, 0)
	gap := startTime.Sub(result.LastAliveTime)

	if gap < 0 {
		result.Class = ClassUnknown
		result.Summary = "last-alive timestamp is in the future"
	} else if wasCleanShutdown && gap <= cleanShutdownWindow {
		result.Class = ClassClean
		result.Summary = fmt.Sprintf("clean shutdown %s ago", gap.Round(time.Second))
	} else if systemUptime > 0 && systemUptime < gap {
		result.Class = ClassReboot
		result.Summary = fmt.Sprintf("system reboot detected (uptime %s < gap %s)", systemUptime.Round(time.Second), gap.Round(time.Second))
		result.Diagnostics = gatherDiagnostics(logsDir, result.LastAliveTime)
	} else {
		result.Class = ClassCrash
		result.Summary = fmt.Sprintf("unexpected restart (gap %s)", gap.Round(time.Second))
		result.Diagnostics = gatherDiagnostics(logsDir, result.LastAliveTime)
	}

	return result
}

func RecordCleanShutdown(idx *session.SessionIndex) error {
	return idx.SetSystemState(systemStateKeyLastCleanShutdown, strconv.FormatInt(time.Now().Unix(), 10))
}

// RecordHeartbeat writes the current time as the last_alive liveness timestamp.
func RecordHeartbeat(idx *session.SessionIndex) error {
	return idx.SetSystemState(systemStateKeyLastAlive, strconv.FormatInt(time.Now().Unix(), 10))
}

// RunHeartbeat records a liveness timestamp every interval until ctx is
// cancelled. Intended to run in its own goroutine. The heartbeat lets restart
// diagnosis measure actual downtime (time since the last beat) rather than the
// full time since the process started. The caller must cancel ctx BEFORE
// recording a clean shutdown so the shutdown timestamp stays the most recent
// proof-of-life on a clean exit.
func RunHeartbeat(ctx context.Context, idx *session.SessionIndex, interval time.Duration) {
	if interval <= 0 {
		interval = HeartbeatInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := RecordHeartbeat(idx); err != nil {
				log.Warnf("startup", "record heartbeat: %v", err)
			}
		}
	}
}

// WasCleanShutdown returns true if the last shutdown was clean (recorded
// within the clean window). Used to decide whether to skip the session
// index rebuild on startup.
func WasCleanShutdown(idx *session.SessionIndex) bool {
	raw, err := idx.GetSystemState(systemStateKeyLastCleanShutdown)
	if err != nil || raw == "" {
		return false
	}
	ts, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return false
	}
	shutdownTime := time.Unix(ts, 0)
	return time.Since(shutdownTime) < cleanShutdownWindow
}

func getSystemUptime() (time.Duration, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}

	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, fmt.Errorf("unexpected format in /proc/uptime")
	}

	uptimeSeconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse uptime: %w", err)
	}

	return time.Duration(uptimeSeconds * float64(time.Second)), nil
}

func gatherDiagnostics(logsDir string, since time.Time) []string {
	var findings []string

	logFile := filepath.Join(logsDir, "foci.log")
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		return findings
	}

	f, err := os.Open(logFile)
	if err != nil {
		log.Debugf("startup", "could not open log file: %v", err)
		return findings
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	errorPattern := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(Z|[+-]\d{2}:\d{2})\s+(ERROR|FATAL)\s+`)
	sinceTrunc := since.Truncate(time.Second) // log timestamps have second precision

	var recentErrors []string
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 20 {
			continue
		}

		// Extract RFC3339 timestamp: 20 chars for "Z" suffix, 25 for "+HH:MM" offset
		tsEnd := 20
		if len(line) >= 25 && (line[19] == '+' || line[19] == '-') {
			tsEnd = 25
		}
		lineT, err := time.Parse(time.RFC3339, line[:tsEnd])
		if err != nil {
			continue
		}
		if lineT.Before(sinceTrunc) {
			continue
		}

		if errorPattern.MatchString(line) {
			recentErrors = append(recentErrors, line)
			if len(recentErrors) >= maxDiagnosticLines {
				break
			}
		}
	}

	if len(recentErrors) > 0 {
		findings = append(findings, fmt.Sprintf("%d error(s) in logs since %s:", len(recentErrors), since.Format("15:04:05")))
		for _, err := range recentErrors {
			if len(err) > 200 {
				err = err[:200] + "..."
			}
			findings = append(findings, "  "+err)
		}
	}

	return findings
}

func (d *DiagnosisResult) FormatNotification() string {
	var sb strings.Builder

	switch d.Class {
	case ClassClean:
		return ""
	case ClassCrash:
		sb.WriteString("⚠️ Unexpected restart")
	case ClassReboot:
		sb.WriteString("🔄 System reboot detected")
	default:
		if len(d.Diagnostics) == 0 {
			return ""
		}
		sb.WriteString("ℹ️ Startup diagnostics")
	}

	if d.Summary != "" {
		sb.WriteString("\n")
		sb.WriteString(d.Summary)
	}

	if len(d.Diagnostics) > 0 {
		sb.WriteString("\n\n")
		for _, line := range d.Diagnostics {
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}

	return sb.String()
}
