package agent

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"foci/internal/session"
)

// testBaseA is a representative root session key for in-flight tests:
// {agentID}/{type}{id}. Branches/sub-agents have their own child keys; the
// in-flight counter is keyed by the session key as-is.
const (
	testBaseA = "test-agent/cTEST"
	testBaseB = "test-agent/cOTHER"
)

// TestInFlight_CounterReflectsState verifies IsTurnInFlight(base) reads the
// underlying map — true when one or more turns are tracked for that base,
// false when none are.
func TestInFlight_CounterReflectsState(t *testing.T) {
	a := &Agent{}

	if a.IsTurnInFlight(testBaseA) {
		t.Fatalf("IsTurnInFlight on fresh Agent: got true, want false")
	}

	done := a.markInFlight(testBaseA, true)
	if !a.IsTurnInFlight(testBaseA) {
		t.Fatalf("after markInFlight(%s): got false, want true", testBaseA)
	}

	done()
	if a.IsTurnInFlight(testBaseA) {
		t.Fatalf("after done(): got true, want false")
	}
}

// TestInFlight_DoneIdempotent verifies that calling the decrement closure
// multiple times only decrements once. Defensive guard against accidental
// double-defer in some future call site.
func TestInFlight_DoneIdempotent(t *testing.T) {
	a := &Agent{}
	done := a.markInFlight(testBaseA, true)
	done()
	done()
	done()
	if a.IsTurnInFlight(testBaseA) {
		t.Fatalf("after triple done(): IsTurnInFlight = true, want false")
	}
}

// TestInFlight_MultipleConcurrentTurnsSameBase verifies that nested or
// concurrent markInFlight calls under the same base accumulate and unwind
// cleanly. Each closure is independent — order of done() calls doesn't
// matter.
func TestInFlight_MultipleConcurrentTurnsSameBase(t *testing.T) {
	a := &Agent{}
	const N = 10
	var dones []func()
	for i := 0; i < N; i++ {
		// Alternate delivering/non-delivering so the test exercises both
		// counters under nested marks on the same base.
		dones = append(dones, a.markInFlight(testBaseA, i%2 == 0))
	}
	if !a.IsTurnInFlight(testBaseA) {
		t.Fatalf("after %d markInFlight: IsTurnInFlight returned false", N)
	}

	// Drain in reverse order.
	for i := len(dones) - 1; i >= 0; i-- {
		dones[i]()
	}
	if a.IsTurnInFlight(testBaseA) {
		t.Fatalf("after draining: IsTurnInFlight returned true")
	}
}

// TestInFlight_DistinctBases verifies that the counter is per-base —
// activity under one session base does not leak into IsTurnInFlight reports
// for another base.
func TestInFlight_DistinctBases(t *testing.T) {
	a := &Agent{}

	doneA := a.markInFlight(testBaseA, true)
	if !a.IsTurnInFlight(testBaseA) {
		t.Fatalf("IsTurnInFlight(%s) after mark: got false, want true", testBaseA)
	}
	if a.IsTurnInFlight(testBaseB) {
		t.Fatalf("IsTurnInFlight(%s) leaked from %s mark: got true, want false", testBaseB, testBaseA)
	}

	doneB := a.markInFlight(testBaseB, true)
	if !a.IsTurnInFlight(testBaseB) {
		t.Fatalf("IsTurnInFlight(%s) after mark: got false, want true", testBaseB)
	}

	doneA()
	if a.IsTurnInFlight(testBaseA) {
		t.Fatalf("IsTurnInFlight(%s) after release: got true, want false", testBaseA)
	}
	if !a.IsTurnInFlight(testBaseB) {
		t.Fatalf("IsTurnInFlight(%s) wrongly cleared by %s release", testBaseB, testBaseA)
	}

	doneB()
	if a.IsTurnInFlight(testBaseB) {
		t.Fatalf("IsTurnInFlight(%s) after release: got true, want false", testBaseB)
	}
}

// TestInFlight_FacetDecoupledFromRoot is the #719 regression: a facet/branch
// ('b' child, its own backend, an independent conversation) must track in-flight
// status separately from its parent root. A busy facet must NOT make the root
// read as in-flight (which would suppress the root's keepalive/reflection),
// while a root-injected periodic turn (runs under the parent key, no child)
// must still register under the root identity.
func TestInFlight_FacetDecoupledFromRoot(t *testing.T) {
	a := &Agent{}

	root := "test-agent/cTEST"
	facet := "test-agent/cTEST/b1700050000"

	// A turn on the facet must not couple the root.
	doneFacet := a.markInFlight(facet, true)
	if !a.IsTurnInFlight(facet) {
		t.Fatalf("IsTurnInFlight(facet=%s) after mark: got false, want true", facet)
	}
	if a.IsTurnInFlight(root) {
		t.Fatalf("#719 coupling: facet turn made root=%s read in-flight; must stay false", root)
	}

	// A root-injected periodic turn (parent key, no child) registers on root.
	doneRoot := a.markInFlight(root, false)
	if !a.IsTurnInFlight(root) {
		t.Fatalf("IsTurnInFlight(root=%s) after mark: got false, want true", root)
	}

	// Releasing the facet must not clear the root.
	doneFacet()
	if a.IsTurnInFlight(facet) {
		t.Fatalf("IsTurnInFlight(facet) after release: got true, want false")
	}
	if !a.IsTurnInFlight(root) {
		t.Fatalf("releasing facet wrongly cleared root in-flight")
	}

	doneRoot()
	if a.IsTurnInFlight(root) {
		t.Fatalf("IsTurnInFlight(root) after release: got true, want false")
	}
}

// TestInFlight_RaceSafe runs concurrent mark/done pairs across two bases to
// verify the map-keyed counter doesn't drift under -race. Each goroutine
// increments and decrements once; each base must end with no in-flight.
func TestInFlight_RaceSafe(t *testing.T) {
	a := &Agent{}
	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		base := testBaseA
		if i%2 == 0 {
			base = testBaseB
		}
		// Mix delivering/non-delivering across goroutines so the parallel
		// inFlightDelivering counter is exercised alongside inFlight.
		delivering := i%3 != 0
		go func(b string, d bool) {
			defer wg.Done()
			done := a.markInFlight(b, d)
			time.Sleep(time.Millisecond)
			done()
		}(base, delivering)
	}
	wg.Wait()
	if a.IsTurnInFlight(testBaseA) {
		t.Fatalf("after %d concurrent mark/done pairs: IsTurnInFlight(%s) = true, want false", N, testBaseA)
	}
	if a.IsTurnInFlight(testBaseB) {
		t.Fatalf("after %d concurrent mark/done pairs: IsTurnInFlight(%s) = true, want false", N, testBaseB)
	}
	if a.IsInFlightDelivering(testBaseA) {
		t.Fatalf("after %d concurrent mark/done pairs: IsInFlightDelivering(%s) = true, want false", N, testBaseA)
	}
	if a.IsInFlightDelivering(testBaseB) {
		t.Fatalf("after %d concurrent mark/done pairs: IsInFlightDelivering(%s) = true, want false", N, testBaseB)
	}
}

// TestInFlight_DeliveringSeparate verifies the delivering counter tracks
// only delivering marks, with the total counter incremented either way.
func TestInFlight_DeliveringSeparate(t *testing.T) {
	a := &Agent{}

	if a.IsInFlightDelivering(testBaseA) {
		t.Fatalf("IsInFlightDelivering on fresh Agent: got true, want false")
	}

	// Non-delivering mark first — total goes up, delivering does not.
	doneNop := a.markInFlight(testBaseA, false)
	if !a.IsTurnInFlight(testBaseA) {
		t.Fatalf("after markInFlight(_, false): IsTurnInFlight = false, want true")
	}
	if a.IsInFlightDelivering(testBaseA) {
		t.Fatalf("after markInFlight(_, false): IsInFlightDelivering = true, want false")
	}

	// Layer a delivering mark on top — both counters go up.
	doneDeliv := a.markInFlight(testBaseA, true)
	if !a.IsTurnInFlight(testBaseA) {
		t.Fatalf("after layered delivering mark: IsTurnInFlight = false, want true")
	}
	if !a.IsInFlightDelivering(testBaseA) {
		t.Fatalf("after layered delivering mark: IsInFlightDelivering = false, want true")
	}

	// Drop the delivering mark — total still positive (nop mark held),
	// delivering returns to false.
	doneDeliv()
	if !a.IsTurnInFlight(testBaseA) {
		t.Fatalf("after delivering done: IsTurnInFlight = false, want true (nop still held)")
	}
	if a.IsInFlightDelivering(testBaseA) {
		t.Fatalf("after delivering done: IsInFlightDelivering = true, want false")
	}

	doneNop()
	if a.IsTurnInFlight(testBaseA) {
		t.Fatalf("after both done: IsTurnInFlight = true, want false")
	}
}

// TestInFlight_WaitChClosesOnChange verifies that InFlightWaitCh returns a
// channel that closes on the next state change (mark or done) for that base,
// and that a fresh channel is installed afterwards.
func TestInFlight_WaitChClosesOnChange(t *testing.T) {
	a := &Agent{}

	ch := a.InFlightWaitCh(testBaseA)
	select {
	case <-ch:
		t.Fatalf("InFlightWaitCh closed before any state change")
	default:
	}

	done := a.markInFlight(testBaseA, false)
	select {
	case <-ch:
		// expected — mark closes the existing waiter.
	case <-time.After(time.Second):
		t.Fatalf("InFlightWaitCh did not close after markInFlight")
	}

	// A new fetch must return a fresh, still-open channel.
	ch2 := a.InFlightWaitCh(testBaseA)
	select {
	case <-ch2:
		t.Fatalf("replacement channel closed prematurely")
	default:
	}

	done()
	select {
	case <-ch2:
		// expected — done closes the new waiter too.
	case <-time.After(time.Second):
		t.Fatalf("replacement InFlightWaitCh did not close after done()")
	}
}

// TestInFlight_WaitChPerBase verifies that a state change on one base does
// not wake waiters on another base.
func TestInFlight_WaitChPerBase(t *testing.T) {
	a := &Agent{}

	chA := a.InFlightWaitCh(testBaseA)
	chB := a.InFlightWaitCh(testBaseB)

	doneA := a.markInFlight(testBaseA, true)
	defer doneA()

	select {
	case <-chA:
		// expected
	case <-time.After(time.Second):
		t.Fatalf("chA did not close after markInFlight(testBaseA)")
	}
	select {
	case <-chB:
		t.Fatalf("chB closed on testBaseA state change — bases leaked")
	case <-time.After(50 * time.Millisecond):
		// expected — no change on testBaseB.
	}
}

// seedCacheTestRow inserts a minimal session_index row so TouchCacheTouch (an
// UPDATE) has something to write to.
func seedCacheTestRow(idx *session.SessionIndex, key string) {
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  key,
		CreatedAt:   time.Now().Add(-time.Hour),
		SessionType: session.ClassifySessionKey(key),
		Status:      session.SessionStatusActive,
	})
}

// TestTouchCacheFreshness_WritesRow verifies the helper stamps last_cache_touch
// on the session's own row within ±2s of now.
func TestTouchCacheFreshness_WritesRow(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{AgentID: "test-agent", SessionIndex: idx}
	seedCacheTestRow(idx, testBaseA)

	before := time.Now().Add(-2 * time.Second)
	a.touchCacheFreshness(testBaseA)
	after := time.Now().Add(2 * time.Second)

	got, ok := idx.LastCacheTouch(testBaseA)
	if !ok {
		t.Fatalf("last_cache_touch not written for %s", testBaseA)
	}
	if got.Before(before) || got.After(after) {
		t.Fatalf("last_cache_touch %v outside window [%v, %v]", got, before, after)
	}
}

// TestTouchCacheFreshness_Monotonic verifies repeated calls keep advancing the
// timestamp (never go backwards).
func TestTouchCacheFreshness_Monotonic(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{AgentID: "test-agent", SessionIndex: idx}
	seedCacheTestRow(idx, testBaseA)

	a.touchCacheFreshness(testBaseA)
	first, ok := idx.LastCacheTouch(testBaseA)
	if !ok {
		t.Fatalf("first cache touch not written")
	}

	time.Sleep(1100 * time.Millisecond)
	a.touchCacheFreshness(testBaseA)
	second, ok := idx.LastCacheTouch(testBaseA)
	if !ok {
		t.Fatalf("second cache touch not written")
	}
	if second.Before(first) {
		t.Fatalf("second cache touch %v before first %v (not monotonic)", second, first)
	}
}

// TestTouchCacheFreshness_BranchBumpsRoot verifies that a branch turn stamps
// BOTH its own row and its root's row — the branch shares (and thus warms) the
// root's cached prefix.
func TestTouchCacheFreshness_BranchIsOwnKeyOnly(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{AgentID: "test-agent", SessionIndex: idx}
	branchKey := testBaseA + "/b1700000000"
	seedCacheTestRow(idx, testBaseA)
	seedCacheTestRow(idx, branchKey)

	a.touchCacheFreshness(branchKey)

	if _, ok := idx.LastCacheTouch(branchKey); !ok {
		t.Fatalf("branch %s cache touch not written", branchKey)
	}
	// A branch TURN must not warm root — root is warmed only at branch creation
	// (TouchRootCacheForBranch).
	if _, ok := idx.LastCacheTouch(testBaseA); ok {
		t.Fatalf("root %s cache touch wrongly written by a branch turn", testBaseA)
	}
}

// TestTouchCacheFreshness_NoSessionIndex verifies the helper is safe when
// SessionIndex is nil (test agents, partially-constructed Agents).
func TestTouchCacheFreshness_NoSessionIndex(t *testing.T) {
	a := &Agent{AgentID: "test-agent"}
	a.touchCacheFreshness(testBaseA) // must not panic
}

// TestTouchCacheFreshness_NoRow verifies the helper is a harmless no-op when
// the session row doesn't exist (UPDATE affects 0 rows).
func TestTouchCacheFreshness_NoRow(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{AgentID: "test-agent", SessionIndex: idx}
	a.touchCacheFreshness(testBaseA)

	if _, ok := idx.LastCacheTouch(testBaseA); ok {
		t.Fatalf("cache touch written for %s despite no row existing", testBaseA)
	}
}
