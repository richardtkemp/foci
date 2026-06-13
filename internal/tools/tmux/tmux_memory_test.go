package tmux

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestParseThreshold(t *testing.T) {
	// Proves that threshold strings in %, mb, and gb formats are correctly converted to kB values,
	// and that invalid or zero/negative inputs are rejected.
	t.Parallel()
	memTotalKB := int64(16 * 1024 * 1024) // 16 GB in kB

	tests := []struct {
		input   string
		wantKB  int64
		wantErr bool
	}{
		// Percentage
		{"10%", memTotalKB * 10 / 100, false},
		{"20%", memTotalKB * 20 / 100, false},
		{"50%", memTotalKB / 2, false},
		{"100%", memTotalKB, false},
		{"1.5%", int64(float64(memTotalKB) * 1.5 / 100), false},

		// Megabytes
		{"512mb", 512 * 1024, false},
		{"1024mb", 1024 * 1024, false},
		{"0.5mb", 512, false},

		// Gigabytes
		{"2gb", 2 * 1024 * 1024, false},
		{"0.5gb", 512 * 1024, false},
		{"16gb", 16 * 1024 * 1024, false},

		// Case insensitivity
		{"10%", memTotalKB * 10 / 100, false},
		{"512MB", 512 * 1024, false},
		{"2GB", 2 * 1024 * 1024, false},

		// Errors
		{"", 0, true},
		{"abc", 0, true},
		{"10", 0, true},
		{"%", 0, true},
		{"0%", 0, true},
		{"-5%", 0, true},
		{"101%", 0, true},
		{"0mb", 0, true},
		{"-1gb", 0, true},
		{"mb", 0, true},
		{"gb", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseThreshold(tt.input, memTotalKB)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseThreshold(%q) = %d, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Errorf("ParseThreshold(%q) error: %v", tt.input, err)
				return
			}
			if got != tt.wantKB {
				t.Errorf("ParseThreshold(%q) = %d kB, want %d kB", tt.input, got, tt.wantKB)
			}
		})
	}
}

func TestCheckOnce_WarnThreshold(t *testing.T) {
	// Proves that RSS exceeding the warn threshold (but not critical) produces exactly one WARNING notification.
	t.Parallel()
	var notifications []string
	var mu sync.Mutex

	m := NewTmuxMemoryMonitor(
		TmuxMemoryConfig{
			WarnStr:     "10%",
			CriticalStr: "20%",
			KillStr:     "30%",
		},
		func(msg string) {
			mu.Lock()
			notifications = append(notifications, msg)
			mu.Unlock()
		},
		func() { t.Error("cleanup should not be called for warn") },
	)

	memTotalKB := int64(16 * 1024 * 1024) // 16GB
	// Set RSS to 12% of RAM (exceeds 10% warn but not 20% critical)
	m.getTmuxRSSFn = func() (int64, error) { return memTotalKB * 12 / 100, nil }
	m.getMemTotalFn = func() (int64, error) { return memTotalKB, nil }

	m.checkOnce()

	mu.Lock()
	defer mu.Unlock()
	if len(notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d: %v", len(notifications), notifications)
	}
	if !strings.Contains(notifications[0], "WARNING") {
		t.Errorf("expected WARNING notification, got: %s", notifications[0])
	}
}

func TestCheckOnce_CriticalThreshold(t *testing.T) {
	// Proves that RSS exceeding the critical threshold (but not kill) produces exactly one CRITICAL notification.
	t.Parallel()
	var notifications []string
	var mu sync.Mutex

	m := NewTmuxMemoryMonitor(
		TmuxMemoryConfig{
			WarnStr:     "10%",
			CriticalStr: "20%",
			KillStr:     "30%",
		},
		func(msg string) {
			mu.Lock()
			notifications = append(notifications, msg)
			mu.Unlock()
		},
		func() { t.Error("cleanup should not be called for critical") },
	)

	memTotalKB := int64(16 * 1024 * 1024)
	// Set RSS to 25% of RAM (exceeds critical but not kill)
	m.getTmuxRSSFn = func() (int64, error) { return memTotalKB * 25 / 100, nil }
	m.getMemTotalFn = func() (int64, error) { return memTotalKB, nil }

	m.checkOnce()

	mu.Lock()
	defer mu.Unlock()
	if len(notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d: %v", len(notifications), notifications)
	}
	if !strings.Contains(notifications[0], "CRITICAL") {
		t.Errorf("expected CRITICAL notification, got: %s", notifications[0])
	}
}

func TestCheckOnce_KillThreshold(t *testing.T) {
	// Proves that RSS exceeding the kill threshold fires both CRITICAL and KILL notifications,
	// invokes the kill function, and calls the cleanup callback.
	t.Parallel()
	var notifications []string
	cleanupCalled := false
	var mu sync.Mutex

	m := NewTmuxMemoryMonitor(
		TmuxMemoryConfig{
			WarnStr:     "10%",
			CriticalStr: "20%",
			KillStr:     "30%",
		},
		func(msg string) {
			mu.Lock()
			notifications = append(notifications, msg)
			mu.Unlock()
		},
		func() { cleanupCalled = true },
	)

	memTotalKB := int64(16 * 1024 * 1024)
	// Set RSS to 35% of RAM (exceeds kill threshold)
	m.getTmuxRSSFn = func() (int64, error) { return memTotalKB * 35 / 100, nil }
	m.getMemTotalFn = func() (int64, error) { return memTotalKB, nil }
	m.killTmuxFn = func() ([]string, error) { return []string{"foci-1", "foci-2"}, nil }

	m.checkOnce()

	mu.Lock()
	defer mu.Unlock()
	if len(notifications) != 2 {
		t.Fatalf("expected 2 notifications (critical + kill), got %d: %v", len(notifications), notifications)
	}
	if !strings.Contains(notifications[0], "CRITICAL") {
		t.Errorf("expected CRITICAL notification first, got: %s", notifications[0])
	}
	if !strings.Contains(notifications[1], "KILL") {
		t.Errorf("expected KILL notification, got: %s", notifications[1])
	}
	if !strings.Contains(notifications[1], "foci-1") || !strings.Contains(notifications[1], "foci-2") {
		t.Errorf("kill notification should list sessions: %s", notifications[1])
	}
	if !cleanupCalled {
		t.Error("cleanup callback should have been called")
	}
}

func TestDedup_SameThresholdDoesNotReNotify(t *testing.T) {
	// Proves that repeated checks at the same memory level do not re-fire notifications —
	// only the first crossing of a threshold triggers an alert.
	t.Parallel()
	var notifications []string
	var mu sync.Mutex

	m := NewTmuxMemoryMonitor(
		TmuxMemoryConfig{
			WarnStr:     "10%",
			CriticalStr: "20%",
			KillStr:     "30%",
		},
		func(msg string) {
			mu.Lock()
			notifications = append(notifications, msg)
			mu.Unlock()
		},
		nil,
	)

	memTotalKB := int64(16 * 1024 * 1024)
	m.getTmuxRSSFn = func() (int64, error) { return memTotalKB * 15 / 100, nil }
	m.getMemTotalFn = func() (int64, error) { return memTotalKB, nil }

	// First check: warn fires
	m.checkOnce()
	// Second check at same level: should NOT fire again
	m.checkOnce()
	// Third check: still above warn
	m.checkOnce()

	mu.Lock()
	defer mu.Unlock()
	if len(notifications) != 1 {
		t.Errorf("expected 1 notification (dedup), got %d: %v", len(notifications), notifications)
	}
}

func TestDedup_EscalatesToCritical(t *testing.T) {
	// Proves that when memory grows from warn to critical level, a second notification is sent
	// for the escalation, resulting in two total notifications.
	t.Parallel()
	var notifications []string
	var mu sync.Mutex

	m := NewTmuxMemoryMonitor(
		TmuxMemoryConfig{
			WarnStr:     "10%",
			CriticalStr: "20%",
			KillStr:     "30%",
		},
		func(msg string) {
			mu.Lock()
			notifications = append(notifications, msg)
			mu.Unlock()
		},
		nil,
	)

	memTotalKB := int64(16 * 1024 * 1024)
	m.getMemTotalFn = func() (int64, error) { return memTotalKB, nil }

	// First check at 15% (warn level)
	m.getTmuxRSSFn = func() (int64, error) { return memTotalKB * 15 / 100, nil }
	m.checkOnce()

	// Memory grows to 25% (critical level)
	m.getTmuxRSSFn = func() (int64, error) { return memTotalKB * 25 / 100, nil }
	m.checkOnce()

	mu.Lock()
	defer mu.Unlock()
	if len(notifications) != 2 {
		t.Fatalf("expected 2 notifications (warn + critical), got %d: %v", len(notifications), notifications)
	}
	if !strings.Contains(notifications[0], "WARNING") {
		t.Errorf("first notification should be WARNING: %s", notifications[0])
	}
	if !strings.Contains(notifications[1], "CRITICAL") {
		t.Errorf("second notification should be CRITICAL: %s", notifications[1])
	}
}

func TestDedup_DropBelowResetsState(t *testing.T) {
	// Proves that when memory drops below all thresholds and then rises again,
	// the warn notification fires a second time (dedup state is reset on recovery).
	t.Parallel()
	var notifications []string
	var mu sync.Mutex

	m := NewTmuxMemoryMonitor(
		TmuxMemoryConfig{
			WarnStr:     "10%",
			CriticalStr: "20%",
			KillStr:     "30%",
		},
		func(msg string) {
			mu.Lock()
			notifications = append(notifications, msg)
			mu.Unlock()
		},
		nil,
	)

	memTotalKB := int64(16 * 1024 * 1024)
	m.getMemTotalFn = func() (int64, error) { return memTotalKB, nil }

	// First: above warn
	m.getTmuxRSSFn = func() (int64, error) { return memTotalKB * 15 / 100, nil }
	m.checkOnce()

	// Drop below all thresholds
	m.getTmuxRSSFn = func() (int64, error) { return memTotalKB * 5 / 100, nil }
	m.checkOnce()

	// Rise above warn again — should fire again
	m.getTmuxRSSFn = func() (int64, error) { return memTotalKB * 15 / 100, nil }
	m.checkOnce()

	mu.Lock()
	defer mu.Unlock()
	if len(notifications) != 2 {
		t.Errorf("expected 2 notifications (warn, then warn again after reset), got %d: %v", len(notifications), notifications)
	}
}

func TestKillCleansUp(t *testing.T) {
	// Proves that after a kill event, dedup state is reset so subsequent warn-level crossings
	// fire new notifications, and that both the kill and cleanup callbacks are invoked.
	t.Parallel()
	cleanupCalled := false
	killCalled := false
	var notifications []string
	var mu sync.Mutex

	m := NewTmuxMemoryMonitor(
		TmuxMemoryConfig{
			WarnStr:     "10%",
			CriticalStr: "20%",
			KillStr:     "30%",
		},
		func(msg string) {
			mu.Lock()
			notifications = append(notifications, msg)
			mu.Unlock()
		},
		func() { cleanupCalled = true },
	)

	memTotalKB := int64(16 * 1024 * 1024)
	m.getTmuxRSSFn = func() (int64, error) { return memTotalKB * 35 / 100, nil }
	m.getMemTotalFn = func() (int64, error) { return memTotalKB, nil }
	m.killTmuxFn = func() ([]string, error) {
		killCalled = true
		return []string{"test-session"}, nil
	}

	m.checkOnce()

	if !killCalled {
		t.Error("expected tmux kill to be called")
	}
	if !cleanupCalled {
		t.Error("expected cleanup callback to be called")
	}

	// After kill, dedup state should be reset. Next check at warn level should fire.
	m.getTmuxRSSFn = func() (int64, error) { return memTotalKB * 15 / 100, nil }
	m.killTmuxFn = func() ([]string, error) {
		t.Error("kill should not be called at warn level")
		return nil, nil
	}

	mu.Lock()
	notifications = nil
	mu.Unlock()

	m.checkOnce()

	mu.Lock()
	defer mu.Unlock()
	if len(notifications) != 1 {
		t.Errorf("expected 1 notification after kill reset, got %d", len(notifications))
	}
}

func TestCheckOnce_TmuxNotRunning(t *testing.T) {
	// Proves that an error from the RSS query (e.g. tmux not running) is handled silently —
	// no notification is sent.
	t.Parallel()
	notifyCalled := false

	m := NewTmuxMemoryMonitor(
		TmuxMemoryConfig{
			WarnStr:     "10%",
			CriticalStr: "20%",
			KillStr:     "30%",
		},
		func(msg string) { notifyCalled = true },
		nil,
	)

	m.getTmuxRSSFn = func() (int64, error) { return 0, fmt.Errorf("no tmux server running") }
	m.getMemTotalFn = func() (int64, error) { return 16 * 1024 * 1024, nil }

	m.checkOnce()

	if notifyCalled {
		t.Error("should not notify when tmux is not running")
	}
}

func TestCheckOnce_KillFails(t *testing.T) {
	// Proves that a failed kill operation still sends a FAILED notification and calls the cleanup
	// callback, so the agent is informed even when the kill could not complete.
	t.Parallel()
	var notifications []string
	cleanupCalled := false
	var mu sync.Mutex

	m := NewTmuxMemoryMonitor(
		TmuxMemoryConfig{
			WarnStr:     "10%",
			CriticalStr: "20%",
			KillStr:     "30%",
		},
		func(msg string) {
			mu.Lock()
			notifications = append(notifications, msg)
			mu.Unlock()
		},
		func() { cleanupCalled = true },
	)

	memTotalKB := int64(16 * 1024 * 1024)
	m.getTmuxRSSFn = func() (int64, error) { return memTotalKB * 35 / 100, nil }
	m.getMemTotalFn = func() (int64, error) { return memTotalKB, nil }
	m.killTmuxFn = func() ([]string, error) {
		return nil, fmt.Errorf("permission denied")
	}

	m.checkOnce()

	mu.Lock()
	defer mu.Unlock()
	// Should still send notifications even if kill fails
	hasKillFailed := false
	for _, n := range notifications {
		if strings.Contains(n, "FAILED") {
			hasKillFailed = true
		}
	}
	if !hasKillFailed {
		t.Error("expected a notification about kill failure")
	}
	if !cleanupCalled {
		t.Error("cleanup should still be called even if kill fails")
	}
}

func TestDedup_CriticalDropsToWarn(t *testing.T) {
	// Proves that when memory drops from critical to warn level and then rises back to critical,
	// the critical notification fires a second time (partial reset on descent).
	t.Parallel()
	var notifications []string
	var mu sync.Mutex

	m := NewTmuxMemoryMonitor(
		TmuxMemoryConfig{
			WarnStr:     "10%",
			CriticalStr: "20%",
			KillStr:     "30%",
		},
		func(msg string) {
			mu.Lock()
			notifications = append(notifications, msg)
			mu.Unlock()
		},
		nil,
	)

	memTotalKB := int64(16 * 1024 * 1024)
	m.getMemTotalFn = func() (int64, error) { return memTotalKB, nil }

	// Jump straight to critical (25%)
	m.getTmuxRSSFn = func() (int64, error) { return memTotalKB * 25 / 100, nil }
	m.checkOnce()

	// Drop to warn level (15%) — critical should reset
	m.getTmuxRSSFn = func() (int64, error) { return memTotalKB * 15 / 100, nil }
	m.checkOnce()

	// Rise back to critical — should fire again
	m.getTmuxRSSFn = func() (int64, error) { return memTotalKB * 25 / 100, nil }
	m.checkOnce()

	mu.Lock()
	defer mu.Unlock()
	if len(notifications) != 2 {
		t.Errorf("expected 2 critical notifications (first + re-fire after drop), got %d: %v", len(notifications), notifications)
	}
}
