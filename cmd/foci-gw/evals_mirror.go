package main

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"foci/internal/clock"
	"foci/internal/evals"
	"foci/internal/log"
	"foci/internal/telemetry"
)

// Retry cadence for the score-config mirror (#3189, Dick's ruling
// 2026-11-19: "Retry the mirror in the background (e.g. every minute) until
// it succeeds; WARN once only if still failing after some minutes.").
const (
	scoreMirrorRetryEvery = time.Minute
	scoreMirrorWarnAfter  = 5 * time.Minute
	scoreMirrorPassLimit  = 30 * time.Second
)

// rubricSource is the part of *evals.Registry the mirror needs.
type rubricSource interface {
	Get(name string) (*evals.Rubric, bool)
	SetConfigID(name, id string)
}

// scoreConfigMirror mirrors rubrics into Langfuse score configs and keeps
// retrying the ones that fail until they succeed.
//
// At boot Langfuse is often not up yet (connection refused), and a rubric
// whose config never mirrors posts every score without a configId until the
// next restart. So a failed rubric stays pending and a pass runs again every
// retryEvery. There is at most one pass running or one retry timer armed at
// any time: Enqueue from a rubric reload joins the running loop, it never
// starts a second one.
//
// Each failed attempt logs at INFO. One WARN fires if rubrics are still
// pending warnAfter after the first failed pass; it re-arms only after every
// pending rubric has mirrored.
type scoreConfigMirror struct {
	reg        rubricSource
	ensure     func(context.Context, telemetry.ScoreConfig) (id, warning string, err error)
	clk        clock.Clock
	retryEvery time.Duration
	warnAfter  time.Duration
	log        *log.ComponentLogger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu sync.Mutex
	// pending maps a rubric name to the generation it was enqueued at. A
	// pass removes a name only if its generation is unchanged, so a reload
	// that re-enqueues a rubric mid-pass is not lost.
	pending      map[string]uint64
	gen          uint64
	running      bool        // a pass goroutine is running
	again        bool        // Enqueue arrived during a pass: run another at once
	timer        clock.Timer // the armed retry, nil when none
	timerSeq     uint64      // identifies the armed timer; a stale callback sees a different value
	failingSince time.Time   // start of the current failing stretch; zero when none
	warned       bool
	closed       bool
}

func newScoreConfigMirror(reg rubricSource, ensure func(context.Context, telemetry.ScoreConfig) (string, string, error), clk clock.Clock, lg *log.ComponentLogger) *scoreConfigMirror {
	ctx, cancel := context.WithCancel(context.Background())
	return &scoreConfigMirror{
		reg:        reg,
		ensure:     ensure,
		clk:        clk,
		retryEvery: scoreMirrorRetryEvery,
		warnAfter:  scoreMirrorWarnAfter,
		log:        lg,
		ctx:        ctx,
		cancel:     cancel,
		pending:    map[string]uint64{},
	}
}

// Enqueue marks rubrics for mirroring and starts a pass now: in a new
// goroutine when the loop is idle or waiting on its retry timer, or right
// after the running pass ends.
func (m *scoreConfigMirror) Enqueue(rs []*evals.Rubric) {
	if len(rs) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	for _, rb := range rs {
		m.gen++
		m.pending[rb.Name] = m.gen
	}
	if m.running {
		m.again = true
		return
	}
	m.stopTimerLocked()
	m.startLocked()
}

// stopTimerLocked disarms the retry timer. A callback already on its way
// finds timerSeq changed and does nothing. Caller holds m.mu.
func (m *scoreConfigMirror) stopTimerLocked() {
	m.timerSeq++
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
}

// startLocked starts a pass goroutine. Caller holds m.mu, and no pass is
// running and no timer is armed.
func (m *scoreConfigMirror) startLocked() {
	m.running = true
	m.wg.Add(1)
	go m.run()
}

// onTimer is the callback of the retry timer armed as seq.
func (m *scoreConfigMirror) onTimer(seq uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if seq != m.timerSeq {
		return
	}
	m.timer = nil
	if m.closed || m.running || len(m.pending) == 0 {
		return
	}
	m.startLocked()
}

func (m *scoreConfigMirror) run() {
	defer m.wg.Done()
	for {
		m.pass()
		m.mu.Lock()
		if m.again && !m.closed {
			m.again = false
			m.mu.Unlock()
			continue
		}
		m.running = false
		m.afterPassLocked()
		m.mu.Unlock()
		return
	}
}

// pass tries every pending rubric once.
func (m *scoreConfigMirror) pass() {
	m.mu.Lock()
	snap := make(map[string]uint64, len(m.pending))
	for name, g := range m.pending {
		snap[name] = g
	}
	m.mu.Unlock()
	names := make([]string, 0, len(snap))
	for name := range snap {
		names = append(names, name)
	}
	sort.Strings(names)

	ctx, cancel := context.WithTimeout(m.ctx, scoreMirrorPassLimit)
	defer cancel()
	for _, name := range names {
		if m.ctx.Err() != nil {
			return
		}
		rb, ok := m.reg.Get(name)
		if !ok { // removed since it was enqueued: nothing to mirror
			m.donePending(name, snap[name])
			continue
		}
		id, warn, err := m.ensure(ctx, scoreConfigFor(rb))
		if err != nil {
			m.log.Infof("rubric %s: score config not mirrored yet (retrying every %s): %v", name, m.retryEvery, err)
			continue
		}
		if warn != "" {
			m.log.Warnf("rubric %s: %s", name, warn)
		}
		m.reg.SetConfigID(name, id)
		m.donePending(name, snap[name])
	}
}

func (m *scoreConfigMirror) donePending(name string, gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending[name] == gen {
		delete(m.pending, name)
	}
}

// afterPassLocked arms the next retry and does the once-only WARN. Caller
// holds m.mu.
func (m *scoreConfigMirror) afterPassLocked() {
	if m.closed {
		return
	}
	if len(m.pending) == 0 {
		if m.warned {
			m.log.Infof("score configs mirrored; the earlier failure has cleared")
		}
		m.failingSince = time.Time{}
		m.warned = false
		return
	}
	now := m.clk.Now()
	if m.failingSince.IsZero() {
		m.failingSince = now
	}
	if !m.warned && now.Sub(m.failingSince) >= m.warnAfter {
		m.warned = true
		names := make([]string, 0, len(m.pending))
		for name := range m.pending {
			names = append(names, name)
		}
		sort.Strings(names)
		m.log.Warnf("score config(s) for rubric(s) [%s] still not mirrored after %s; scores post without configId until they are (retrying every %s)",
			strings.Join(names, " "), now.Sub(m.failingSince).Round(time.Second), m.retryEvery)
	}
	m.timerSeq++
	seq := m.timerSeq
	m.timer = m.clk.AfterFunc(m.retryEvery, func() { m.onTimer(seq) })
}

// Close stops the retry loop and waits for a running pass to end.
func (m *scoreConfigMirror) Close() {
	m.mu.Lock()
	m.closed = true
	m.stopTimerLocked()
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
}
