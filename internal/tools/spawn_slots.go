package tools

import (
	"context"
	"sync"
)

// spawnSlots is the counting semaphore that bounds how many spawn runs one
// Spawner has going at once ([tools] max_concurrent_spawns). Unlike the
// buffered channel it replaces, its limit can change while it is in use
// (#2722): raising it starts queued runs at once; lowering it never touches a
// run that already holds a slot, it only holds new ones back until the count
// drops below the new limit. A limit <= 0 lets no run start (they wait).
//
// Waiters are served FIFO, in the order their acquire enqueued: a free slot
// goes to the head waiter, and acquire never takes a slot while anyone waits.
type spawnSlots struct {
	mu      sync.Mutex
	limit   int
	held    int
	waiters []chan struct{}
}

func newSpawnSlots(limit int) *spawnSlots {
	return &spawnSlots{limit: limit}
}

// acquire blocks until it holds a slot (nil) or ctx ends (ctx.Err(), no slot
// held). A grant that races the cancellation wins: the slot is handed back.
func (s *spawnSlots) acquire(ctx context.Context) error {
	if wait := s.reserve(); wait != nil {
		return wait(ctx)
	}
	return nil
}

// reserve takes a slot at once when one is free and nobody waits (nil), or
// joins the FIFO queue now and returns wait, which blocks until the slot is
// granted (nil) or ctx ends (ctx.Err(), no slot held). A background spawn uses
// it to return to its caller at once, queued, without losing its place
// (#3235). Exactly one wait call must follow a non-nil return.
func (s *spawnSlots) reserve() (wait func(ctx context.Context) error) {
	s.mu.Lock()
	if s.held < s.limit && len(s.waiters) == 0 {
		s.held++
		s.mu.Unlock()
		return nil
	}
	grant := make(chan struct{})
	s.waiters = append(s.waiters, grant)
	s.mu.Unlock()
	return func(ctx context.Context) error {
		select {
		case <-grant:
			return nil
		case <-ctx.Done():
			s.mu.Lock()
			for i, w := range s.waiters {
				if w == grant {
					s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
					s.mu.Unlock()
					return ctx.Err()
				}
			}
			s.mu.Unlock()
			// Already granted (dequeued and counted): give the slot back.
			<-grant
			s.release()
			return ctx.Err()
		}
	}
}

// release frees a slot and hands any room to the head waiters.
func (s *spawnSlots) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held > 0 {
		s.held--
	}
	s.grantLocked()
}

// setLimit changes the limit for the next acquisitions; a raise wakes as many
// queued waiters as now fit.
func (s *spawnSlots) setLimit(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limit = n
	s.grantLocked()
}

// grantLocked hands free slots to waiters, head first. The caller holds s.mu.
func (s *spawnSlots) grantLocked() {
	for s.held < s.limit && len(s.waiters) > 0 {
		head := s.waiters[0]
		s.waiters = s.waiters[1:]
		s.held++
		close(head)
	}
}

// counts reports (held, waiting, limit), for tests and status.
func (s *spawnSlots) counts() (held, waiting, limit int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held, len(s.waiters), s.limit
}
