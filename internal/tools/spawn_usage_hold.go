package tools

import (
	"context"
	"errors"
	"sync"
	"time"

	"foci/internal/turnevent"
)

// A background spawn in a usage hold (#2491 requirement 3, holdout H9).
//
// A step the hold interrupts pauses: the spawn stays "running" to its parent
// (no [SPAWN RESULT]), its timeout stops, and its branch joins the keepalive
// warm set. At lift a spawn whose cache is still warm goes on with the
// hidden resume line; one whose cache went cold is not resumed (Dick
// 2026-11-02: "It was paused. There is nothing to resume.") and ends with one
// [SPAWN RESULT] saying so, so its parent does not wait for ever.

// UsageResumeLine is the hidden input of a turn the usage hold interrupted,
// at lift (Dick 2026-11-02: "simulate a user message saying \"resume what you
// were doing\" - do not show this in the clients").
const UsageResumeLine = "resume what you were doing"

// spawnHoldColdText ends the [SPAWN RESULT] of a paused spawn gone cold.
const spawnHoldColdText = "was stopped by the usage hold and not resumed: its cache went cold while it was paused, so there is nothing to resume. Start a new spawn to go on."

// spawnStep is how one step of a background spawn ended.
type spawnStep struct {
	// resume: the hold paused the step and lifted with the cache warm; run
	// texts next.
	resume bool
	texts  []string
	err    error
	result string
	// cold: the hold paused the step and the cache went cold; the spawn ends.
	cold bool
}

// runSpawnStep runs texts on branchKey once, pausing across a usage hold (see
// the file comment). A step never starts while the account holds: one
// restored or resumed then waits for the lift first.
func (s *Spawner) runSpawnStep(agent SpawnAgent, pauser UsageHoldPauser, runCtx, spawnCtx context.Context, parent, id, branchKey string, texts []string) spawnStep {
	if pauser != nil && pauser.UsageHoldActive() && !s.waitHoldLift(pauser, spawnCtx, parent, id, branchKey) {
		return spawnStep{err: context.Cause(spawnCtx)}
	}
	turnCtx, holdDone := runCtx, func() bool { return false }
	if pauser != nil {
		turnCtx, holdDone = pauser.UsageHoldTurn(runCtx, branchKey, "spawn")
	}
	tr, obs := s.deps.Tracker, s.deps.Observer
	buf := turnevent.NewBufferSink()
	sink := &spawnTextSink{BufferSink: buf, obs: obs, tracker: tr, parent: parent, id: id}
	err := agent.HandleMessage(turnevent.WithSink(turnCtx, sink), branchKey, texts, nil)
	step := spawnStep{err: err, result: buf.FinalText()}
	if !holdDone() {
		return step
	}
	if spawnCtx.Err() != nil {
		pauser.UsageHoldPause(branchKey, "spawn", false) // stopped, not paused
		return step
	}
	if !s.waitHoldLift(pauser, spawnCtx, parent, id, branchKey) {
		return spawnStep{err: context.Cause(spawnCtx)}
	}
	if s.cacheCold(agent, branchKey) {
		spawnLog.Infof("spawn=%s branch=%s paused by the usage hold went cold — not resumed", id, branchKey)
		return spawnStep{cold: true}
	}
	spawnLog.Infof("spawn=%s branch=%s resumes after the usage hold", id, branchKey)
	tr.appendLog(parent, id, "message", "(resumed after the usage hold)")
	return spawnStep{resume: true, texts: append([]string{UsageResumeLine}, tr.drainPending(parent, id)...)}
}

// waitHoldLift pauses the spawn until the hold lifts: its timeout stops and
// its branch is in the paused set meanwhile. False when spawnCtx ended (a
// stop, or shutdown) first.
func (s *Spawner) waitHoldLift(pauser UsageHoldPauser, spawnCtx context.Context, parent, id, branchKey string) bool {
	clock := spawnClockFrom(spawnCtx)
	clock.pause()
	defer clock.resume()
	spawnLog.Infof("spawn=%s branch=%s paused by the usage hold", id, branchKey)
	s.deps.Tracker.appendLog(parent, id, "message", "(paused by the usage hold)")
	pauser.UsageHoldPause(branchKey, "spawn", true)
	defer pauser.UsageHoldPause(branchKey, "spawn", false)
	return pauser.WaitUsageLift(spawnCtx)
}

// cacheCold reports whether branchKey's prompt cache has expired, by the same
// reading spawn_send's cold rule uses. An agent that reports no cache reads
// as warm.
func (s *Spawner) cacheCold(agent SpawnAgent, branchKey string) bool {
	rep, ok := agent.(SpawnCacheReporter)
	if !ok {
		return false
	}
	now := time.Now()
	return !rep.CacheExpiry(branchKey, now).After(now)
}

// spawnClock is a spawn's timeout that a usage hold can stop and restart:
// the time paused does not count.
type spawnClock struct {
	mu        sync.Mutex
	cancel    context.CancelCauseFunc
	timer     *time.Timer
	remaining time.Duration
	deadline  time.Time
	paused    int
}

type spawnClockKey struct{}

// withSpawnClock is context.WithTimeout with a pausable clock: at timeout the
// context is cancelled with cause context.DeadlineExceeded.
func withSpawnClock(base context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(base)
	c := &spawnClock{cancel: cancel, deadline: time.Now().Add(timeout)}
	c.timer = time.AfterFunc(timeout, func() { cancel(context.DeadlineExceeded) })
	ctx = &clockCtx{Context: context.WithValue(ctx, spawnClockKey{}, c), clock: c}
	return ctx, func() {
		c.mu.Lock()
		c.timer.Stop()
		c.mu.Unlock()
		cancel(context.Canceled)
	}
}

// clockCtx keeps context.WithTimeout's reading of a spawn context: Err is
// context.DeadlineExceeded once the clock ran out, and Deadline is the
// clock's (none while paused).
type clockCtx struct {
	context.Context
	clock *spawnClock
}

func (c *clockCtx) Err() error {
	err := c.Context.Err()
	if err != nil && errors.Is(context.Cause(c.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return err
}

func (c *clockCtx) Deadline() (time.Time, bool) {
	c.clock.mu.Lock()
	defer c.clock.mu.Unlock()
	if c.clock.paused > 0 {
		return c.Context.Deadline()
	}
	if d, ok := c.Context.Deadline(); ok && d.Before(c.clock.deadline) {
		return d, true
	}
	return c.clock.deadline, true
}

// spawnClockFrom is ctx's spawn clock (a no-op clock when it has none).
func spawnClockFrom(ctx context.Context) *spawnClock {
	if c, ok := ctx.Value(spawnClockKey{}).(*spawnClock); ok {
		return c
	}
	return nil
}

func (c *spawnClock) pause() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused++
	if c.paused > 1 {
		return
	}
	if c.timer.Stop() {
		c.remaining = time.Until(c.deadline)
	} else {
		c.remaining = -1 // already fired
	}
}

func (c *spawnClock) resume() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused--
	if c.paused > 0 || c.remaining < 0 {
		return
	}
	c.deadline = time.Now().Add(c.remaining)
	c.timer = time.AfterFunc(c.remaining, func() { c.cancel(context.DeadlineExceeded) })
}
