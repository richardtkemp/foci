package startup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"foci/internal/log"
	"foci/internal/state"
)

const (
	StateKeyLastCleanShutdown = "system:last_clean_shutdown"
	cleanShutdownWindow       = 5 * time.Minute
	maxDiagnosticLines        = 10
)

type RestartClass string

const (
	ClassClean   RestartClass = "clean"
	ClassCrash   RestartClass = "crash"
	ClassReboot  RestartClass = "reboot"
	ClassUnknown RestartClass = "unknown"
)

type DiagnosisResult struct {
	Class        RestartClass
	ShutdownTime time.Time
	Diagnostics  []string
	Summary      string
}

// GetSystemUptime returns the system uptime. Replaceable for testing.
var GetSystemUptime = getSystemUptime

func DiagnoseRestart(st *state.Store, startTime time.Time, logsDir string) *DiagnosisResult {
	var shutdownUnix int64
	hasShutdown := st.Get(StateKeyLastCleanShutdown, &shutdownUnix)

	systemUptime, err := GetSystemUptime()
	if err != nil {
		log.Debugf("startup", "could not read system uptime: %v", err)
	}

	result := &DiagnosisResult{
		Class: ClassUnknown,
	}

	if hasShutdown {
		result.ShutdownTime = time.Unix(shutdownUnix, 0)
		shutdownAge := startTime.Sub(result.ShutdownTime)

		if shutdownAge < 0 {
			result.Class = ClassUnknown
			result.Summary = "shutdown timestamp is in the future"
		} else if shutdownAge <= cleanShutdownWindow {
			result.Class = ClassClean
			result.Summary = fmt.Sprintf("clean shutdown %s ago", shutdownAge.Round(time.Second))
		} else if systemUptime > 0 && systemUptime < shutdownAge {
			result.Class = ClassReboot
			result.Summary = fmt.Sprintf("system reboot detected (uptime %s < gap %s)", systemUptime.Round(time.Second), shutdownAge.Round(time.Second))
			result.Diagnostics = gatherDiagnostics(logsDir, result.ShutdownTime)
		} else {
			result.Class = ClassCrash
			result.Summary = fmt.Sprintf("unexpected restart (gap %s)", shutdownAge.Round(time.Second))
			result.Diagnostics = gatherDiagnostics(logsDir, result.ShutdownTime)
		}
	} else {
		result.Class = ClassUnknown
		if systemUptime > 0 && systemUptime < 5*time.Minute {
			result.Class = ClassReboot
			result.Summary = "system recently rebooted (no prior shutdown record)"
		} else {
			result.Summary = "no prior shutdown record"
		}
		result.Diagnostics = gatherDiagnostics(logsDir, startTime.Add(-1*time.Hour))
	}

	return result
}

func RecordCleanShutdown(st *state.Store) error {
	return st.Set(StateKeyLastCleanShutdown, time.Now().Unix())
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

	errorPattern := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z\s+(ERROR|FATAL)\s+`)
	sinceStr := since.UTC().Format("2006-01-02T15:04:05Z")

	var recentErrors []string
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 20 {
			continue
		}

		lineTime := line[:20]
		if lineTime < sinceStr {
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
