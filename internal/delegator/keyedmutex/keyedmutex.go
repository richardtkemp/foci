// Package keyedmutex provides mutual exclusion per string key.
//
// It exists for one job: making "use the existing server for this agent, or
// start one if none exists" ATOMIC (#1718). Both pooled backends had that logic
// spread across two critical sections — check the pool under a lock, release it,
// spawn, re-take it to insert — so a second caller arriving in the gap saw an
// empty slot, correctly concluded none existed, and started a second subprocess.
// Both then opened the same SQLite store and one died with "database is locked".
//
// Serialisation, NOT deduplication, and that distinction is the whole reason
// this is not golang.org/x/sync/singleflight (already a dependency, already used
// by DelegatedManager.createGroup — but keyed per sessionKey, which is the wrong
// grain here). singleflight hands the second caller the first's return value
// WITHOUT running its function, so the second caller would never run its own
// refCount++ and the pool's refcount would undercount by one per deduplicated
// acquire. A keyed mutex instead lets every caller run the whole existing
// acquire body, one at a time per agent: the second finds a live pooled server
// and takes the ordinary attach path. Nothing about refcounting changes.
//
// Why not one global mutex held across the spawn: the pool mutex is shared by
// every agent, and opencode's Start() is a health probe measured at 13s with
// NO deadline (it is passed context.Background(), and healthProbe returns only
// on healthy or subprocess death). One agent whose subprocess is alive but never
// healthy would block every other agent's acquire forever.
package keyedmutex

import "sync"

// Map is a set of mutexes keyed by string. The zero value is ready to use.
// Entries are reaped when the last holder/waiter releases, so a long-lived Map
// keyed by something unbounded does not grow without limit.
type Map struct {
	mu sync.Mutex
	m  map[string]*entry
}

type entry struct {
	mu   sync.Mutex
	refs int // holders + waiters; the entry is reaped when this reaches 0
}

// Lock acquires the mutex for key and returns its unlock function. The returned
// function must be called exactly once — deferring it at the call site is the
// intended use:
//
//	defer km.Lock(agentID)()
//
// Blocking here is the point: a second caller for the same key waits for the
// first to finish rather than racing it. Different keys never contend.
func (m *Map) Lock(key string) (unlock func()) {
	m.mu.Lock()
	if m.m == nil {
		m.m = make(map[string]*entry)
	}
	e, ok := m.m[key]
	if !ok {
		e = &entry{}
		m.m[key] = e
	}
	// Counted BEFORE releasing the map lock, so a concurrent unlock cannot reap
	// this entry out from under us and leave two callers holding different
	// mutexes for the same key — which would silently restore the bug this
	// package exists to fix, with no symptom until two subprocesses collided.
	e.refs++
	m.mu.Unlock()

	e.mu.Lock()

	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Unlock()
			m.mu.Lock()
			e.refs--
			if e.refs == 0 {
				delete(m.m, key)
			}
			m.mu.Unlock()
		})
	}
}

// NOTE: there is deliberately no exported accessor for the entry count. The
// reaping tests live in this package and read len(m.m) directly (see held() in
// keyedmutex_test.go); exporting it would add API that only tests use, which the
// repo's deadcode gate correctly rejects.
