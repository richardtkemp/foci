package resources

import (
	"context"
	"runtime"
	"sync"
	"time"

	"foci/internal/log"
)

var (
	goroutine_monitorLog = log.NewComponentLogger("goroutine_monitor")
	memory_guardLog      = log.NewComponentLogger(

		// GoroutineMonitorConfig holds parsed configuration for the goroutine monitor.
		"memory_guard")
	resourcesLog = log.NewComponentLogger("resources")
)

type GoroutineMonitorConfig struct {
	Interval  time.Duration
	Threshold int // static base: warn when goroutine count exceeds this

	// DynamicExtra, if set, is added to Threshold on every check. It lets the
	// budget track quantities unknown at startup — e.g. live app sockets, each
	// of which costs a couple of goroutines that come and go with the phone.
	// Returns 0 when there is nothing dynamic to account for.
	DynamicExtra func() int
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
		resourcesLog.Warnf("goroutine monitor not started: interval=%v (<=0, disabled)", m.cfg.Interval)
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
	goroutine_monitorLog.Infof("started (interval=%v, threshold=%d)", m.cfg.Interval, m.cfg.Threshold)
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
	goroutine_monitorLog.Debugf("goroutines=%d", count)

	// Effective threshold = static base + any dynamic headroom (live app
	// sockets etc.), recomputed each tick so the budget tracks reality.
	threshold := m.cfg.Threshold
	if m.cfg.DynamicExtra != nil {
		threshold += m.cfg.DynamicExtra()
	}

	if count > threshold {
		m.mu.Lock()
		alreadyWarned := m.warnFired
		m.warnFired = true
		m.mu.Unlock()

		if !alreadyWarned {
			goroutine_monitorLog.Warnf("goroutine count %d exceeds threshold %d — possible leak", count, threshold)
		}
		return
	}

	// Back below threshold — reset dedup
	m.mu.Lock()
	if m.warnFired {
		goroutine_monitorLog.Infof("goroutine count %d back below threshold %d", count, threshold)
	}
	m.warnFired = false
	m.mu.Unlock()
}
