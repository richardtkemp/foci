package resources

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeProcFile writes a file at procDir/relPath, creating parent dirs.
func writeProcFile(t *testing.T, procDir, relPath, content string) {
	t.Helper()
	path := filepath.Join(procDir, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// procStatus renders a fake /proc/[pid]/status body. rssKB < 0 omits the
// VmRSS line (as for kernel threads).
func procStatus(name, uid string, rssKB int64) string {
	s := fmt.Sprintf("Name:\t%s\nState:\tS (sleeping)\nUid:\t%s\t%s\t%s\t%s\n", name, uid, uid, uid, uid)
	if rssKB >= 0 {
		s += fmt.Sprintf("VmRSS:\t%d kB\n", rssKB)
	}
	return s
}

func newTestGuard(warnPct, killPct int, pressureThreshold float64) (*MemoryGuard, *mockState) {
	ms := &mockState{
		memTotalKB:    16 * 1024 * 1024, // 16GB
		pressureAvg10: 0,
	}
	g := &MemoryGuard{
		cfg: MemoryGuardConfig{
			Interval:          time.Second,
			WarnPercent:       warnPct,
			KillPercent:       killPct,
			PressureThreshold: pressureThreshold,
		},
		warnFn: func(msg string) {
			ms.mu.Lock()
			ms.warnings = append(ms.warnings, msg)
			ms.mu.Unlock()
		},
		uid: 1000,
		getMemTotalFn: func() (int64, error) {
			ms.mu.Lock()
			defer ms.mu.Unlock()
			return ms.memTotalKB, nil
		},
		getUserRSSFn: func(uid int) (int64, error) {
			ms.mu.Lock()
			defer ms.mu.Unlock()
			return ms.userRSSKB, nil
		},
		getPressureFn: func() (float64, error) {
			ms.mu.Lock()
			defer ms.mu.Unlock()
			return ms.pressureAvg10, nil
		},
		findLargestFn: func(uid, selfPid int) (int, string, int64, error) {
			ms.mu.Lock()
			defer ms.mu.Unlock()
			if ms.largestPid == 0 {
				return 0, "", 0, fmt.Errorf("no process found")
			}
			return ms.largestPid, ms.largestComm, ms.largestRSS, nil
		},
		killProcessFn: func(pid int) error {
			ms.mu.Lock()
			ms.killedPids = append(ms.killedPids, pid)
			ms.mu.Unlock()
			return nil
		},
	}
	return g, ms
}

type mockState struct {
	mu            sync.Mutex
	memTotalKB    int64
	userRSSKB     int64
	pressureAvg10 float64
	largestPid    int
	largestComm   string
	largestRSS    int64
	warnings      []string
	killedPids    []int
}

func TestMemoryGuard_BelowThreshold_NoAction(t *testing.T) {
	// Proves that no warning or kill fires when memory usage is well below the warn threshold,
	// by setting RSS to 10% of total and confirming no warnings are emitted.
	g, ms := newTestGuard(25, 40, 10.0)

	// 10% usage — well below 25% warn
	ms.userRSSKB = ms.memTotalKB / 10

	g.checkOnce()

	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.warnings) != 0 {
		t.Errorf("expected no warnings, got %d: %v", len(ms.warnings), ms.warnings)
	}
}

func TestMemoryGuard_AboveWarn_NoPressure_NoAction(t *testing.T) {
	// Proves that memory pressure is a required co-condition: even when RSS exceeds the warn
	// threshold, no warning fires if memory pressure is below its own threshold.
	g, ms := newTestGuard(25, 40, 10.0)

	// 30% usage — above warn, but no pressure
	ms.userRSSKB = ms.memTotalKB * 30 / 100
	ms.pressureAvg10 = 0.5 // well below threshold of 10.0

	g.checkOnce()

	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.warnings) != 0 {
		t.Errorf("expected no warnings (no pressure), got %d: %v", len(ms.warnings), ms.warnings)
	}
}

func TestMemoryGuard_AboveWarn_WithPressure_Warns(t *testing.T) {
	// Proves that a WARNING is emitted when both RSS exceeds the warn threshold and memory
	// pressure exceeds its threshold simultaneously.
	g, ms := newTestGuard(25, 40, 10.0)

	// 30% usage + pressure
	ms.userRSSKB = ms.memTotalKB * 30 / 100
	ms.pressureAvg10 = 15.0

	g.checkOnce()

	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(ms.warnings))
	}
	if !strings.Contains(ms.warnings[0], "WARNING") {
		t.Errorf("warning should contain WARNING: %q", ms.warnings[0])
	}
}

func TestMemoryGuard_WarnDedup(t *testing.T) {
	// Proves that repeated checks under the same high-memory condition only produce one warning,
	// preventing log spam by deduplicating the warning until conditions recover.
	g, ms := newTestGuard(25, 40, 10.0)

	ms.userRSSKB = ms.memTotalKB * 30 / 100
	ms.pressureAvg10 = 15.0

	g.checkOnce()
	g.checkOnce() // should not warn again

	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.warnings) != 1 {
		t.Errorf("expected 1 warning (dedup), got %d", len(ms.warnings))
	}
}

func TestMemoryGuard_WarnResetsOnRecovery(t *testing.T) {
	// Proves that the dedup latch resets once memory drops below the threshold, allowing a new
	// warning to fire on the next spike — validated by triggering, recovering, then spiking again.
	g, ms := newTestGuard(25, 40, 10.0)

	// Trigger warn
	ms.userRSSKB = ms.memTotalKB * 30 / 100
	ms.pressureAvg10 = 15.0
	g.checkOnce()

	// Drop below threshold
	ms.mu.Lock()
	ms.userRSSKB = ms.memTotalKB * 10 / 100
	ms.mu.Unlock()
	g.checkOnce()

	// Trigger again — should warn again
	ms.mu.Lock()
	ms.userRSSKB = ms.memTotalKB * 30 / 100
	ms.mu.Unlock()
	g.checkOnce()

	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.warnings) != 2 {
		t.Errorf("expected 2 warnings (reset on recovery), got %d", len(ms.warnings))
	}
}

func TestMemoryGuard_AboveKill_WithPressure_Kills(t *testing.T) {
	// Proves that when RSS exceeds the kill threshold and pressure is high, the largest process
	// is killed and a KILL warning is emitted.
	g, ms := newTestGuard(25, 40, 10.0)

	// 50% usage + pressure + target process
	ms.userRSSKB = ms.memTotalKB * 50 / 100
	ms.pressureAvg10 = 20.0
	ms.largestPid = 12345
	ms.largestComm = "node"
	ms.largestRSS = ms.memTotalKB * 40 / 100

	g.checkOnce()

	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.killedPids) != 1 || ms.killedPids[0] != 12345 {
		t.Errorf("expected kill of pid 12345, got %v", ms.killedPids)
	}
	// Should also have a warning about the kill
	foundKill := false
	for _, w := range ms.warnings {
		if strings.Contains(w, "KILL") {
			foundKill = true
		}
	}
	if !foundKill {
		t.Errorf("expected KILL warning, got: %v", ms.warnings)
	}
}

func TestMemoryGuard_Kill_NoPressure_NoAction(t *testing.T) {
	// Proves that pressure is required even for kill-level memory: RSS above kill threshold alone
	// does not cause any kill or warning when pressure is low.
	g, ms := newTestGuard(25, 40, 10.0)

	// 50% usage but no pressure
	ms.userRSSKB = ms.memTotalKB * 50 / 100
	ms.pressureAvg10 = 2.0
	ms.largestPid = 12345
	ms.largestComm = "node"
	ms.largestRSS = ms.memTotalKB * 40 / 100

	g.checkOnce()

	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.killedPids) != 0 {
		t.Errorf("expected no kills (no pressure), got %v", ms.killedPids)
	}
	if len(ms.warnings) != 0 {
		t.Errorf("expected no warnings (no pressure), got %v", ms.warnings)
	}
}

func TestMemoryGuard_Kill_NoProcess_WarnsError(t *testing.T) {
	// Proves that when conditions call for a kill but no target process can be found, a CRITICAL
	// warning is emitted and no kill attempt is made.
	g, ms := newTestGuard(25, 40, 10.0)

	// 50% usage + pressure but no process to kill
	ms.userRSSKB = ms.memTotalKB * 50 / 100
	ms.pressureAvg10 = 20.0
	ms.largestPid = 0 // will return error

	g.checkOnce()

	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.killedPids) != 0 {
		t.Errorf("expected no kills, got %v", ms.killedPids)
	}
	// Should have CRITICAL warning about inability to find process
	foundCritical := false
	for _, w := range ms.warnings {
		if strings.Contains(w, "CRITICAL") {
			foundCritical = true
		}
	}
	if !foundCritical {
		t.Errorf("expected CRITICAL warning, got: %v", ms.warnings)
	}
}

func TestReadMemTotal(t *testing.T) {
	// Proves that readMemTotal extracts the MemTotal value in kB from a meminfo file, and
	// returns errors for a missing file, a missing MemTotal line, and a non-numeric value.
	tests := []struct {
		name    string
		meminfo string // empty means don't create the file
		want    int64
		wantErr bool
	}{
		{
			name:    "happy path",
			meminfo: "MemTotal:       16384000 kB\nMemFree:         1024000 kB\nMemAvailable:    8192000 kB\n",
			want:    16384000,
		},
		{
			name:    "MemTotal line missing",
			meminfo: "MemFree:         1024000 kB\n",
			wantErr: true,
		},
		{
			name:    "MemTotal line without value",
			meminfo: "MemTotal:\nMemFree:         1024000 kB\n",
			wantErr: true,
		},
		{
			name:    "non-numeric value",
			meminfo: "MemTotal:       lots kB\n",
			wantErr: true,
		},
		{
			name:    "meminfo file missing",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			procDir := t.TempDir()
			if tt.meminfo != "" {
				writeProcFile(t, procDir, "meminfo", tt.meminfo)
			}
			got, err := readMemTotal(procDir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("readMemTotal() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("readMemTotal() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestReadUserRSS(t *testing.T) {
	// Proves that readUserRSS sums VmRSS only across PID directories owned by the given UID,
	// skipping other users' processes, non-PID entries, plain files, and unreadable PIDs.
	procDir := t.TempDir()
	writeProcFile(t, procDir, "1234/status", procStatus("foci", "1000", 100))
	writeProcFile(t, procDir, "5678/status", procStatus("node", "1000", 200))
	writeProcFile(t, procDir, "9999/status", procStatus("rootd", "0", 5000))  // other UID — excluded
	writeProcFile(t, procDir, "sys/status", procStatus("fake", "1000", 7000)) // non-numeric dir — skipped
	writeProcFile(t, procDir, "meminfo", "MemTotal: 1 kB\n")                  // plain file — skipped
	if err := os.MkdirAll(filepath.Join(procDir, "42"), 0o755); err != nil {  // PID without status file — skipped
		t.Fatal(err)
	}

	got, err := readUserRSS(procDir, 1000)
	if err != nil {
		t.Fatalf("readUserRSS() error = %v", err)
	}
	if got != 300 {
		t.Errorf("readUserRSS() = %d, want 300", got)
	}
}

func TestReadUserRSS_MissingProcDir(t *testing.T) {
	// Proves that readUserRSS returns an error when the proc directory cannot be read.
	if _, err := readUserRSS(filepath.Join(t.TempDir(), "nonexistent"), 1000); err == nil {
		t.Error("expected error for missing proc dir")
	}
}

func TestReadStatusRSS(t *testing.T) {
	// Proves that readStatusRSS reports VmRSS and UID ownership from a status file: owned
	// processes report their RSS, other-UID processes report not-owned, kernel-thread-style
	// files without VmRSS report 0 RSS, and a missing file reports (0, false).
	procDir := t.TempDir()
	tests := []struct {
		name      string
		content   string // empty means don't create the file
		uidStr    string
		wantRSS   int64
		wantOwned bool
	}{
		{"owned with RSS", procStatus("node", "1000", 4096), "1000", 4096, true},
		{"other UID", procStatus("rootd", "0", 4096), "1000", 4096, false},
		{"owned without VmRSS", procStatus("kthread", "1000", -1), "1000", 0, true},
		{"missing file", "", "1000", 0, false},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(procDir, fmt.Sprintf("%d", i+1), "status")
			if tt.content != "" {
				writeProcFile(t, procDir, fmt.Sprintf("%d/status", i+1), tt.content)
			}
			rss, owned := readStatusRSS(path, tt.uidStr)
			if rss != tt.wantRSS || owned != tt.wantOwned {
				t.Errorf("readStatusRSS() = (%d, %v), want (%d, %v)", rss, owned, tt.wantRSS, tt.wantOwned)
			}
		})
	}
}

func TestReadStatusFull(t *testing.T) {
	// Proves that readStatusFull additionally extracts the process name (comm) alongside RSS
	// and ownership, and returns zero values for a missing file.
	procDir := t.TempDir()
	writeProcFile(t, procDir, "77/status", procStatus("chromium", "1000", 8192))

	rss, owned, comm := readStatusFull(filepath.Join(procDir, "77", "status"), "1000")
	if rss != 8192 || !owned || comm != "chromium" {
		t.Errorf("readStatusFull() = (%d, %v, %q), want (8192, true, \"chromium\")", rss, owned, comm)
	}

	rss, owned, comm = readStatusFull(filepath.Join(procDir, "missing", "status"), "1000")
	if rss != 0 || owned || comm != "" {
		t.Errorf("readStatusFull() on missing file = (%d, %v, %q), want zero values", rss, owned, comm)
	}
}

func TestReadMemoryPressure(t *testing.T) {
	// Proves that readMemoryPressure extracts the avg10 value from the "some" line of a PSI
	// file, and errors when the file, the "some" line, or the avg10 field is missing or malformed.
	tests := []struct {
		name    string
		psi     string // empty means don't create the file
		want    float64
		wantErr bool
	}{
		{
			name: "happy path",
			psi:  "some avg10=3.25 avg60=1.10 avg300=0.50 total=123456\nfull avg10=0.80 avg60=0.20 avg300=0.10 total=65432\n",
			want: 3.25,
		},
		{
			name:    "no some line",
			psi:     "full avg10=0.80 avg60=0.20 avg300=0.10 total=65432\n",
			wantErr: true,
		},
		{
			name:    "some line without avg10",
			psi:     "some avg60=1.10 avg300=0.50 total=123456\n",
			wantErr: true,
		},
		{
			name:    "malformed avg10 value",
			psi:     "some avg10=high avg60=1.10 avg300=0.50 total=123456\n",
			wantErr: true,
		},
		{
			name:    "pressure file missing",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			procDir := t.TempDir()
			if tt.psi != "" {
				writeProcFile(t, procDir, "pressure/memory", tt.psi)
			}
			got, err := readMemoryPressure(procDir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("readMemoryPressure() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("readMemoryPressure() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFindLargestProcess(t *testing.T) {
	// Proves that findLargestProcess returns the PID, comm and RSS of the largest process
	// owned by the UID, excluding the guard's own PID and other users' processes even when
	// those have larger RSS.
	procDir := t.TempDir()
	writeProcFile(t, procDir, "100/status", procStatus("foci", "1000", 900000)) // self — excluded
	writeProcFile(t, procDir, "200/status", procStatus("node", "1000", 300000))
	writeProcFile(t, procDir, "300/status", procStatus("chromium", "1000", 500000))
	writeProcFile(t, procDir, "400/status", procStatus("rootd", "0", 800000)) // other UID — excluded
	writeProcFile(t, procDir, "notapid/status", procStatus("fake", "1000", 999999))

	pid, comm, rss, err := findLargestProcess(procDir, 1000, 100)
	if err != nil {
		t.Fatalf("findLargestProcess() error = %v", err)
	}
	if pid != 300 || comm != "chromium" || rss != 500000 {
		t.Errorf("findLargestProcess() = (%d, %q, %d), want (300, \"chromium\", 500000)", pid, comm, rss)
	}
}

func TestFindLargestProcess_Errors(t *testing.T) {
	// Proves that findLargestProcess errors when no eligible process exists (only self and
	// other-UID processes present) and when the proc directory cannot be read.
	procDir := t.TempDir()
	writeProcFile(t, procDir, "100/status", procStatus("foci", "1000", 900000)) // self
	writeProcFile(t, procDir, "400/status", procStatus("rootd", "0", 800000))   // other UID

	if _, _, _, err := findLargestProcess(procDir, 1000, 100); err == nil {
		t.Error("expected error when no eligible process exists")
	}
	if _, _, _, err := findLargestProcess(filepath.Join(procDir, "nonexistent"), 1000, 100); err == nil {
		t.Error("expected error for missing proc dir")
	}
}

func TestKillProcess_NonexistentPid(t *testing.T) {
	// Proves that killProcess returns an error when the target PID does not exist, since the
	// initial SIGTERM cannot be delivered. Uses a PID above the kernel's pid_max ceiling.
	if err := killProcess(1 << 30); err == nil {
		t.Error("expected error killing nonexistent pid")
	}
}

func TestMemoryGuard_KillError_StillWarns(t *testing.T) {
	// Proves that when the kill attempt itself fails, the KILL warning has already been emitted
	// and the failure is absorbed without panic — the guard degrades to notification-only.
	g, ms := newTestGuard(25, 40, 10.0)
	ms.userRSSKB = ms.memTotalKB * 50 / 100
	ms.pressureAvg10 = 20.0
	ms.largestPid = 12345
	ms.largestComm = "node"
	ms.largestRSS = ms.memTotalKB * 40 / 100

	g.killProcessFn = func(pid int) error {
		return fmt.Errorf("permission denied")
	}

	g.checkOnce()

	ms.mu.Lock()
	defer ms.mu.Unlock()
	foundKill := false
	for _, w := range ms.warnings {
		if strings.Contains(w, "KILL") {
			foundKill = true
		}
	}
	if !foundKill {
		t.Errorf("expected KILL warning despite kill failure, got: %v", ms.warnings)
	}
}

func TestStartStop(t *testing.T) {
	// Proves that the guard starts a polling goroutine and shuts down cleanly when the context
	// is cancelled and Stop is called — no deadlock or panic.
	g, _ := newTestGuard(25, 40, 10.0)
	g.cfg.Interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	g.Start(ctx)

	time.Sleep(30 * time.Millisecond)

	cancel()
	g.Stop()
}

func TestNewMemoryGuard(t *testing.T) {
	// Proves that the constructor correctly stores the config and sets uid to the current user
	// (non-zero), confirming proper initialization of the guard.
	cfg := MemoryGuardConfig{
		Interval:          time.Second,
		WarnPercent:       25,
		KillPercent:       40,
		PressureThreshold: 10.0,
	}
	warnFn := func(msg string) {
		// no-op
	}

	g := NewMemoryGuard(cfg, warnFn)

	if g.cfg.WarnPercent != 25 {
		t.Errorf("WarnPercent = %d, want 25", g.cfg.WarnPercent)
	}
	if g.cfg.KillPercent != 40 {
		t.Errorf("KillPercent = %d, want 40", g.cfg.KillPercent)
	}
	if g.uid == 0 {
		t.Errorf("uid should be set to current user")
	}
}

func TestMemoryGuard_Start_ZeroInterval(t *testing.T) {
	// Proves that a zero interval disables polling entirely: Start returns immediately without
	// spawning a goroutine, leaving the done channel nil.
	cfg := MemoryGuardConfig{
		Interval: 0, // Disabled
	}
	g := NewMemoryGuard(cfg, func(msg string) {})

	ctx := context.Background()
	g.Start(ctx)

	if g.done != nil {
		t.Errorf("done channel should not be created with zero interval")
	}
}

func TestMemoryGuard_Stop_WithoutStart(t *testing.T) {
	// Proves that calling Stop on a guard that was never started does not panic.
	g := NewMemoryGuard(MemoryGuardConfig{}, func(msg string) {})
	g.Stop() // Should not panic
}

func TestMemoryGuard_GetMemTotal_Error(t *testing.T) {
	// Proves that a failure reading total memory causes checkOnce to abort silently without
	// panicking, since a missing total makes percentage calculations impossible.
	g := &MemoryGuard{
		cfg: MemoryGuardConfig{
			WarnPercent: 25,
			KillPercent: 40,
		},
		uid: 1000,
		getMemTotalFn: func() (int64, error) {
			return 0, fmt.Errorf("can't read /proc/meminfo")
		},
		getUserRSSFn: func(uid int) (int64, error) {
			return 0, nil
		},
		getPressureFn: func() (float64, error) {
			return 0, nil
		},
	}

	g.checkOnce() // Should not panic, just skip the check
}

func TestMemoryGuard_GetUserRSS_Error(t *testing.T) {
	// Proves that a failure reading the user's RSS causes checkOnce to abort silently without
	// panicking, since no usage figure means no meaningful threshold comparison.
	g := &MemoryGuard{
		cfg: MemoryGuardConfig{
			WarnPercent: 25,
			KillPercent: 40,
		},
		uid: 1000,
		getMemTotalFn: func() (int64, error) {
			return 16 * 1024 * 1024, nil
		},
		getUserRSSFn: func(uid int) (int64, error) {
			return 0, fmt.Errorf("can't read proc")
		},
		getPressureFn: func() (float64, error) {
			return 0, nil
		},
	}

	g.checkOnce() // Should not panic, just skip the check
}

func TestMemoryGuard_GetPressure_Error(t *testing.T) {
	// Proves that a failure reading memory pressure causes checkOnce to abort silently even when
	// RSS is above the warn threshold — no panic, no spurious warning.
	g, ms := newTestGuard(25, 40, 10.0)
	ms.userRSSKB = ms.memTotalKB * 30 / 100 // Above warn threshold

	g.getPressureFn = func() (float64, error) {
		return 0, fmt.Errorf("can't read pressure")
	}

	g.checkOnce() // Should not panic
}

func TestMemoryGuard_Multiple_Kills(t *testing.T) {
	// Proves that checkOnce performs at most one kill per invocation, even if RSS remains above
	// the kill threshold after the first kill — the caller must re-invoke for subsequent kills.
	g, ms := newTestGuard(25, 40, 10.0)
	ms.memTotalKB = 1000 // Small total to make ratios work

	// Set user RSS to 500 (50% of total)
	ms.userRSSKB = 500
	ms.pressureAvg10 = 20.0

	killCount := 0
	g.killProcessFn = func(pid int) error {
		killCount++
		// Simulate process death reducing RSS
		ms.mu.Lock()
		defer ms.mu.Unlock()
		ms.userRSSKB -= 200 // Reduce by amount of killed process
		ms.largestRSS -= 200
		return nil
	}

	// After first kill (300 RSS remaining, 30%), should still be above 25% warn but below 40% kill
	g.checkOnce()

	if killCount > 1 {
		t.Errorf("expected at most 1 kill per check, got %d", killCount)
	}
}
