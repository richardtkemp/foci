package clock

import (
	"sort"
	"sync"
	"time"
)

// Fake is a manually-advanced Clock for deterministic tests. Now() and any
// AfterFunc callback only move/fire when the test calls Advance — there is no
// wall-clock wait anywhere, so a test built on Fake cannot flake under load.
//
// Fake is safe for concurrent use: Now/AfterFunc/Stop/Reset may be called
// from the goroutine under test while Advance is called from the test
// goroutine. Advance itself must only be called from the test goroutine (it
// is the thing driving time forward) and must not be called concurrently
// with itself.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	// armed is closed (and replaced) whenever a timer is armed, waking
	// BlockUntilArmed. nil until someone waits.
	armed chan struct{}
}

// NewFake returns a Fake clock starting at an arbitrary fixed instant (not
// the real wall clock — tests should never need the real time, only
// durations relative to their own starting point).
func NewFake() *Fake {
	return &Fake{now: time.Unix(0, 0)}
}

// Now returns the fake clock's current virtual time.
func (c *Fake) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// AfterFunc schedules f to run when the fake clock's virtual time reaches
// Now()+d, i.e. the next time Advance crosses that deadline. f runs
// synchronously on the goroutine that called Advance, not in its own
// goroutine — unlike the real Clock, so callers must not rely on AfterFunc
// callbacks running concurrently with the caller when using Fake.
func (c *Fake) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clock: c, deadline: c.now.Add(d), fn: f, active: true}
	c.timers = append(c.timers, t)
	c.signalArmedLocked()
	return t
}

// BlockUntilArmed waits until at least n timers are armed (scheduled, not yet
// fired or stopped) or timeout passes, and reports whether n was reached.
//
// It closes the race between a goroutine under test arming its timer and the
// test calling Advance: an Advance that lands first moves virtual time past
// nothing, and the timer armed afterwards is due a full duration later, so it
// never fires. Wait for the timer, then Advance. timeout is a real-time bound
// so a timer that is never armed fails the test instead of hanging it.
func (c *Fake) BlockUntilArmed(n int, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		c.mu.Lock()
		count := 0
		for _, t := range c.timers {
			if t.active {
				count++
			}
		}
		if count >= n {
			c.mu.Unlock()
			return true
		}
		if c.armed == nil {
			c.armed = make(chan struct{})
		}
		wake := c.armed
		c.mu.Unlock()
		select {
		case <-wake:
		case <-deadline.C:
			return false
		}
	}
}

// signalArmedLocked wakes every BlockUntilArmed waiter. Caller holds c.mu.
func (c *Fake) signalArmedLocked() {
	if c.armed != nil {
		close(c.armed)
		c.armed = nil
	}
}

// Advance moves the fake clock's virtual time forward by d and then runs, in
// deadline order, every armed timer whose deadline is now due. Call this in
// place of time.Sleep in a test: it deterministically fires exactly the
// timers that would have fired by then, with no scheduling variance.
func (c *Fake) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var due []*fakeTimer
	for _, t := range c.timers {
		if t.active && !t.deadline.After(now) {
			t.active = false
			due = append(due, t)
		}
	}
	c.mu.Unlock()

	sort.Slice(due, func(i, j int) bool { return due[i].deadline.Before(due[j].deadline) })
	for _, t := range due {
		t.fn()
	}
}

// fakeTimer is the Timer returned by Fake.AfterFunc.
type fakeTimer struct {
	clock    *Fake
	deadline time.Time
	fn       func()
	active   bool
}

// Stop cancels the timer, returning true iff it was still armed (matching
// time.Timer.Stop's contract).
func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := t.active
	t.active = false
	return was
}

// Reset reschedules the timer to fire at the fake clock's current time plus
// d, returning true iff it was still armed beforehand (matching
// time.Timer.Reset's contract).
func (t *fakeTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := t.active
	t.deadline = t.clock.now.Add(d)
	t.active = true
	t.clock.signalArmedLocked()
	return was
}
