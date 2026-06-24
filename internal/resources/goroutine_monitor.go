package resources

import (
	"context"
	"runtime"
	"sync"
	"time"

	"foci/internal/log"
)

// GoroutineMonitorConfig holds parsed configuration for the goroutine monitor.
type GoroutineMonitorConfig struct {
	Interval  time.Duration
	Threshold int // warn when goroutine count exceeds this
}

// GoroutineMonitor periodically logs the current goroutine count and warns
// if it exceeds a configurable threshold — a simple leak indicator for
// long-running processes.
type GoroutineMonitor struct {
	cfg GoroutineMonitorConfig

	mu        sync.Mutex
	warnFired bool

	cancel context.CancelFunc
	done   chan struct{}

	// Overridable for testing.
	numGoroutinesFn func() int
}

// NewGoroutineMonitor creates a goroutine monitor.
func NewGoroutineMonitor(cfg GoroutineMonitorConfig) *GoroutineMonitor {
	return &GoroutineMonitor{cfg: cfg}
}

// Start launches the background monitoring goroutine.
func (m *GoroutineMonitor) Start(ctx context.Context) {
	if m.cfg.Interval <= 0 {
		log.Warnf("resources", "goroutine monitor not started: interval=%v (<=0, disabled)", m.cfg.Interval)
		return
	}
	monCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.done = make(chan struct{})

	go func() {
		defer close(m.done)
		ticker := time.NewTicker(m.cfg.Interval)
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
	log.Infof("goroutine_monitor", "started (interval=%v, threshold=%d)", m.cfg.Interval, m.cfg.Threshold)
}

// Stop stops the monitor and waits for the goroutine to exit.
func (m *GoroutineMonitor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	if m.done != nil {
		<-m.done
	}
}

// checkOnce runs a single check cycle.
func (m *GoroutineMonitor) checkOnce() {
	getCount := m.numGoroutinesFn
	if getCount == nil {
		getCount = runtime.NumGoroutine
	}

	count := getCount()
	log.Debugf("goroutine_monitor", "goroutines=%d", count)

	if count > m.cfg.Threshold {
		m.mu.Lock()
		alreadyWarned := m.warnFired
		m.warnFired = true
		m.mu.Unlock()

		if !alreadyWarned {
			log.Warnf("goroutine_monitor", "goroutine count %d exceeds threshold %d — possible leak", count, m.cfg.Threshold)
		}
		return
	}

	// Back below threshold — reset dedup
	m.mu.Lock()
	if m.warnFired {
		log.Infof("goroutine_monitor", "goroutine count %d back below threshold %d", count, m.cfg.Threshold)
	}
	m.warnFired = false
	m.mu.Unlock()
}
