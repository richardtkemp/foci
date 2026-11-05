package agent

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/delegator"
)

// defaultLivePlacePoll is how often a user turn waiting for a live-session
// place (#2280) re-checks for a backend that has gone idle and can be evicted.
// A busy backend that finishes its turn or its background work frees no place
// by itself (it stays live), and those transitions have no channel, so the wait
// polls, like the deferred compaction bounce (defaultDeferredBouncePoll).
const defaultLivePlacePoll = time.Second

// LiveSessionLimiter counts the live delegated backends of every
// DelegatedManager in the gateway and enforces the live-session caps (#2280):
// the gateway total ([sessions] max_live_sessions, held here) and each agent's
// own cap ([[agents]].sessions.max_live_sessions, DelegatedManager.
// MaxLiveSessions). One limiter is built in main and shared by every delegated
// agent; a manager built without one (tests) gets a private limiter with no
// gateway cap.
//
// A backend holds a place from the moment its creation starts until it leaves
// its manager's map (livePlace.release, exactly once per backend). When a
// creation finds a cap full, the least-recently-used IDLE backend under that
// cap is evicted (closed like an idle close, resume ID kept). When nothing is
// idle, a creation for a user turn waits — FIFO with the other waiters, one
// queued notice — while every other creation (a system one) goes ahead over
// the cap, and the count comes back down to the cap at the next creation or
// close once backends are idle again.
//
// Locking: l.mu guards the counts and the waiter queue only. It is never held
// while a manager's m.mu is taken, and m.mu is never held while l.mu is taken
// (places are released after a manager drops its lock), so there is no lock
// order between them. No lock is held across a backend's Close.
type LiveSessionLimiter struct {
	globalCap int

	// poll is how often a waiting user creation re-checks for an evictable
	// backend. Zero uses defaultLivePlacePoll; tests shorten it.
	poll time.Duration

	mu       sync.Mutex
	live     int                       // live places across all managers
	perAgent map[*DelegatedManager]int // live places per manager
	managers []*DelegatedManager       // every manager that has taken a place (global eviction scope)
	waiters  []*liveWaiter             // user creations waiting for a place, FIFO
	wake     chan struct{}             // closed (and replaced) whenever the counts or queue change

	// scans counts eviction scans (evictLRU calls). Observability for tests,
	// which wait on it to prove a waiter kept polling.
	scans atomic.Int64
}

// NewLiveSessionLimiter builds the gateway's limiter. globalCap is [sessions]
// max_live_sessions: the most live delegated backends across all agents;
// 0 = no gateway cap.
func NewLiveSessionLimiter(globalCap int) *LiveSessionLimiter {
	if globalCap < 0 {
		globalCap = 0
	}
	return &LiveSessionLimiter{globalCap: globalCap}
}

// liveWaiter is one user-turn creation waiting for a place.
type liveWaiter struct {
	m       *DelegatedManager
	granted bool // a place was reserved for it by grantLocked
}

// livePlace is the place one backend holds. release frees it exactly once,
// whichever way the backend leaves (close, reset, idle reap, eviction, dead
// respawn, failed start, shutdown).
type livePlace struct {
	l        *LiveSessionLimiter
	m        *DelegatedManager
	released atomic.Bool
}

// release frees the place. Safe to call more than once and on nil; only the
// first call counts. Must not be called with the manager's m.mu held.
func (p *livePlace) release() {
	if p == nil || !p.released.CompareAndSwap(false, true) {
		return
	}
	l := p.l
	l.mu.Lock()
	l.live--
	l.perAgent[p.m]--
	l.grantLocked()
	l.broadcastLocked()
	over := l.overCapLocked()
	l.mu.Unlock()
	// A system creation that went over a cap while nothing was idle leaves the
	// count above it; bring it back down as soon as something is evictable.
	if over {
		go l.trim()
	}
}

func (l *LiveSessionLimiter) pollInterval() time.Duration {
	if l.poll > 0 {
		return l.poll
	}
	return defaultLivePlacePoll
}

// registerLocked adds m to the global eviction scope.
func (l *LiveSessionLimiter) registerLocked(m *DelegatedManager) {
	if l.perAgent == nil {
		l.perAgent = make(map[*DelegatedManager]int)
	}
	if _, ok := l.perAgent[m]; !ok {
		l.perAgent[m] = 0
		l.managers = append(l.managers, m)
	}
}

// broadcastLocked wakes every waiter.
func (l *LiveSessionLimiter) broadcastLocked() {
	if l.wake != nil {
		close(l.wake)
	}
	l.wake = make(chan struct{})
}

// wakeChLocked returns the channel the next broadcast closes.
func (l *LiveSessionLimiter) wakeChLocked() <-chan struct{} {
	if l.wake == nil {
		l.wake = make(chan struct{})
	}
	return l.wake
}

// capFull says which cap (if any) has no room for one more backend of m.
type capFull struct {
	agent  bool
	global bool
}

func (c capFull) any() bool { return c.agent || c.global }

// fullLocked reports which of m's caps are full.
func (l *LiveSessionLimiter) fullLocked(m *DelegatedManager) capFull {
	return capFull{
		agent:  m.MaxLiveSessions > 0 && l.perAgent[m] >= m.MaxLiveSessions,
		global: l.globalCap > 0 && l.live >= l.globalCap,
	}
}

// overCapLocked reports whether any count is above its cap (a system creation
// went over while nothing was idle).
func (l *LiveSessionLimiter) overCapLocked() bool {
	if l.globalCap > 0 && l.live > l.globalCap {
		return true
	}
	for m, n := range l.perAgent {
		if m.MaxLiveSessions > 0 && n > m.MaxLiveSessions {
			return true
		}
	}
	return false
}

// reserveLocked takes a place for m.
func (l *LiveSessionLimiter) reserveLocked(m *DelegatedManager) *livePlace {
	l.live++
	l.perAgent[m]++
	return &livePlace{l: l, m: m}
}

// grantLocked walks the queue in arrival order and reserves a place for every
// waiter that fits. A waiter that does not fit leaves its full cap full for
// everyone behind it, so waiters for the same cap are served FIFO, while a
// waiter blocked only by its own agent's cap does not hold up other agents.
func (l *LiveSessionLimiter) grantLocked() {
	for _, w := range l.waiters {
		if w.granted {
			continue
		}
		if l.fullLocked(w.m).any() {
			continue
		}
		if l.blockedByEarlierLocked(w, w.m) {
			continue
		}
		l.live++
		l.perAgent[w.m]++
		w.granted = true
	}
}

// blockedByEarlierLocked reports whether a still-waiting waiter ahead of w
// needs a cap w would also take: one of the same agent when the agent has a
// cap, or any waiter that is short of the gateway cap. w == nil asks for a new
// arrival, which is behind every waiter. grantLocked runs on every release, so
// a waiter still queued is genuinely short of a place.
func (l *LiveSessionLimiter) blockedByEarlierLocked(w *liveWaiter, m *DelegatedManager) bool {
	for _, x := range l.waiters {
		if x == w {
			return false
		}
		if x.granted {
			continue
		}
		if x.m == m && m.MaxLiveSessions > 0 {
			return true
		}
		if l.fullLocked(x.m).global {
			return true
		}
	}
	return false
}

// removeWaiterLocked drops w from the queue.
func (l *LiveSessionLimiter) removeWaiterLocked(w *liveWaiter) {
	for i, x := range l.waiters {
		if x == w {
			l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)
			return
		}
	}
}

// capLabel names the full cap for a log line: which cap and its size.
func (l *LiveSessionLimiter) capLabel(m *DelegatedManager, full capFull) string {
	if full.agent {
		return fmt.Sprintf("agent cap %d", m.MaxLiveSessions)
	}
	return fmt.Sprintf("global cap %d", l.globalCap)
}

// acquire takes a place for a new backend of m on sessionKey. user is true
// only for the creation a user turn needs (DelegatedManager.GetForUserTurn):
// that one waits when a cap is full and nothing is idle; every other creation
// goes over the cap instead. The wait ends with ctx's error or, once the agent
// is shutting down, ErrShuttingDown; neither leaks a place.
func (l *LiveSessionLimiter) acquire(ctx context.Context, m *DelegatedManager, sessionKey string, user bool) (*livePlace, error) {
	var (
		w        *liveWaiter
		waitFrom time.Time
	)
	// Leaving the queue without the place that was granted meanwhile gives it
	// back, so a cancelled or refused wait never leaks a place.
	defer func() {
		if w == nil {
			return
		}
		l.mu.Lock()
		l.removeWaiterLocked(w)
		if w.granted {
			// Still marked: the grant was never handed out (error return).
			l.live--
			l.perAgent[m]--
			l.grantLocked()
		}
		l.broadcastLocked()
		l.mu.Unlock()
	}()

	for {
		if m.shuttingDown.Load() {
			return nil, fmt.Errorf("create delegated backend for %s: %w", sessionKey, ErrShuttingDown)
		}

		l.mu.Lock()
		l.registerLocked(m)
		if w != nil && w.granted {
			w.granted = false // handed out: the deferred removal must not give it back
			l.mu.Unlock()
			m.logger().Infof("session %s got a live-session place after waiting %s", sessionKey, time.Since(waitFrom).Round(time.Millisecond))
			return &livePlace{l: l, m: m}, nil
		}
		full := l.fullLocked(m)
		blocked := user && l.blockedByEarlierLocked(w, m)
		if !full.any() && !blocked {
			p := l.reserveLocked(m)
			l.mu.Unlock()
			return p, nil
		}
		l.mu.Unlock()

		// A full cap: make room by evicting the least-recently-used idle
		// backend under it, then try again.
		if full.any() && l.evictLRU(m, full) {
			continue
		}

		l.mu.Lock()
		full = l.fullLocked(m)
		if w != nil && w.granted {
			l.mu.Unlock()
			continue
		}
		if !user {
			// Nothing idle: a system creation never waits (Dick 2026-10-11).
			p := l.reserveLocked(m)
			agentLive, live := l.perAgent[m], l.live
			l.mu.Unlock()
			if full.any() {
				n := live
				if full.agent {
					n = agentLive
				}
				m.logger().Infof("session %s created over the %s (%d live): nothing idle to evict, and a system creation never waits", sessionKey, l.capLabel(m, full), n)
			}
			return p, nil
		}
		first := w == nil
		if first {
			w = &liveWaiter{m: m}
			l.waiters = append(l.waiters, w)
			l.grantLocked()
		}
		wake := l.wakeChLocked()
		l.mu.Unlock()

		if first {
			waitFrom = time.Now()
			reason := "an earlier waiter"
			if full.any() {
				reason = l.capLabel(m, full)
			}
			m.logger().Infof("session %s waits for a live-session place (%s full, nothing idle)", sessionKey, reason)
			if m.SystemNoticeFunc != nil {
				go m.SystemNoticeFunc(sessionKey, turnQueuedNotice)
			}
		}

		t := time.NewTimer(l.pollInterval())
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// evictCandidate is one idle backend that may be evicted.
type evictCandidate struct {
	m        *DelegatedManager
	key      string
	mb       *managedBackend
	lastSeen time.Time
	seenAt   time.Time // mb.lastActive when scanned: a later Get changes it
}

// evictLRU evicts the least-recently-used idle backend under the full cap:
// m's own backends for the agent cap, every registered manager's for the
// gateway cap. It reports whether one was evicted.
func (l *LiveSessionLimiter) evictLRU(m *DelegatedManager, full capFull) bool {
	l.scans.Add(1)
	var scope []*DelegatedManager
	if full.agent {
		scope = []*DelegatedManager{m}
	} else {
		l.mu.Lock()
		scope = append(scope, l.managers...)
		l.mu.Unlock()
	}
	var cands []evictCandidate
	for _, mgr := range scope {
		cands = append(cands, mgr.idleCandidates()...)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].lastSeen.Before(cands[j].lastSeen) })
	for _, c := range cands {
		if c.m.evictExact(c, l.capLabel(m, full)) {
			return true
		}
	}
	return false
}

// broadcast wakes every waiter, so it re-checks (shutdown, a turn ending).
func (l *LiveSessionLimiter) broadcast() {
	l.mu.Lock()
	l.broadcastLocked()
	l.mu.Unlock()
}

// trim brings every count above its cap back down by evicting idle backends.
func (l *LiveSessionLimiter) trim() {
	for {
		l.mu.Lock()
		var (
			target *DelegatedManager
			full   capFull
		)
		for _, m := range l.managers {
			if m.MaxLiveSessions > 0 && l.perAgent[m] > m.MaxLiveSessions {
				target, full = m, capFull{agent: true}
				break
			}
		}
		if target == nil && l.globalCap > 0 && l.live > l.globalCap && len(l.managers) > 0 {
			target, full = l.managers[0], capFull{global: true}
		}
		l.mu.Unlock()
		if target == nil || !l.evictLRU(target, full) {
			return
		}
	}
}

// limiter returns the manager's live-session limiter: the shared gateway one,
// or a private one with no gateway cap when none was wired.
func (m *DelegatedManager) limiter() *LiveSessionLimiter {
	m.limiterOnce.Do(func() {
		if m.LiveSessions == nil {
			m.LiveSessions = NewLiveSessionLimiter(0)
		}
	})
	return m.LiveSessions
}

// idleCandidates lists the backends of m that may be evicted (#2280): running,
// not mid-creation, no turn in flight (backend or agent), no background work,
// no pending permission prompt, and not compacting. Stricter than closeIdle:
// evicting work in progress would kill subagents or strand a prompt. The
// backend calls are made outside m.mu.
func (m *DelegatedManager) idleCandidates() []evictCandidate {
	type snap struct {
		key      string
		mb       *managedBackend
		lastSeen time.Time
		seenAt   time.Time
	}
	m.mu.Lock()
	snaps := make([]snap, 0, len(m.backends))
	for key, mb := range m.backends {
		if mb.creating.Load() {
			continue
		}
		snaps = append(snaps, snap{key: key, mb: mb, lastSeen: mb.lastActive, seenAt: mb.lastActive})
	}
	m.mu.Unlock()

	var out []evictCandidate
	for _, s := range snaps {
		if !m.evictable(s.key, s.mb) {
			continue
		}
		lastSeen := s.lastSeen
		if ac, ok := delegator.As[delegator.ActivityChecker](s.mb.be); ok {
			if t := ac.LastActivity(); !t.IsZero() && t.After(lastSeen) {
				lastSeen = t
			}
		}
		out = append(out, evictCandidate{m: m, key: s.key, mb: s.mb, lastSeen: lastSeen, seenAt: s.seenAt})
	}
	return out
}

// evictable is the eviction idle predicate for one backend (see
// idleCandidates). Never called with m.mu held.
func (m *DelegatedManager) evictable(key string, mb *managedBackend) bool {
	be := mb.be
	if !be.IsRunning() || be.IsTurnInFlight() {
		return false
	}
	if r, ok := delegator.As[delegator.SubagentReporter](be); ok && len(r.RunningSubagents()) > 0 {
		return false
	}
	if aw, ok := delegator.As[delegator.AutonomousRunAwaiter](be); ok && aw.AwaitingAutonomousRun() {
		return false
	}
	mb.permMu.Lock()
	pending := mb.permPending
	mb.permMu.Unlock()
	if pending {
		return false
	}
	if m.SessionBusy != nil && m.SessionBusy(key) {
		return false
	}
	return true
}

// evictExact evicts c's backend if it is still mapped, not mid-creation and
// unused since it was scanned (a Get in between moves lastActive): an idle
// close that keeps the resume ID. The process closes after m.mu is released
// (#749). Reports whether it evicted.
func (m *DelegatedManager) evictExact(c evictCandidate, capLabel string) bool {
	return m.closeIdleExact(c, func() {
		m.logger().Infof("evicting idle session %s to make room (%s full; idle %s)", c.key, capLabel, time.Since(c.lastSeen).Round(time.Second))
	})
}

// closeIdleExact closes c's backend if it is still mapped, not mid-creation
// and unused since it was scanned, keeping its resume ID so the next turn
// resumes the same conversation. logf runs once the backend is unmapped,
// before it closes.
func (m *DelegatedManager) closeIdleExact(c evictCandidate, logf func()) bool {
	m.mu.Lock()
	mb, ok := m.backends[c.key]
	if !ok || mb != c.mb || mb.creating.Load() || !mb.lastActive.Equal(c.seenAt) {
		m.mu.Unlock()
		return false
	}
	delete(m.backends, c.key)
	m.saveResumeID(c.key, mb.be.SessionID())
	m.mu.Unlock()

	logf()
	m.closeRemoved(c.key, mb)
	return true
}

// CloseFinishedSession closes sessionKey's backend now that the work it ran
// for is over (a background spawn's run ended), instead of leaving it to the
// idle reaper (idle_timeout, 3h by default). It keeps the resume ID, so a
// later turn (spawn_send to the finished spawn) resumes the same conversation
// exactly as after an idle close.
//
// The backend must be idle by the eviction rule (evictable): no turn, no
// background work (subagents, background shells, an autonomous run they
// would trigger), no pending prompt, no compaction. A backend that is not
// is left for the idle reaper. Reports whether it closed one.
//
// Why: a codex app-server serves every session of an agent and keeps each
// thread it has loaded open (its rollout and writer lock) until the session's
// facade closes and releases it (codex release.go). A finished spawn's
// branch that stayed open for 3h held its thread loaded for 3h.
func (m *DelegatedManager) CloseFinishedSession(sessionKey, reason string) bool {
	m.mu.Lock()
	mb, ok := m.backends[sessionKey]
	if !ok || mb.creating.Load() {
		m.mu.Unlock()
		return false
	}
	c := evictCandidate{m: m, key: sessionKey, mb: mb, seenAt: mb.lastActive}
	m.mu.Unlock()
	if !m.evictable(sessionKey, mb) {
		m.logger().Infof("finished session %s (%s) is not idle (a turn, background work, a prompt or compaction); left for the idle reaper", sessionKey, reason)
		return false
	}
	return m.closeIdleExact(c, func() {
		m.logger().Infof("closing finished session %s (%s; session %s)", sessionKey, reason, mb.be.SessionID())
	})
}

// closeRemoved tears down a backend that has already been removed from the
// map: clears its permission state, closes the process and bridge, takes down
// its prompts and frees its live-session place. Never called with m.mu held.
func (m *DelegatedManager) closeRemoved(sessionKey string, mb *managedBackend) {
	mb.clearPermission()
	_ = mb.be.Close()
	if mb.bridge != nil {
		mb.bridge.Close()
	}
	// A prompt the closed backend raised can never be answered now.
	m.ClearSessionPrompts(sessionKey)
	mb.place.release()
}
