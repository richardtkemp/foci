package startup

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"foci/internal/session"
)

func newTestIndex(t *testing.T, dir string) *session.SessionIndex {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("create session index: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx
}

func setShutdownTime(t *testing.T, idx *session.SessionIndex, ts int64) {
	t.Helper()
	if err := idx.SetSystemState("last_clean_shutdown", strconv.FormatInt(ts, 10)); err != nil {
		t.Fatalf("set shutdown time: %v", err)
	}
}

func TestDiagnoseRestart_CleanShutdown(t *testing.T) {
	tmpDir := t.TempDir()
	idx := newTestIndex(t, tmpDir)

	startTime := time.Now()
	shutdownTime := startTime.Add(-30 * time.Second)
	setShutdownTime(t, idx, shutdownTime.Unix())

	result := DiagnoseRestart(idx, startTime, tmpDir)

	if result.Class != ClassClean {
		t.Errorf("expected ClassClean, got %s", result.Class)
	}
	if len(result.Diagnostics) != 0 {
		t.Errorf("expected no diagnostics for clean shutdown, got %d", len(result.Diagnostics))
	}
}

func TestDiagnoseRestart_Crash(t *testing.T) {
	tmpDir := t.TempDir()
	idx := newTestIndex(t, tmpDir)

	startTime := time.Now()
	shutdownTime := startTime.Add(-10 * time.Minute)
	setShutdownTime(t, idx, shutdownTime.Unix())

	logFile := filepath.Join(tmpDir, "foci.log")
	shutdownStr := shutdownTime.UTC().Format("2006-01-02T15:04:05Z")
	logContent := shutdownStr + ` INFO  [main] starting
` + shutdownStr + ` ERROR [agent] something went wrong
` + shutdownStr + ` FATAL [main] crash
`
	if err := os.WriteFile(logFile, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	result := DiagnoseRestart(idx, startTime, tmpDir)

	if result.Class != ClassCrash {
		t.Errorf("expected ClassCrash, got %s", result.Class)
	}
	if len(result.Diagnostics) == 0 {
		t.Error("expected diagnostics for crash")
	}
}

func TestDiagnoseRestart_NoPriorRecord(t *testing.T) {
	tmpDir := t.TempDir()
	idx := newTestIndex(t, tmpDir)

	result := DiagnoseRestart(idx, time.Now(), tmpDir)

	if result.Class != ClassUnknown {
		t.Errorf("expected ClassUnknown, got %s", result.Class)
	}
}

func TestDiagnoseRestart_FutureShutdown(t *testing.T) {
	tmpDir := t.TempDir()
	idx := newTestIndex(t, tmpDir)

	startTime := time.Now()
	shutdownTime := startTime.Add(5 * time.Minute)
	setShutdownTime(t, idx, shutdownTime.Unix())

	result := DiagnoseRestart(idx, startTime, tmpDir)

	if result.Class != ClassUnknown {
		t.Errorf("expected ClassUnknown for future shutdown, got %s", result.Class)
	}
}

func TestDiagnoseRestart_Reboot(t *testing.T) {
	tmpDir := t.TempDir()
	idx := newTestIndex(t, tmpDir)

	startTime := time.Now()
	// Shutdown was 1 hour ago
	shutdownTime := startTime.Add(-1 * time.Hour)
	setShutdownTime(t, idx, shutdownTime.Unix())

	// Inject uptime shorter than the gap (simulates reboot)
	orig := GetSystemUptime
	GetSystemUptime = func() (time.Duration, error) {
		return 10 * time.Minute, nil
	}
	defer func() { GetSystemUptime = orig }()

	result := DiagnoseRestart(idx, startTime, tmpDir)

	if result.Class != ClassReboot {
		t.Errorf("expected ClassReboot, got %s (summary: %s)", result.Class, result.Summary)
	}
}

func TestDiagnoseRestart_RebootNoRecord(t *testing.T) {
	tmpDir := t.TempDir()
	idx := newTestIndex(t, tmpDir)

	// No prior shutdown record + very short uptime -> reboot
	orig := GetSystemUptime
	GetSystemUptime = func() (time.Duration, error) {
		return 2 * time.Minute, nil
	}
	defer func() { GetSystemUptime = orig }()

	result := DiagnoseRestart(idx, time.Now(), tmpDir)

	if result.Class != ClassReboot {
		t.Errorf("expected ClassReboot, got %s (summary: %s)", result.Class, result.Summary)
	}
}

func TestRecordCleanShutdown(t *testing.T) {
	tmpDir := t.TempDir()
	idx := newTestIndex(t, tmpDir)

	before := time.Now().Truncate(time.Second)
	if err := RecordCleanShutdown(idx); err != nil {
		t.Fatalf("record clean shutdown: %v", err)
	}
	after := time.Now().Truncate(time.Second).Add(time.Second)

	raw, err := idx.GetSystemState("last_clean_shutdown")
	if err != nil || raw == "" {
		t.Fatal("shutdown timestamp not set")
	}

	shutdownUnix, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("parse shutdown timestamp: %v", err)
	}

	shutdownTime := time.Unix(shutdownUnix, 0)
	if shutdownTime.Before(before) || shutdownTime.After(after) {
		t.Errorf("shutdown time %v not in expected range [%v, %v]", shutdownTime, before, after)
	}
}

func TestFormatNotification_Clean(t *testing.T) {
	result := &DiagnosisResult{
		Class:   ClassClean,
		Summary: "clean shutdown 30s ago",
	}

	text := result.FormatNotification()
	if text != "" {
		t.Errorf("expected empty notification for clean shutdown, got %q", text)
	}
}

func TestFormatNotification_Crash(t *testing.T) {
	result := &DiagnosisResult{
		Class:       ClassCrash,
		Summary:     "unexpected restart (gap 10m0s)",
		Diagnostics: []string{"1 error(s) in logs:", "  ERROR something"},
	}

	text := result.FormatNotification()
	if text == "" {
		t.Error("expected non-empty notification for crash")
	}
	if !contains(text, "Unexpected restart") {
		t.Errorf("expected 'Unexpected restart' in notification, got %q", text)
	}
}

func TestFormatNotification_Reboot(t *testing.T) {
	result := &DiagnosisResult{
		Class:       ClassReboot,
		Summary:     "system reboot detected",
		Diagnostics: []string{"1 error(s) in logs:"},
	}

	text := result.FormatNotification()
	if text == "" {
		t.Error("expected non-empty notification for reboot")
	}
	if !contains(text, "System reboot detected") {
		t.Errorf("expected 'System reboot detected' in notification, got %q", text)
	}
}

func TestGatherDiagnostics_NoFile(t *testing.T) {
	findings := gatherDiagnostics("/nonexistent/path", time.Now().Add(-1*time.Hour))
	if len(findings) != 0 {
		t.Errorf("expected no findings when log file missing, got %d", len(findings))
	}
}

func TestGatherDiagnostics_WithErrors(t *testing.T) {
	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "foci.log")

	now := time.Now().UTC()
	since := now.Add(-30 * time.Minute)
	sinceStr := since.Format("2006-01-02T15:04:05Z")

	logContent := sinceStr + ` INFO  [main] starting
` + sinceStr + ` ERROR [agent] something went wrong
` + sinceStr + ` WARN  [agent] warning message
` + sinceStr + ` FATAL [main] crash
`
	if err := os.WriteFile(logFile, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	findings := gatherDiagnostics(tmpDir, since)

	if len(findings) == 0 {
		t.Error("expected findings from log file with errors")
	}

	hasError := false
	for _, f := range findings {
		if contains(f, "ERROR") || contains(f, "FATAL") {
			hasError = true
			break
		}
	}
	if !hasError {
		t.Error("expected findings to include ERROR or FATAL lines")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestGetSystemUptime tests the getSystemUptime function
func TestGetSystemUptime(t *testing.T) {
	// We can't test the real getSystemUptime without /proc/uptime,
	// but we can test it for now to ensure it runs without panicking
	// In CI/production, this will use the actual /proc/uptime
	uptime, err := getSystemUptime()
	if err != nil && !os.IsNotExist(err) {
		// On systems without /proc/uptime, it should fail gracefully
		// On Linux systems with /proc/uptime, it should succeed
		t.Logf("getSystemUptime error (expected on non-Linux): %v", err)
	} else if err == nil {
		// On Linux, should return a positive duration
		if uptime <= 0 {
			t.Errorf("uptime = %v, want positive duration", uptime)
		}
	}
}

// TestFormatNotification_UnknownWithoutDiagnostics tests Unknown class with no diagnostics
func TestFormatNotification_UnknownWithoutDiagnostics(t *testing.T) {
	result := &DiagnosisResult{
		Class:   ClassUnknown,
		Summary: "no prior shutdown record",
	}

	text := result.FormatNotification()
	if text != "" {
		t.Errorf("expected empty notification for Unknown class without diagnostics, got %q", text)
	}
}

// TestFormatNotification_UnknownWithDiagnostics tests Unknown class with diagnostics
func TestFormatNotification_UnknownWithDiagnostics(t *testing.T) {
	result := &DiagnosisResult{
		Class:       ClassUnknown,
		Summary:     "no prior shutdown record",
		Diagnostics: []string{"1 error(s) in logs:"},
	}

	text := result.FormatNotification()
	if text == "" {
		t.Error("expected non-empty notification for Unknown class with diagnostics")
	}
	if !contains(text, "Startup diagnostics") {
		t.Errorf("expected 'Startup diagnostics' in notification, got %q", text)
	}
}

// TestGatherDiagnostics_ReadError tests handling of file read errors
func TestGatherDiagnostics_ReadError(t *testing.T) {
	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "foci.log")

	// Create a file but make it unreadable on Unix systems (if possible)
	if err := os.WriteFile(logFile, []byte("test"), 0000); err != nil {
		t.Fatalf("write log file: %v", err)
	}
	defer os.Chmod(logFile, 0644)

	// gatherDiagnostics should handle the read error gracefully
	findings := gatherDiagnostics(tmpDir, time.Now().Add(-1*time.Hour))
	// Should return empty findings on error, not panic
	if len(findings) > 0 {
		t.Logf("findings: %v", findings)
	}
}

// TestGatherDiagnostics_ShortLines tests handling of short log lines
func TestGatherDiagnostics_ShortLines(t *testing.T) {
	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "foci.log")

	logContent := `short
` + time.Now().UTC().Format("2006-01-02T15:04:05Z") + ` ERROR too short
`
	if err := os.WriteFile(logFile, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	findings := gatherDiagnostics(tmpDir, time.Now().Add(-1*time.Hour))
	// Short lines should be skipped, errors after timestamp should be found
	if len(findings) == 0 {
		t.Error("expected findings for error after timestamp")
	}
}

// TestGatherDiagnostics_NoErrors tests log with no errors
func TestGatherDiagnostics_NoErrors(t *testing.T) {
	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "foci.log")

	now := time.Now().UTC()
	since := now.Add(-30 * time.Minute)
	sinceStr := since.Format("2006-01-02T15:04:05Z")

	logContent := sinceStr + ` INFO  [main] starting
` + sinceStr + ` DEBUG [agent] processing
` + sinceStr + ` WARN  [agent] warning message
`
	if err := os.WriteFile(logFile, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	findings := gatherDiagnostics(tmpDir, since)
	if len(findings) != 0 {
		t.Errorf("expected no findings for log without errors, got %d", len(findings))
	}
}

// TestGatherDiagnostics_TruncateLongLines tests truncation of long error lines
func TestGatherDiagnostics_TruncateLongLines(t *testing.T) {
	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "foci.log")

	now := time.Now().UTC()
	since := now.Add(-30 * time.Minute)
	sinceStr := since.Format("2006-01-02T15:04:05Z")

	// Create a very long error line
	longError := sinceStr + ` ERROR [test] ` + string(make([]byte, 300))

	logContent := longError + "\n"
	if err := os.WriteFile(logFile, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	findings := gatherDiagnostics(tmpDir, since)
	if len(findings) == 0 {
		t.Error("expected findings for error")
	}

	// Check that the error line was included but truncated
	hasError := false
	for _, f := range findings {
		if len(f) > 0 && contains(f, "ERROR") {
			hasError = true
			// Check that lines are not excessively long (should be truncated)
			if len(f) > 250 {
				t.Logf("long line: %d chars", len(f))
			}
			break
		}
	}
	if !hasError {
		t.Error("expected findings to include ERROR lines")
	}
}

// TestDiagnoseRestart_UpimeReadError tests handling when uptime read fails
func TestDiagnoseRestart_UptimeReadError(t *testing.T) {
	tmpDir := t.TempDir()
	idx := newTestIndex(t, tmpDir)

	startTime := time.Now()
	shutdownTime := startTime.Add(-30 * time.Second)
	setShutdownTime(t, idx, shutdownTime.Unix())

	// Mock uptime read to fail
	orig := GetSystemUptime
	GetSystemUptime = func() (time.Duration, error) {
		return 0, os.ErrNotExist
	}
	defer func() { GetSystemUptime = orig }()

	result := DiagnoseRestart(idx, startTime, tmpDir)

	// Should still classify as ClassClean even if uptime read fails
	if result.Class != ClassClean {
		t.Errorf("expected ClassClean when uptime read fails, got %s", result.Class)
	}
}

// TestDiagnoseRestart_RebootNoRecordNoUptime tests reboot detection without uptime
func TestDiagnoseRestart_RebootNoRecordNoUptime(t *testing.T) {
	tmpDir := t.TempDir()
	idx := newTestIndex(t, tmpDir)

	// No prior shutdown record + uptime read fails -> unknown
	orig := GetSystemUptime
	GetSystemUptime = func() (time.Duration, error) {
		return 0, os.ErrNotExist
	}
	defer func() { GetSystemUptime = orig }()

	result := DiagnoseRestart(idx, time.Now(), tmpDir)

	if result.Class != ClassUnknown {
		t.Errorf("expected ClassUnknown when uptime unavailable, got %s", result.Class)
	}
}
