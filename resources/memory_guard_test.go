package resources

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestGuard(warnPct, killPct int, pressureThreshold float64) (*MemoryGuard, *mockState) {
	ms := &mockState{
		memTotalKB: 16 * 1024 * 1024, // 16GB
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

func TestReadMemoryPressure_ParseFormat(t *testing.T) {
	// Test the parsing logic by calling the real function if /proc/pressure/memory exists
	_, err := readMemoryPressure()
	if err != nil {
		t.Skipf("skipping on systems without /proc/pressure/memory: %v", err)
	}
}

func TestReadStatusRSS(t *testing.T) {
	// Smoke test — just ensure the function handles missing files gracefully
	rss, owned := readStatusRSS("/proc/nonexistent/status", "1000")
	if owned {
		t.Error("readStatusRSS should return false for nonexistent path")
	}
	if rss != 0 {
		t.Errorf("readStatusRSS should return 0 for nonexistent path, got %d", rss)
	}
}

func TestReadStatusFull(t *testing.T) {
	rss, owned, comm := readStatusFull("/proc/nonexistent/status", "1000")
	if owned {
		t.Error("readStatusFull should return false for nonexistent path")
	}
	if rss != 0 || comm != "" {
		t.Errorf("readStatusFull should return zero values for nonexistent path")
	}
}

func TestStartStop(t *testing.T) {
	g, _ := newTestGuard(25, 40, 10.0)
	g.cfg.Interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	g.Start(ctx)

	time.Sleep(30 * time.Millisecond)

	cancel()
	g.Stop()
}
