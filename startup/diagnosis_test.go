package startup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/state"
)

func TestDiagnoseRestart_CleanShutdown(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")
	st := state.New(statePath)
	if err := st.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	startTime := time.Now()
	shutdownTime := startTime.Add(-30 * time.Second)
	if err := st.Set(StateKeyLastCleanShutdown, shutdownTime.Unix()); err != nil {
		t.Fatalf("set shutdown time: %v", err)
	}

	result := DiagnoseRestart(st, startTime, tmpDir)

	if result.Class != ClassClean {
		t.Errorf("expected ClassClean, got %s", result.Class)
	}
	if len(result.Diagnostics) != 0 {
		t.Errorf("expected no diagnostics for clean shutdown, got %d", len(result.Diagnostics))
	}
}

func TestDiagnoseRestart_Crash(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")
	st := state.New(statePath)
	if err := st.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	startTime := time.Now()
	shutdownTime := startTime.Add(-10 * time.Minute)
	if err := st.Set(StateKeyLastCleanShutdown, shutdownTime.Unix()); err != nil {
		t.Fatalf("set shutdown time: %v", err)
	}

	logFile := filepath.Join(tmpDir, "foci.log")
	shutdownStr := shutdownTime.UTC().Format("2006-01-02T15:04:05Z")
	logContent := shutdownStr + ` INFO  [main] starting
` + shutdownStr + ` ERROR [agent] something went wrong
` + shutdownStr + ` FATAL [main] crash
`
	if err := os.WriteFile(logFile, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	result := DiagnoseRestart(st, startTime, tmpDir)

	if result.Class != ClassCrash {
		t.Errorf("expected ClassCrash, got %s", result.Class)
	}
	if len(result.Diagnostics) == 0 {
		t.Error("expected diagnostics for crash")
	}
}

func TestDiagnoseRestart_NoPriorRecord(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")
	st := state.New(statePath)
	if err := st.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	result := DiagnoseRestart(st, time.Now(), tmpDir)

	if result.Class != ClassUnknown {
		t.Errorf("expected ClassUnknown, got %s", result.Class)
	}
}

func TestDiagnoseRestart_FutureShutdown(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")
	st := state.New(statePath)
	if err := st.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	startTime := time.Now()
	shutdownTime := startTime.Add(5 * time.Minute)
	if err := st.Set(StateKeyLastCleanShutdown, shutdownTime.Unix()); err != nil {
		t.Fatalf("set shutdown time: %v", err)
	}

	result := DiagnoseRestart(st, startTime, tmpDir)

	if result.Class != ClassUnknown {
		t.Errorf("expected ClassUnknown for future shutdown, got %s", result.Class)
	}
}

func TestDiagnoseRestart_Reboot(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")
	st := state.New(statePath)
	if err := st.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	startTime := time.Now()
	// Shutdown was 1 hour ago
	shutdownTime := startTime.Add(-1 * time.Hour)
	if err := st.Set(StateKeyLastCleanShutdown, shutdownTime.Unix()); err != nil {
		t.Fatalf("set shutdown time: %v", err)
	}

	// Inject uptime shorter than the gap (simulates reboot)
	orig := GetSystemUptime
	GetSystemUptime = func() (time.Duration, error) {
		return 10 * time.Minute, nil
	}
	defer func() { GetSystemUptime = orig }()

	result := DiagnoseRestart(st, startTime, tmpDir)

	if result.Class != ClassReboot {
		t.Errorf("expected ClassReboot, got %s (summary: %s)", result.Class, result.Summary)
	}
}

func TestDiagnoseRestart_RebootNoRecord(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")
	st := state.New(statePath)
	if err := st.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	// No prior shutdown record + very short uptime → reboot
	orig := GetSystemUptime
	GetSystemUptime = func() (time.Duration, error) {
		return 2 * time.Minute, nil
	}
	defer func() { GetSystemUptime = orig }()

	result := DiagnoseRestart(st, time.Now(), tmpDir)

	if result.Class != ClassReboot {
		t.Errorf("expected ClassReboot, got %s (summary: %s)", result.Class, result.Summary)
	}
}

func TestRecordCleanShutdown(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")
	st := state.New(statePath)
	if err := st.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	before := time.Now().Truncate(time.Second)
	if err := RecordCleanShutdown(st); err != nil {
		t.Fatalf("record clean shutdown: %v", err)
	}
	after := time.Now().Truncate(time.Second).Add(time.Second)

	var shutdownUnix int64
	if !st.Get(StateKeyLastCleanShutdown, &shutdownUnix) {
		t.Fatal("shutdown timestamp not set")
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
