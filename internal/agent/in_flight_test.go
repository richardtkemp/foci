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

	done := a.markInFlight(testBaseA)
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
	done := a.markInFlight(testBaseA)
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
		dones = append(dones, a.markInFlight(testBaseA))
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

	doneA := a.markInFlight(testBaseA)
	if !a.IsTurnInFlight(testBaseA) {
		t.Fatalf("IsTurnInFlight(%s) after mark: got false, want true", testBaseA)
	}
	if a.IsTurnInFlight(testBaseB) {
		t.Fatalf("IsTurnInFlight(%s) leaked from %s mark: got true, want false", testBaseB, testBaseA)
	}

	doneB := a.markInFlight(testBaseB)
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
		go func(b string) {
			defer wg.Done()
			done := a.markInFlight(b)
			time.Sleep(time.Millisecond)
			done()
		}(base)
	}
	wg.Wait()
	if a.IsTurnInFlight(testBaseA) {
		t.Fatalf("after %d concurrent mark/done pairs: IsTurnInFlight(%s) = true, want false", N, testBaseA)
	}
	if a.IsTurnInFlight(testBaseB) {
		t.Fatalf("after %d concurrent mark/done pairs: IsTurnInFlight(%s) = true, want false", N, testBaseB)
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
