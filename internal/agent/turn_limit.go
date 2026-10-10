package agent

import (
	"context"
	"sync"
)

// turnQueuedNotice is the exact text sent to a session's chat when its user
// turn has to wait for a turn-limit slot (#2281). One notice per wait, fired
// before blocking; messages that join an already-waiting batch do not re-fire it.
const turnQueuedNotice = "⏳ Busy right now. Your message is queued and will run when a slot frees up."

// TurnLimiter is a counting semaphore of size N with FIFO waiters, capping how
// many user turns run at once (#2281). A nil *TurnLimiter means unlimited:
// every method is nil-receiver safe (TryAcquire/Acquire return true, Release
// is a no-op, Size reports 0), so callers need no nil branches — a limit of 0
// builds no limiter at all and therefore never waits, never notifies, never
// logs.
//
// Waiters are served in the order their Acquire reaches the semaphore (the
// moment it enqueues), which is the practical meaning of "first come, first
// served" here: two sessions that begin waiting microseconds apart may enqueue
// in either order. Release hands a slot directly to the head waiter rather
// than decrementing, so a later TryAcquire can never jump the queue.
//
// Not live-resizable: the limit sizes the semaphore at construction, like
// [tools] max_concurrent_spawns — changing it requires a restart.
type TurnLimiter struct {
	limit   int
	mu      sync.Mutex
	held    int
	waiters []chan struct{}
}

// NewTurnLimiter builds a limiter allowing n concurrent holders. n <= 0 means
// no limit and returns nil (see TurnLimiter for the nil semantics).
func NewTurnLimiter(n int) *TurnLimiter {
	if n <= 0 {
		return nil
	}
	return &TurnLimiter{limit: n}
}

// Size returns the configured limit; 0 when the limiter is nil (unlimited).
func (l *TurnLimiter) Size() int {
	if l == nil {
		return 0
	}
	return l.limit
}

// TryAcquire takes a slot without waiting. It reports false when the limit is
// reached OR any waiter is queued — a fast path must never jump a waiter that
// blocked first, or FIFO would be a fiction under load.
func (l *TurnLimiter) TryAcquire() bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held < l.limit && len(l.waiters) == 0 {
		l.held++
		return true
	}
	return false
}

// Acquire blocks until a slot is available or ctx ends. It reports true when
// the caller holds a slot (and must Release it) — including the case where the
// grant and the ctx cancellation race and the grant wins. It reports false
// only when the wait was abandoned without a slot being taken.
func (l *TurnLimiter) Acquire(ctx context.Context) bool {
	if l == nil {
		return true
	}
	if l.TryAcquire() {
		return true
	}
	grant := make(chan struct{})
	l.mu.Lock()
	l.waiters = append(l.waiters, grant)
	l.mu.Unlock()
	select {
	case <-grant:
		return true
	case <-ctx.Done():
		// The grant may have landed concurrently with the cancel. Re-check
		// under the mutex: a granted waiter has already been dequeued, so
		// finding ourselves still queued means we can safely leave; finding
		// the grant closed means we own a slot and must hand it back here —
		// the caller cannot know to Release what it was told it did not get.
		l.mu.Lock()
		for i, w := range l.waiters {
			if w == grant {
				l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)
				l.mu.Unlock()
				return false
			}
		}
		l.mu.Unlock()
		select {
		case <-grant:
			l.Release()
		default:
		}
		return false
	}
}

// Release frees a slot. If waiters are queued, the slot is handed to the head
// waiter (its grant channel is closed; held stays unchanged) instead of
// returning to the pool — preserving FIFO service.
func (l *TurnLimiter) Release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.waiters) > 0 {
		head := l.waiters[0]
		l.waiters = l.waiters[1:]
		close(head)
		return
	}
	if l.held > 0 {
		l.held--
	}
}
