package ccstream

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestOutstandingRegistry_RegisterAndHas(t *testing.T) {
	r := NewOutstandingRegistry()
	if r.Has("req1") {
		t.Error("empty registry should not have req1")
	}

	r.Register("req1", OutstandingPermission)
	if !r.Has("req1") {
		t.Error("registry should have req1 after Register")
	}
	if r.Len() != 1 {
		t.Errorf("Len() = %d, want 1", r.Len())
	}
}

func TestOutstandingRegistry_Resolve(t *testing.T) {
	r := NewOutstandingRegistry()
	r.Register("req1", OutstandingPermission)

	if !r.Resolve("req1") {
		t.Error("Resolve should return true for registered prompt")
	}
	if r.Has("req1") {
		t.Error("Has should return false after Resolve")
	}
	if r.Resolve("req1") {
		t.Error("Resolve should return false for already-removed prompt")
	}
}

// TestOutstandingRegistry_ResolveDoesNotFireCancelListeners proves that
// listeners only fire on Cancel, never on Resolve.
func TestOutstandingRegistry_ResolveDoesNotFireCancelListeners(t *testing.T) {
	r := NewOutstandingRegistry()
	r.Register("req1", OutstandingPermission)

	var fired int32
	r.AddCancelListener("req1", func(reason string) {
		atomic.AddInt32(&fired, 1)
	})

	r.Resolve("req1")
	if n := atomic.LoadInt32(&fired); n != 0 {
		t.Errorf("cancel listener fired %d times on Resolve, want 0", n)
	}
}

// TestOutstandingRegistry_CancelFiresListeners proves that listeners run in
// registration order with the cancel reason.
func TestOutstandingRegistry_CancelFiresListeners(t *testing.T) {
	r := NewOutstandingRegistry()
	r.Register("req1", OutstandingPermission)

	var order []string
	var mu sync.Mutex
	r.AddCancelListener("req1", func(reason string) {
		mu.Lock()
		order = append(order, "first:"+reason)
		mu.Unlock()
	})
	r.AddCancelListener("req1", func(reason string) {
		mu.Lock()
		order = append(order, "second:"+reason)
		mu.Unlock()
	})

	if !r.Cancel("req1", "tool_aborted") {
		t.Error("Cancel should return true for registered prompt")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 {
		t.Fatalf("listeners fired %d times, want 2", len(order))
	}
	if order[0] != "first:tool_aborted" {
		t.Errorf("order[0] = %q, want %q", order[0], "first:tool_aborted")
	}
	if order[1] != "second:tool_aborted" {
		t.Errorf("order[1] = %q, want %q", order[1], "second:tool_aborted")
	}
}

// TestOutstandingRegistry_CancelMissing proves that cancelling an unknown
// requestID is a no-op (no panic, returns false).
func TestOutstandingRegistry_CancelMissing(t *testing.T) {
	r := NewOutstandingRegistry()
	if r.Cancel("nope", "reason") {
		t.Error("Cancel should return false for unregistered requestID")
	}
}

// TestOutstandingRegistry_AddCancelListenerWithoutRegister proves that
// adding a listener for an unknown requestID is silently dropped.
func TestOutstandingRegistry_AddCancelListenerWithoutRegister(t *testing.T) {
	r := NewOutstandingRegistry()

	var fired int32
	r.AddCancelListener("nope", func(reason string) {
		atomic.AddInt32(&fired, 1)
	})

	// No prompt registered, so Cancel returns false and listener is never run.
	r.Cancel("nope", "reason")
	if n := atomic.LoadInt32(&fired); n != 0 {
		t.Errorf("dropped listener fired %d times, want 0", n)
	}
}

// TestOutstandingRegistry_OnEmptyFiresAfterLastResolve proves that the
// registry-wide drain hook fires when the last prompt is resolved.
func TestOutstandingRegistry_OnEmptyFiresAfterLastResolve(t *testing.T) {
	r := NewOutstandingRegistry()
	var fired int32
	r.SetOnEmpty(func() { atomic.AddInt32(&fired, 1) })

	r.Register("req1", OutstandingPermission)
	r.Register("req2", OutstandingElicitation)

	r.Resolve("req1")
	if n := atomic.LoadInt32(&fired); n != 0 {
		t.Errorf("onEmpty fired %d times after first resolve, want 0", n)
	}

	r.Resolve("req2")
	if n := atomic.LoadInt32(&fired); n != 1 {
		t.Errorf("onEmpty fired %d times after last resolve, want 1", n)
	}
}

// TestOutstandingRegistry_OnEmptyFiresAfterCancel proves that Cancel also
// triggers the drain hook when it empties the registry.
func TestOutstandingRegistry_OnEmptyFiresAfterCancel(t *testing.T) {
	r := NewOutstandingRegistry()
	var fired int32
	r.SetOnEmpty(func() { atomic.AddInt32(&fired, 1) })

	r.Register("req1", OutstandingPermission)
	r.Cancel("req1", "tool_aborted")

	if n := atomic.LoadInt32(&fired); n != 1 {
		t.Errorf("onEmpty fired %d times after cancel, want 1", n)
	}
}

// TestOutstandingRegistry_OnEmptyDoesNotFireOnEmptyResolve proves that
// resolving a missing requestID does not fire onEmpty (idempotent semantic —
// no actual state change).
func TestOutstandingRegistry_OnEmptyDoesNotFireOnEmptyResolve(t *testing.T) {
	r := NewOutstandingRegistry()
	var fired int32
	r.SetOnEmpty(func() { atomic.AddInt32(&fired, 1) })

	r.Resolve("nope")
	if n := atomic.LoadInt32(&fired); n != 0 {
		t.Errorf("onEmpty fired %d times on no-op resolve, want 0", n)
	}
}

// TestOutstandingRegistry_OnEmptyMixedKinds proves that onEmpty fires only
// when both permission and elicitation prompts are gone — fixing the
// pre-Phase-2 asymmetry where removePendingPerm checked only the perms map
// while removePendingElicit checked both.
func TestOutstandingRegistry_OnEmptyMixedKinds(t *testing.T) {
	r := NewOutstandingRegistry()
	var fired int32
	r.SetOnEmpty(func() { atomic.AddInt32(&fired, 1) })

	r.Register("perm1", OutstandingPermission)
	r.Register("elic1", OutstandingElicitation)

	// Resolving the perm should not fire onEmpty — elic still outstanding.
	r.Resolve("perm1")
	if n := atomic.LoadInt32(&fired); n != 0 {
		t.Errorf("onEmpty fired %d times with elic still outstanding, want 0", n)
	}

	r.Resolve("elic1")
	if n := atomic.LoadInt32(&fired); n != 1 {
		t.Errorf("onEmpty fired %d times after both resolved, want 1", n)
	}
}

// TestOutstandingRegistry_RegisterReplacesListeners proves that re-registering
// the same requestID drops previously-attached listeners. (Defensive — the
// real call paths shouldn't double-register, but the semantic should be safe.)
func TestOutstandingRegistry_RegisterReplacesListeners(t *testing.T) {
	r := NewOutstandingRegistry()
	r.Register("req1", OutstandingPermission)

	var fired int32
	r.AddCancelListener("req1", func(reason string) {
		atomic.AddInt32(&fired, 1)
	})

	// Re-register: previous listener is dropped.
	r.Register("req1", OutstandingPermission)
	r.Cancel("req1", "reason")

	if n := atomic.LoadInt32(&fired); n != 0 {
		t.Errorf("dropped listener fired %d times after re-register, want 0", n)
	}
}

// TestOutstandingRegistry_Concurrent stresses the lock with concurrent
// Register/Resolve/Cancel/AddCancelListener to catch races (run with -race).
func TestOutstandingRegistry_Concurrent(t *testing.T) {
	r := NewOutstandingRegistry()
	var wg sync.WaitGroup
	const goroutines = 32
	const iterations = 100

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				reqID := string(rune('a'+id)) + ":" + string(rune('0'+i%10))
				r.Register(reqID, OutstandingPermission)
				r.AddCancelListener(reqID, func(reason string) {})
				if i%2 == 0 {
					r.Resolve(reqID)
				} else {
					r.Cancel(reqID, "race")
				}
			}
		}(g)
	}
	wg.Wait()

	// After all goroutines finish, registry should be empty (every register
	// is followed by exactly one resolve/cancel within the same goroutine).
	if !r.IsEmpty() {
		t.Errorf("registry not empty after concurrent run: Len=%d", r.Len())
	}
}
