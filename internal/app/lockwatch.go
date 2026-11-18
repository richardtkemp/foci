package app

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"time"
)

// Hub lock watchdog (#3171).
//
// On 18 Nov 2026 the work Mac's app hub stopped granting its lock: one
// goroutine held the read lock while stuck, a writer queued behind it, and
// every later reader and new socket blocked. The process stayed alive, so
// nothing reported the hang; the only way in was to enable pprof by hand.
//
// The watchdog probes the hub's lock every hubLockProbeInterval (10s). A probe
// takes and releases the READ lock in its own goroutine: a read lock blocks
// as soon as a writer waits, which is the cascade that kills the app. When a
// probe is not granted within hubLockStuckAfter (20s), the watchdog logs one WARN
// and writes a full goroutine dump to a file in the logs directory (the WARN
// names the file). It warns once per hang: while the probe stays blocked, no
// new probe starts (so at most one probe goroutine waits) and nothing more
// is logged. When the probe is granted, it logs one INFO with the hang's
// length. The log line and the dump file are the outputs that do not depend
// on the app: a WARN that is injected into a chat may not arrive.

const (
	// hubLockProbeInterval is how often the hub's lock is probed. With
	// hubLockStuckAfter it bounds the time from a hang's start to its WARN
	// at about 30s.
	hubLockProbeInterval = 10 * time.Second
	// hubLockStuckAfter is how long a probe may wait before it is a hang.
	hubLockStuckAfter = 20 * time.Second
)

// lockWatchdog watches one lock. acquire takes and releases it.
type lockWatchdog struct {
	name       string // what the lock is, for the log lines
	acquire    func()
	interval   time.Duration
	stuckAfter time.Duration
	dumpDir    string

	// pending is closed when the outstanding probe is granted; nil = no
	// probe outstanding. since is when it started. Owned by the run loop.
	pending chan struct{}
	since   time.Time
	stuck   bool // the WARN for the outstanding probe was logged
}

// run probes until done is closed.
func (w *lockWatchdog) run(done <-chan struct{}) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}
		w.probe(done)
	}
}

// probe starts a probe (unless one is outstanding) and waits up to the
// threshold, measured from the probe's start, for it to be granted.
func (w *lockWatchdog) probe(done <-chan struct{}) {
	if w.pending == nil {
		granted := make(chan struct{})
		w.pending, w.since = granted, time.Now()
		go func() {
			w.acquire()
			close(granted)
		}()
	}
	wait := w.stuckAfter - time.Since(w.since)
	if w.stuck {
		// Already reported: look for the recovery without waiting long, so
		// the loop keeps its cadence.
		wait = 0
	}
	timer := time.NewTimer(max(wait, 0))
	defer timer.Stop()
	select {
	case <-w.pending:
		if w.stuck {
			appLog.Infof("%s granted again after %s: the hang is over", w.name, time.Since(w.since).Round(time.Second))
		}
		w.pending, w.stuck = nil, false
	case <-timer.C:
		if !w.stuck {
			w.stuck = true
			w.reportStuck()
		}
	case <-done:
	}
}

// reportStuck writes the goroutine dump and logs the WARN.
func (w *lockWatchdog) reportStuck() {
	waited := time.Since(w.since).Round(time.Second)
	path, err := w.writeDump()
	if err != nil {
		appLog.Warnf("%s not granted for %s: the app is hung; goroutine dump failed: %v", w.name, waited, err)
		return
	}
	appLog.Warnf("%s not granted for %s: the app is hung; goroutine dump written to %s", w.name, waited, path)
}

// writeDump writes every goroutine's stack (the debug=2 form a panic
// prints) to a new file in dumpDir and returns its path.
func (w *lockWatchdog) writeDump() (string, error) {
	dir := w.dumpDir
	if dir == "" {
		dir = os.TempDir()
	}
	path := filepath.Join(dir, fmt.Sprintf("hang-%s.txt", time.Now().UTC().Format("20060102T150405.000Z")))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	werr := pprof.Lookup("goroutine").WriteTo(f, 2)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return path, werr
	}
	return path, nil
}

// hubLockDumpDir is the logs directory: the directory of the event log, or
// "" (the temp directory) when the hub has no config.
func (h *Hub) hubLockDumpDir() string {
	if h.deps.Config == nil {
		return ""
	}
	dir := filepath.Dir(h.deps.Config.Logging.EventFile)
	if dir == "." {
		return ""
	}
	return dir
}

// startLockWatchdog starts the hub lock watchdog, once per hub. It stops when
// the gateway context ends; a hub with no context (tests) starts none.
func (h *Hub) startLockWatchdog() {
	ctx := h.deps.Ctx
	if ctx == nil {
		return
	}
	h.lockWatchOnce.Do(func() {
		w := h.newLockWatchdog(hubLockProbeInterval, hubLockStuckAfter)
		safeGo("hub-lock-watchdog", func() { w.run(ctx.Done()) })
	})
}

// newLockWatchdog returns a watchdog on the hub's lock.
func (h *Hub) newLockWatchdog(interval, stuckAfter time.Duration) *lockWatchdog {
	return &lockWatchdog{
		name:       "app hub lock",
		acquire:    func() { h.mu.RLock(); h.mu.RUnlock() }, //nolint:staticcheck // SA2001: the probe only proves the lock can be granted
		interval:   interval,
		stuckAfter: stuckAfter,
		dumpDir:    h.hubLockDumpDir(),
	}
}
