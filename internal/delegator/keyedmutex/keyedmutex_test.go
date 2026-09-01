package keyedmutex

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestExclusivePerKey: the property the whole package exists for. Two callers on
// the same key must never be inside the critical section together — that overlap
// IS the double-spawn of #1718.
func TestExclusivePerKey(t *testing.T) {
	var m Map
	var inside, maxInside int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := m.Lock("agent")
			defer unlock()
			n := atomic.AddInt32(&inside, 1)
			for {
				old := atomic.LoadInt32(&maxInside)
				if n <= old || atomic.CompareAndSwapInt32(&maxInside, old, n) {
					break
				}
			}
			time.Sleep(time.Millisecond) // widen the window a real spawn would occupy
			atomic.AddInt32(&inside, -1)
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&maxInside); got != 1 {
		t.Fatalf("max concurrent holders of one key = %d, want 1", got)
	}
}

// TestDifferentKeysDoNotBlock: the reason this is keyed rather than one global
// mutex held across the spawn. opencode's Start() was measured at 13s with no
// deadline, so serialising unrelated agents behind it is not an option.
func TestDifferentKeysDoNotBlock(t *testing.T) {
	var m Map
	held := make(chan struct{})
	done := make(chan struct{})
	go func() {
		unlock := m.Lock("agent-a")
		close(held)
		<-done // hold it open
		unlock()
	}()
	<-held

	got := make(chan struct{})
	go func() {
		unlock := m.Lock("agent-b") // must NOT wait on agent-a
		unlock()
		close(got)
	}()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		close(done)
		t.Fatal("a second key blocked behind the first — the lock is not keyed")
	}
	close(done)
}

// TestEntriesAreReaped: an unbounded map keyed by agent id would be a slow leak.
// Held() is the tell.
func TestEntriesAreReaped(t *testing.T) {
	var m Map
	for i := 0; i < 100; i++ {
		unlock := m.Lock("agent")
		unlock()
	}
	if got := held(&m); got != 0 {
		t.Fatalf("Held() = %d after all unlocks, want 0", got)
	}
	unlock := m.Lock("agent")
	if got := held(&m); got != 1 {
		t.Errorf("Held() = %d while holding, want 1", got)
	}
	unlock()
}

// TestUnlockIsIdempotent: a defer that also runs on a panic path must not
// double-unlock into a "sync: unlock of unlocked mutex" panic, which would
// surface as an unrelated crash far from the real fault.
func TestUnlockIsIdempotent(t *testing.T) {
	var m Map
	unlock := m.Lock("agent")
	unlock()
	unlock()
	if got := held(&m); got != 0 {
		t.Fatalf("Held() = %d after a double unlock, want 0", got)
	}
}

// TestReapRace: the entry is refcounted BEFORE the map lock is released, so a
// concurrent unlock cannot delete it out from under an incoming Lock. If it
// could, two callers would hold DIFFERENT mutexes for the same key and the
// exclusion above would silently stop holding — with no symptom until two
// subprocesses collided. Run under -race; the assertion is exclusion itself.
func TestReapRace(t *testing.T) {
	var m Map
	var inside, maxInside int32
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := m.Lock("churn") // same key, constantly reaped and recreated
			n := atomic.AddInt32(&inside, 1)
			for {
				old := atomic.LoadInt32(&maxInside)
				if n <= old || atomic.CompareAndSwapInt32(&maxInside, old, n) {
					break
				}
			}
			atomic.AddInt32(&inside, -1)
			unlock()
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&maxInside); got != 1 {
		t.Fatalf("max concurrent holders under reap churn = %d, want 1", got)
	}
	if got := held(&m); got != 0 {
		t.Errorf("Held() = %d after churn, want 0", got)
	}
}

// held reports the live entry count. Unexported and test-local on purpose: an
// exported accessor would be app-unreachable API existing only for tests.
func held(m *Map) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.m)
}
