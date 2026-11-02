package agent

import (
	"context"
	"sync"
	"time"
)

// Idle eviction of per-session inboxes (#2283). Each session key the agent has
// seen keeps a sessionInbox, a worker goroutine and a 64-slot channel; a
// multi-user transport (one session per user, chat or thread) would grow them
// without bound. A session idle for inboxIdleAfter has its worker stopped, its
// a.inboxes entry dropped and its refilling bookkeeping (turn lock, in-flight
// change channel, expired background-origin stamps) cleared; the next message
// on the key recreates all of it. Neither value is configurable (Dick
// 2026-10-11); they match DefaultIdleTimeout and the delegated idle reaper.
const (
	inboxIdleAfter = 3 * time.Hour
	inboxReapEvery = 10 * time.Minute
)

// inboxIdleThreshold is how long a session must be idle before its inbox is
// evicted. Tests shorten it through Agent.inboxIdleAfter.
func (a *Agent) inboxIdleThreshold() time.Duration {
	if a.inboxIdleAfter > 0 {
		return a.inboxIdleAfter
	}
	return inboxIdleAfter
}

// inboxReapInterval is how often the reaper looks for idle inboxes. Tests
// shorten it through Agent.inboxReapEvery.
func (a *Agent) inboxReapInterval() time.Duration {
	if a.inboxReapEvery > 0 {
		return a.inboxReapEvery
	}
	return inboxReapEvery
}

// touch records that the session was used now: Enqueue accepted an envelope
// for it, or its worker finished a batch or an injection. Read-only lookups
// never call it.
func (inb *sessionInbox) touch() {
	inb.lastUsed.Store(time.Now().UnixNano())
}

// idleFor is how long ago the session was last used.
func (inb *sessionInbox) idleFor() time.Duration {
	return time.Since(time.Unix(0, inb.lastUsed.Load()))
}

// pinInbox returns sk's live inbox (creating it, and its worker, if needed)
// with inb.evictMu read-locked, so it cannot be evicted until the caller has
// written to it and called inb.evictMu.RUnlock. An inbox evicted between the
// map lookup and the pin is never written to: the loop finds or creates its
// replacement instead.
//
// Lock order: a.inboxesMu is released before evictMu is taken here; eviction
// takes evictMu (TryLock, never waiting) and then a.inboxesMu. So the order is
// evictMu → inboxesMu, and neither side ever waits on the other while holding
// a lock: eviction skips a pinned inbox rather than waiting, and a pin waits
// only for an eviction already in progress on that one inbox. Other sessions'
// Enqueues are never blocked.
func (a *Agent) pinInbox(sk string) *sessionInbox {
	for {
		inb := a.getOrCreateInbox(sk)
		if a.enqueueFoundHook != nil {
			a.enqueueFoundHook(sk)
		}
		inb.evictMu.RLock()
		if !inb.evicted {
			return inb
		}
		inb.evictMu.RUnlock()
	}
}

// inboxReaper asks every inbox idle past the threshold to evict itself, every
// inboxReapInterval, until ctx (inboxCtx) ends. It never evicts anything
// itself: the worker does, on its own goroutine, at its idle point, which is
// what makes the final check race-free (tryEvictInbox).
func (a *Agent) inboxReaper(ctx context.Context) {
	t := time.NewTicker(a.inboxReapInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.requestIdleEvictions()
		}
	}
}

// requestIdleEvictions sends an eviction request to every inbox idle past the
// threshold. The request is a hint: a busy worker sees it at its next idle
// point and re-checks everything then.
func (a *Agent) requestIdleEvictions() {
	threshold := a.inboxIdleThreshold()
	a.inboxesMu.Lock()
	candidates := make([]*sessionInbox, 0, len(a.inboxes))
	for _, inb := range a.inboxes {
		if inb.idleFor() >= threshold {
			candidates = append(candidates, inb)
		}
	}
	a.inboxesMu.Unlock()
	for _, inb := range candidates {
		select {
		case inb.evictReq <- struct{}{}:
		default: // one request already pending
		}
	}
}

// tryEvictInbox evicts inb if it is still idle, reporting whether it did. It
// runs on inb's worker at its idle point (the main select, nothing taken off
// the queue), or in tests on an inbox with no worker. The session is NOT
// evicted while:
//   - its channel, steer buffer, worker queue or held list holds an envelope;
//   - it holds a deferred inject;
//   - a turn runs or the worker waits for a turn-limit slot (turnCancel /
//     waitCancel set);
//   - the agent has a turn in flight on it (IsTurnInFlight) or, on a
//     delegated agent, the backend does (BackendTurnInFlight);
//   - it was used again within the threshold (since the reaper's request);
//   - an Enqueue has it pinned (evictMu read-locked): it is about to write.
//
// Under evictMu (exclusive) no Enqueue can write to inb, so a session found
// empty here stays empty: it is marked evicted and dropped from a.inboxes
// before the lock is released, and every later Enqueue pins its replacement.
// A skipped eviction changes nothing.
func (a *Agent) tryEvictInbox(inb *sessionInbox) bool {
	// Worker-confined state first: only the worker (or a test with none)
	// calls this.
	if len(inb.held) > 0 || len(inb.usageHeld) > 0 || inb.usageResume.Load() {
		return false
	}
	inb.pendMu.Lock()
	queued := len(inb.pending)
	inb.pendMu.Unlock()
	if queued > 0 {
		return false
	}
	if !inb.evictMu.TryLock() {
		a.logger().Debugf("inbox: eviction of sk=%s skipped: an enqueue is in progress", inb.sk)
		return false
	}
	idle := inb.idleFor()
	if reason := a.inboxBusyReason(inb, idle); reason != "" {
		inb.evictMu.Unlock()
		a.logger().Debugf("inbox: eviction of sk=%s skipped: %s", inb.sk, reason)
		return false
	}
	inb.evicted = true
	a.inboxesMu.Lock()
	if a.inboxes[inb.sk] == inb {
		delete(a.inboxes, inb.sk)
	}
	a.inboxesMu.Unlock()
	inb.evictMu.Unlock()

	a.dropSessionBookkeeping(inb.sk)
	a.inboxEvictions.Add(1)
	a.logger().Debugf("inbox: evicted idle session sk=%s (idle %s)", inb.sk, idle.Round(time.Millisecond))
	return true
}

// inboxBusyReason names why inb must not be evicted now, or "" when it may be.
// Called with inb.evictMu held exclusively.
func (a *Agent) inboxBusyReason(inb *sessionInbox, idle time.Duration) string {
	if idle < a.inboxIdleThreshold() {
		return "used again"
	}
	if len(inb.ch) > 0 {
		return "envelope queued"
	}
	inb.steerMu.Lock()
	steer := len(inb.steer)
	inb.steerMu.Unlock()
	if steer > 0 {
		return "steer buffered"
	}
	inb.injMu.Lock()
	deferred := len(inb.deferredInjects)
	inb.injMu.Unlock()
	if deferred > 0 {
		return "deferred inject held"
	}
	inb.cancelMu.Lock()
	running := inb.turnCancel != nil || inb.waitCancel != nil
	inb.cancelMu.Unlock()
	if running {
		return "turn running or waiting for a turn slot"
	}
	if a.IsTurnInFlight(inb.sk) {
		return "turn in flight"
	}
	if a.DelegatedManager != nil && a.DelegatedManager.BackendTurnInFlight(inb.sk) {
		return "backend turn in flight"
	}
	return ""
}

// dropSessionBookkeeping clears an evicted session's per-session maps that
// refill on their own when the session is next used (#2283): the turn lock,
// the in-flight change channel and expired background-origin stamps. It keeps
// meta, the delivery routers, sessionEvents, thinking buffers, tool tallies,
// lastTurnEndAt and compacting: those hold live or non-refilling state, and
// AttachDelivery binds the routers and buffers to the live backend once.
func (a *Agent) dropSessionBookkeeping(sk string) {
	a.dropTurnLock(sk)
	a.dropInFlightChanged(sk)
	a.pruneBgOrigins(sk)
}

// dropTurnLock removes sk's turn mutex from the map unless it is held. A
// caller that fetched it before the removal and locks it afterwards finds it
// is no longer the map's entry and retries (lockTurn), so two turns on one key
// never run at once.
func (a *Agent) dropTurnLock(sk string) {
	a.turnLocksMu.Lock()
	defer a.turnLocksMu.Unlock()
	mu, ok := a.turnLocks[sk]
	if !ok || !mu.TryLock() {
		return
	}
	delete(a.turnLocks, sk)
	mu.Unlock()
}

// lockTurn locks sk's turn mutex and returns it. It re-checks, once locked,
// that the mutex is still the map's entry for sk: one removed meanwhile
// (dropTurnLock — eviction, ClearSessionState) is released and the lock is
// taken on the current entry instead. A mutex is only removed while nobody
// holds it, so at most one holder of a current entry exists per key.
func (a *Agent) lockTurn(sk string) *sync.Mutex {
	for {
		mu := a.turnLock(sk)
		if a.turnLockFetchedHook != nil {
			a.turnLockFetchedHook(sk)
		}
		mu.Lock()
		a.turnLocksMu.Lock()
		current := a.turnLocks[sk] == mu
		a.turnLocksMu.Unlock()
		if current {
			return mu
		}
		mu.Unlock()
	}
}

// dropInFlightChanged removes sk's in-flight change channel when no turn is in
// flight on it, closing it first so a goroutine waiting on InFlightWaitCh
// wakes, re-checks and fetches a fresh channel.
func (a *Agent) dropInFlightChanged(sk string) {
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	if a.inFlight[sk] != 0 || a.inFlightDelivering[sk] != 0 {
		return
	}
	if ch, ok := a.inFlightChanged[sk]; ok {
		close(ch)
		delete(a.inFlightChanged, sk)
	}
}

// pruneBgOrigins drops sk's background-origin stamps older than
// bgOriginMaxAge, and the session's entry once no live stamp remains.
func (a *Agent) pruneBgOrigins(sk string) {
	a.bgOriginsMu.Lock()
	defer a.bgOriginsMu.Unlock()
	m, ok := a.bgOrigins[sk]
	if !ok {
		return
	}
	now := time.Now()
	for k, o := range m {
		if now.Sub(o.at) > bgOriginMaxAge {
			delete(m, k)
		}
	}
	if len(m) == 0 {
		delete(a.bgOrigins, sk)
	}
}
