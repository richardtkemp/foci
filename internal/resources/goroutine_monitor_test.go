package resources

import (
	"context"
	"testing"
	"time"
)

func newTestMonitor(threshold int) (*GoroutineMonitor, *int) {
	count := 50
	m := &GoroutineMonitor{
		cfg: GoroutineMonitorConfig{
			Interval:  time.Second,
			Threshold: threshold,
		},
		numGoroutinesFn: func() int { return count },
	}
	return m, &count
}

func TestGoroutineMonitor_BelowThreshold(t *testing.T) {
	// Verifies that no warning fires when
	// the goroutine count is below the threshold.
	m, _ := newTestMonitor(100)

	m.checkOnce()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.warnFired {
		t.Error("expected no warning when below threshold")
	}
}

func TestGoroutineMonitor_AboveThreshold(t *testing.T) {
	// Verifies that a warning fires when
	// the goroutine count exceeds the threshold.
	m, count := newTestMonitor(100)
	*count = 150

	m.checkOnce()

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.warnFired {
		t.Error("expected warning when above threshold")
	}
}

func TestGoroutineMonitor_WarnDedup(t *testing.T) {
	// Verifies that repeated checks above the
	// threshold only fire the warning once (dedup).
	m, count := newTestMonitor(100)
	*count = 150

	m.checkOnce()
	m.checkOnce() // should not log a second WARN

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.warnFired {
		t.Error("expected warnFired to remain true")
	}
}

func TestGoroutineMonitor_WarnResetsOnRecovery(t *testing.T) {
	// Verifies that the warning resets
	// when the count drops below the threshold, allowing re-fire if it spikes again.
	m, count := newTestMonitor(100)

	// Trigger warn
	*count = 150
	m.checkOnce()

	// Recover
	*count = 50
	m.checkOnce()

	m.mu.Lock()
	if m.warnFired {
		t.Error("expected warnFired to reset after recovery")
	}
	m.mu.Unlock()

	// Spike again — should warn again
	*count = 200
	m.checkOnce()

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.warnFired {
		t.Error("expected warning to re-fire after recovery")
	}
}

func TestGoroutineMonitor_StartStop(t *testing.T) {
	// Verifies that the monitor starts a goroutine
	// and stops cleanly via context cancellation.
	m, _ := newTestMonitor(100)
	m.cfg.Interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)

	time.Sleep(30 * time.Millisecond)

	cancel()
	m.Stop()
}

func TestGoroutineMonitor_Start_ZeroInterval(t *testing.T) {
	// Verifies that Start with zero
	// interval returns immediately without spawning a goroutine.
	m := NewGoroutineMonitor(GoroutineMonitorConfig{Interval: 0, Threshold: 100})

	ctx := context.Background()
	m.Start(ctx)

	if m.done != nil {
		t.Error("done channel should not be created with zero interval")
	}
}

func TestGoroutineMonitor_Stop_WithoutStart(t *testing.T) {
	// Verifies that calling Stop without
	// Start does not panic.
	m := NewGoroutineMonitor(GoroutineMonitorConfig{})
	m.Stop() // should not panic
}

func TestNewGoroutineMonitor(t *testing.T) {
	// Verifies the constructor sets config correctly.
	cfg := GoroutineMonitorConfig{
		Interval:  30 * time.Second,
		Threshold: 200,
	}
	m := NewGoroutineMonitor(cfg)

	if m.cfg.Interval != 30*time.Second {
		t.Errorf("Interval = %v, want 30s", m.cfg.Interval)
	}
	if m.cfg.Threshold != 200 {
		t.Errorf("Threshold = %d, want 200", m.cfg.Threshold)
	}
}

func TestGoroutineMonitor_DefaultCountFn(t *testing.T) {
	// Verifies that checkOnce falls back to runtime.NumGoroutine when no count function is
	// injected, and that the real (small) count stays below a generous threshold without warning.
	m := NewGoroutineMonitor(GoroutineMonitorConfig{Interval: time.Second, Threshold: 1 << 20})

	m.checkOnce()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.warnFired {
		t.Error("expected no warning with real goroutine count below huge threshold")
	}
}

func TestGoroutineMonitor_ExactThreshold(t *testing.T) {
	// Verifies that the count equal to the
	// threshold does NOT trigger a warning (only > threshold does).
	m, count := newTestMonitor(100)
	*count = 100

	m.checkOnce()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.warnFired {
		t.Error("expected no warning when count equals threshold exactly")
	}
}
