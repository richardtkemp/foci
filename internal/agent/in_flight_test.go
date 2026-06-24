package agent

import (
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"foci/internal/session"
)

// testBaseA is a representative SessionKeyBase for in-flight tests:
// {agentID}/{type}{id}. Branches/sub-agents would extend this; the in-flight
// counter is keyed by exactly this base.
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

	root := "test-agent/cTEST/1700000000"
	facet := "test-agent/cTEST/1700000000/b1700050000"
	rotatedRoot := "test-agent/cTEST/1700100000" // post-compaction version of root

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
	// And a post-compaction version of the root shares the root identity.
	if !a.IsTurnInFlight(rotatedRoot) {
		t.Fatalf("rotated root %s must share in-flight identity with %s", rotatedRoot, root)
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

// TestTouchLastActivity_WritesRow verifies the helper writes the row keyed
// by session base and the timestamp parses as a unix epoch within ±2s of
// time.Now.
func TestTouchLastActivity_WritesRow(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{
		AgentID:      "test-agent",
		SessionIndex: idx,
	}

	before := time.Now().Unix()
	a.touchLastActivity(testBaseA)
	after := time.Now().Unix()

	raw, err := idx.GetSessionMetadata(testBaseA, sessionMetaLastActivity)
	if err != nil {
		t.Fatalf("GetSessionMetadata: %v", err)
	}
	if raw == "" {
		t.Fatalf("last_activity not written (empty value)")
	}
	got, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("parse last_activity %q: %v", raw, err)
	}
	if got < before || got > after {
		t.Fatalf("last_activity %d outside expected window [%d, %d]", got, before, after)
	}
}

// TestTouchLastActivity_Idempotent verifies repeated calls keep advancing
// the timestamp without error. Each call overwrites the previous value.
func TestTouchLastActivity_Idempotent(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{
		AgentID:      "test-agent",
		SessionIndex: idx,
	}

	a.touchLastActivity(testBaseA)
	first, err := idx.GetSessionMetadata(testBaseA, sessionMetaLastActivity)
	if err != nil || first == "" {
		t.Fatalf("first read: err=%v val=%q", err, first)
	}

	// Sleep long enough that a new unix-second tick is likely.
	time.Sleep(1100 * time.Millisecond)
	a.touchLastActivity(testBaseA)
	second, err := idx.GetSessionMetadata(testBaseA, sessionMetaLastActivity)
	if err != nil || second == "" {
		t.Fatalf("second read: err=%v val=%q", err, second)
	}

	firstN, _ := strconv.ParseInt(first, 10, 64)
	secondN, _ := strconv.ParseInt(second, 10, 64)
	if secondN < firstN {
		t.Fatalf("second timestamp %d < first %d (not monotonic)", secondN, firstN)
	}
}

// TestTouchLastActivity_PerBase verifies that writes under one base do not
// affect another base's row. Confirms the per-session keying.
func TestTouchLastActivity_PerBase(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{
		AgentID:      "test-agent",
		SessionIndex: idx,
	}

	a.touchLastActivity(testBaseA)
	rawA, _ := idx.GetSessionMetadata(testBaseA, sessionMetaLastActivity)
	rawB, _ := idx.GetSessionMetadata(testBaseB, sessionMetaLastActivity)
	if rawA == "" {
		t.Fatalf("touchLastActivity(%s): row not written", testBaseA)
	}
	if rawB != "" {
		t.Fatalf("touchLastActivity(%s) leaked into %s row: got %q, want empty", testBaseA, testBaseB, rawB)
	}
}

// TestTouchLastActivity_NoSessionIndex verifies the helper is safe when
// SessionIndex is nil (test agents, partially-constructed Agents).
func TestTouchLastActivity_NoSessionIndex(t *testing.T) {
	a := &Agent{AgentID: "test-agent"}
	// Must not panic.
	a.touchLastActivity(testBaseA)
}

// TestTouchLastActivity_EmptyBase verifies the helper is a no-op when base
// is empty (defensive — prevents writing to an "" session row, which would
// be meaningless).
func TestTouchLastActivity_EmptyBase(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{
		AgentID:      "test-agent",
		SessionIndex: idx,
	}
	a.touchLastActivity("")

	// Confirm nothing got written under the empty base.
	raw, _ := idx.GetSessionMetadata("", sessionMetaLastActivity)
	if raw != "" {
		t.Fatalf("touchLastActivity with empty base wrote %q under empty key", raw)
	}
}
