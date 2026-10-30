package tools

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"foci/internal/delegator"
)

// The allowance check on every spawn (#2662, Dick 2026-10-30): before a spawn
// starts, foci reads the plan allowance of the backend the spawn runs on (the
// target agent's, for a spawn as another agent). When any window has less
// than the reserve left (tools.spawn_reserve_percent, default 10), the spawn
// does not start and the caller is told which window, how much is left and
// when it resets, and that use_reserve (--use-reserve) runs it anyway. A
// backend with no usage source runs; so does a spawn whose usage read failed
// or has not come back yet, and its result says the allowance was not checked.
//
// The read must not make spawns slow: Claude Code's get_usage was measured at
// 25-58 s a call (2026-10-30, 4 samples). So reads are cached per usage
// account (UsageCache) and refreshed on a schedule (Dick 2026-10-30 12:06:
// "every 10 minutes with large fuzzing, 4-14 minutes"): each account is read
// once per interval, each interval drawn at random from 4-14 minutes, so a
// spawn's check normally finds a reading under usageFresh old and costs a map
// lookup. A reading up to usageStale old is still used while a new read runs;
// with none, the check waits at most usageWait and otherwise lets the spawn
// run unchecked.
//
// The account: Claude Code's usage is the operator's account, the same for
// every Claude Code agent (Spec.UsageOperatorWide), so all of them share ONE
// cache entry and one scheduled read, not one throwaway CC process each.
// codex reads per agent (its own app-server's rate-limit state).

// defaultSpawnReservePercent is the reserve when the config gives none.
const defaultSpawnReservePercent = 10

const (
	// usageEveryMin/Max bound one scheduled interval (uniform in between).
	usageEveryMin = 4 * time.Minute
	usageEveryMax = 14 * time.Minute
	// usageFirstMin/Max bound the delay before an account's first scheduled
	// read: soon after start, so the first spawns are checked, but not all
	// accounts in the same instant.
	usageFirstMin = 10 * time.Second
	usageFirstMax = 60 * time.Second
	// usageFresh covers the longest interval plus one slow read, so with
	// the schedule running a reading is always fresh.
	usageFresh = 20 * time.Minute
	usageStale = 60 * time.Minute
	// usageRetry: after a failed read, a spawn starts no new read for this
	// long (the schedule still reads at its next tick).
	usageRetry = 2 * time.Minute
	usageWait  = 3 * time.Second
	// usageReadTimeout bounds one read (Claude Code's own bound is 90 s;
	// codex reads its process's state).
	usageReadTimeout = 100 * time.Second
)

// UsageRead is one answer of a UsageReader.
type UsageRead struct {
	Info *delegator.UsageInfo
	Err  error
	// Age is how old Info is (zero for a reading made for this call).
	Age time.Duration
	// Pending: no reading is available yet; one is running.
	Pending bool
}

// UsageReader returns the current allowance reading of one agent's backend.
type UsageReader func(ctx context.Context) UsageRead

// UsageCache caches usage reads per account key and, once started, refreshes
// each registered key on its own randomised schedule.
type UsageCache struct {
	mu       sync.Mutex
	entries  map[string]*usageEntry
	now      func() time.Time
	wait     time.Duration
	interval func() time.Duration // next scheduled interval
	first    func() time.Duration // delay before a key's first scheduled read
	after    func(time.Duration) <-chan time.Time
	runCtx   context.Context // non-nil once Start ran: scheduling is on
}

type usageEntry struct {
	query     func(context.Context) (*delegator.UsageInfo, error)
	enabled   func() bool // the schedule reads only while true; nil = always
	scheduled bool        // its schedule loop runs

	info   *delegator.UsageInfo // last good reading
	infoAt time.Time            // when info was read; zero = never
	err    error                // the last read's error, while it is the latest read
	errAt  time.Time
	// running is non-nil while a read runs; closed when it ends.
	running chan struct{}
}

// NewUsageCache returns an empty cache on the wall clock, not scheduling.
func NewUsageCache() *UsageCache {
	return &UsageCache{
		entries:  map[string]*usageEntry{},
		now:      time.Now,
		wait:     usageWait,
		interval: func() time.Duration { return randBetween(usageEveryMin, usageEveryMax) },
		first:    func() time.Duration { return randBetween(usageFirstMin, usageFirstMax) },
		after:    time.After,
	}
}

func randBetween(lo, hi time.Duration) time.Duration {
	return lo + time.Duration(rand.Int64N(int64(hi-lo)+1)) //nolint:gosec // G404: schedule jitter, not a secret
}

// spawnUsage is the process-wide cache the gateway's spawn deps read through.
var spawnUsage = NewUsageCache()

// StartSpawnUsageRefresh turns on the scheduled usage reads of every account
// a spawn can check (registered by SpawnUsageReader, before or after this
// call), until ctx ends. The gateway calls it once at startup; tests that
// build spawn deps never do, so they start no usage reads.
func StartSpawnUsageRefresh(ctx context.Context) { spawnUsage.Start(ctx) }

// SpawnUsageReader is the UsageReader of agentID on backend: query through
// the process-wide cache, keyed by the usage account (the backend alone when
// operatorWide, else backend and agent). enabled gates the scheduled reads of
// that account (the first registration's is kept). Nil query = nil reader
// (no usage source).
func SpawnUsageReader(backend, agentID string, query delegator.UsageQuery, operatorWide bool, enabled func() bool) UsageReader {
	if query == nil {
		return nil
	}
	key := backend + "/" + agentID
	if operatorWide {
		key = backend
	}
	return spawnUsage.Register(key, func(ctx context.Context) (*delegator.UsageInfo, error) {
		return query(ctx, agentID)
	}, enabled)
}

// Register records key's query (the first registration wins: every
// registrant of one key reads the same account) and returns its reader. Once
// the cache is started, key's schedule runs.
func (c *UsageCache) Register(key string, query func(context.Context) (*delegator.UsageInfo, error), enabled func() bool) UsageReader {
	c.mu.Lock()
	e := c.entry(key)
	if e.query == nil {
		e.query, e.enabled = query, enabled
	}
	if c.runCtx != nil && !e.scheduled {
		e.scheduled = true
		go c.schedule(c.runCtx, key, e)
	}
	c.mu.Unlock()
	return func(ctx context.Context) UsageRead { return c.read(ctx, key) }
}

// Start turns scheduling on: every registered key, and every key registered
// later, is read on its own schedule until ctx ends. A second call is a no-op.
func (c *UsageCache) Start(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runCtx != nil {
		return
	}
	c.runCtx = ctx
	for key, e := range c.entries {
		if e.query != nil && !e.scheduled {
			e.scheduled = true
			go c.schedule(ctx, key, e)
		}
	}
}

// schedule reads key after a short first delay and then once per interval.
// A tick while a read already runs (one a spawn started) adds no second one.
func (c *UsageCache) schedule(ctx context.Context, key string, e *usageEntry) {
	wait := c.first()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.after(wait):
		}
		if e.enabled == nil || e.enabled() {
			c.mu.Lock()
			running := c.startRefreshLocked(key, e)
			c.mu.Unlock()
			select {
			case <-running:
			case <-ctx.Done():
				return
			}
		}
		wait = c.interval()
	}
}

func (c *UsageCache) entry(key string) *usageEntry {
	e := c.entries[key]
	if e == nil {
		e = &usageEntry{}
		c.entries[key] = e
	}
	return e
}

func (c *UsageCache) read(ctx context.Context, key string) UsageRead {
	c.mu.Lock()
	e := c.entry(key)
	now := c.now()
	age := now.Sub(e.infoAt)
	if !e.infoAt.IsZero() && age < usageFresh {
		r := UsageRead{Info: e.info, Age: age}
		c.mu.Unlock()
		return r
	}
	// No fresh reading. A read that failed lately is not retried on every
	// spawn (each Claude Code read is a process for up to a minute).
	recentErr := e.err != nil && now.Sub(e.errAt) < usageRetry
	running := e.running
	if running == nil && !recentErr && e.query != nil {
		running = c.startRefreshLocked(key, e)
	}
	switch {
	case !e.infoAt.IsZero() && age < usageStale:
		// Use the older reading while the new one runs.
		r := UsageRead{Info: e.info, Age: age}
		c.mu.Unlock()
		return r
	case e.err != nil && running == nil:
		r := UsageRead{Err: e.err}
		c.mu.Unlock()
		return r
	case running == nil:
		c.mu.Unlock()
		return UsageRead{Err: fmt.Errorf("no usage source registered")}
	}
	c.mu.Unlock()

	timer := time.NewTimer(c.wait)
	defer timer.Stop()
	select {
	case <-running:
		c.mu.Lock()
		defer c.mu.Unlock()
		if e.err != nil {
			return UsageRead{Err: e.err}
		}
		return UsageRead{Info: e.info, Age: c.now().Sub(e.infoAt)}
	case <-timer.C:
	case <-ctx.Done():
	}
	return UsageRead{Pending: true}
}

// startRefreshLocked starts one read of key unless one runs, and returns the
// channel closed when the running read ends. c.mu must be held.
func (c *UsageCache) startRefreshLocked(key string, e *usageEntry) chan struct{} {
	if e.running != nil {
		return e.running
	}
	done := make(chan struct{})
	e.running = done
	go c.refresh(key, e, done)
	return done
}

func (c *UsageCache) refresh(key string, e *usageEntry, done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), usageReadTimeout)
	defer cancel()
	start := time.Now()
	info, err := e.query(ctx)
	spawnLog.Infof("usage read for %s took %s (err=%v)", key, time.Since(start).Round(time.Millisecond), err)
	c.mu.Lock()
	if err != nil {
		e.err, e.errAt = err, c.now()
	} else {
		e.info, e.infoAt, e.err = info, c.now(), nil
	}
	e.running = nil
	c.mu.Unlock()
	close(done)
}

// lowWindow is one allowance window under the reserve.
type lowWindow struct {
	Name     string // e.g. "5-hour", "weekly", "premium 7d"
	Left     int    // percent left
	ResetsAt time.Time
}

// lowWindows lists the windows of info with less than reserve percent left.
// A window the backend did not report (Claude Code: no reset time and no use)
// is skipped.
func lowWindows(info *delegator.UsageInfo, reserve int) []lowWindow {
	if info == nil {
		return nil
	}
	var out []lowWindow
	check := func(name string, used int, resets time.Time) {
		if used == 0 && resets.IsZero() {
			return // not reported
		}
		if left := 100 - used; left < reserve {
			out = append(out, lowWindow{Name: name, Left: max(left, 0), ResetsAt: resets})
		}
	}
	if a := info.Allowance; a != nil {
		for _, b := range a.Buckets {
			for _, w := range b.Windows {
				name := w.Label
				if b.Name != "" {
					name = b.Name + " " + w.Label
				}
				check(name, w.Percent, w.ResetsAt)
			}
		}
		return out
	}
	check("5-hour", info.FiveHour.Percent, info.FiveHour.ResetsAt)
	check("weekly", info.SevenDay.Percent, info.SevenDay.ResetsAt)
	return out
}

// errLowAllowance is the refusal of a spawn whose backend is low.
func errLowAllowance(agentID, backend string, low []lowWindow, reserve int) error {
	parts := make([]string, 0, len(low))
	for _, w := range low {
		p := fmt.Sprintf("the %s window has %d%% left", w.Name, w.Left)
		if !w.ResetsAt.IsZero() {
			p += " (resets " + w.ResetsAt.Local().Format("Mon 15:04 MST") + ")"
		}
		parts = append(parts, p)
	}
	if backend == "" {
		backend = "backend"
	}
	return fmt.Errorf("spawn not started: %s's %s allowance is running low: %s, under the %d%% reserve. To run it anyway, call spawn again with use_reserve=true (--use-reserve)",
		agentID, backend, strings.Join(parts, "; "), reserve)
}

// reservePercent is the reserve a spawn of this caller keeps.
func (s *Spawner) reservePercent() int {
	if s.deps.ReservePercent == nil {
		return defaultSpawnReservePercent
	}
	return s.deps.ReservePercent()
}

// checkAllowance checks the allowance of s's backend (the agent the spawn
// runs as) against reserve before a spawn runs on it. It returns an error when the spawn must not start, and
// otherwise a note for the result when the allowance could not be checked
// ("" when it was, or when the backend has no usage source).
func (s *Spawner) checkAllowance(ctx context.Context, reserve int, useReserve bool) (note string, err error) {
	if useReserve || s.deps.Usage == nil {
		return "", nil
	}
	r := s.deps.Usage(ctx)
	switch {
	case r.Pending:
		return "\n\n(The backend allowance was not checked: the usage read is still running. Later spawns are checked once it is in.)", nil
	case r.Err != nil:
		spawnLog.Warnf("agent=%s spawn allowance check: usage read failed: %v", s.deps.AgentID, r.Err)
		return fmt.Sprintf("\n\n(The backend allowance was not checked: the usage read failed: %v)", r.Err), nil
	}
	if low := lowWindows(r.Info, reserve); len(low) > 0 {
		backend := ""
		if r.Info != nil {
			backend = r.Info.Backend
		}
		return "", errLowAllowance(s.deps.AgentID, backend, low, reserve)
	}
	return "", nil
}
