package agent

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"foci/internal/agent/turnevent"
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

// recordTurnActivityTS builds a minimal TurnState for driving recordTurnActivity.
func recordTurnActivityTS(key, trigger string) *TurnState {
	ts := NewTurnState(context.Background(), key, []string{"x"}, nil)
	ts.Trigger = trigger
	return ts
}

// TestRecordTurnActivity_WritesCacheTouch verifies the single per-turn write
// stamps last_cache_touch on the session's own row within ±2s of now.
func TestRecordTurnActivity_WritesCacheTouch(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{AgentID: "test-agent", SessionIndex: idx}
	seedCacheTestRow(idx, testBaseA)

	before := time.Now().Add(-2 * time.Second)
	a.recordTurnActivity(recordTurnActivityTS(testBaseA, "keepalive"))
	after := time.Now().Add(2 * time.Second)

	got, ok := idx.LastCacheTouch(testBaseA)
	if !ok {
		t.Fatalf("last_cache_touch not written for %s", testBaseA)
	}
	if got.Before(before) || got.After(after) {
		t.Fatalf("last_cache_touch %v outside window [%v, %v]", got, before, after)
	}
}

// TestRecordTurnActivity_CreatesRowIfMissing verifies the write is an UPSERT: a
// turn on a key with no index row creates it (subsuming the old
// RegisterSessionIndex), unlike the former UPDATE-only touchCacheFreshness.
func TestRecordTurnActivity_CreatesRowIfMissing(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{AgentID: "test-agent", SessionIndex: idx}
	a.recordTurnActivity(recordTurnActivityTS(testBaseA, "user"))

	if _, err := idx.Get(testBaseA); err != nil {
		t.Fatalf("row not created by recordTurnActivity: %v", err)
	}
	if _, ok := idx.LastCacheTouch(testBaseA); !ok {
		t.Fatalf("last_cache_touch not written on freshly-created row")
	}
}

// TestRecordTurnActivity_BranchOwnKeyOnly verifies a branch turn stamps its own
// cache row but NOT its root's (root is warmed only at branch creation).
func TestRecordTurnActivity_BranchOwnKeyOnly(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{AgentID: "test-agent", SessionIndex: idx}
	branchKey := testBaseA + "/b1700000000"
	seedCacheTestRow(idx, testBaseA)
	seedCacheTestRow(idx, branchKey)

	a.recordTurnActivity(recordTurnActivityTS(branchKey, "keepalive"))

	if _, ok := idx.LastCacheTouch(branchKey); !ok {
		t.Fatalf("branch %s cache touch not written", branchKey)
	}
	if _, ok := idx.LastCacheTouch(testBaseA); ok {
		t.Fatalf("root %s cache touch wrongly written by a branch turn", testBaseA)
	}
}

// TestRecordTurnActivity_NoSessionIndex verifies the write is safe when
// SessionIndex is nil (partially-constructed Agents).
func TestRecordTurnActivity_NoSessionIndex(t *testing.T) {
	a := &Agent{AgentID: "test-agent"}
	a.recordTurnActivity(recordTurnActivityTS(testBaseA, "user")) // must not panic
}

// fakeDurableSink is a minimal turnevent.Sink that records every event it
// receives, standing in for "a sink that persists durably" without pulling
// in the real app/hub.go conv-binding machinery. Its presence in a test is
// the observable proxy for "this got stored somewhere a reconnecting client
// could replay it from" — the real implementation would be backed by
// app_frames.db via the session's chat_metadata conv_id binding, but what
// this test cares about is that autonomousTurnSink REACHES for a durable
// fallback at all when no live connection exists, not the storage engine
// behind it.
type fakeDurableSink struct {
	events []turnevent.Event
}

func (f *fakeDurableSink) Emit(_ context.Context, ev turnevent.Event) { f.events = append(f.events, ev) }
func (f *fakeDurableSink) DeliversToPlatform() bool                   { return false }

// TestAutonomousTurnSink_NoConnection_FallsBackToDurableSink is the clutch
// #1350 follow-up (2026-07-17): a running-edge autonomous adoption landing
// when ResolveLateConn's normal cascade (route.ConnFor) resolves no
// connection at all must not simply discard everything the adopted turn
// produces. (For the app platform specifically this gap is narrower than "no
// live socket" — the app's own delivery already tolerates a dead socket fine,
// since it binds to a durable per-conversation object, not a live one; see
// app.DurableConnFor's doc comment. It's genuine resolution failures — e.g.
// the session's agent has no app registration at all — that reach here.)
// internal/app/hub.go's convBinding.send already establishes the right
// pattern elsewhere in the app: persist every frame to a durable store
// UNCONDITIONALLY, before even checking which clients are currently
// connected — so persistence and live-push are separate concerns, and a
// client that connects later replays what it missed. autonomousTurnSink had
// no equivalent: it only built ANY sink when conn != nil, and fell straight
// to the total-discard turnevent.NopSink otherwise — "nothing resolved"
// degraded to "gone", not "durable but not pushed live".
//
// This test specifies the fix: when ResolveLateConn finds no connection but
// the Agent has a DurableTurnSink resolver wired (i.e. this session DOES have
// a platform/chat binding to persist against), autonomousTurnSink must use
// THAT sink rather than falling to NopSink.
func TestAutonomousTurnSink_NoConnection_FallsBackToDurableSink(t *testing.T) {
	durable := &fakeDurableSink{}
	a := &Agent{
		DurableTurnSink: func(sk string) turnevent.Sink {
			if sk != testBaseA {
				t.Fatalf("DurableTurnSink called with sk=%q, want %q", sk, testBaseA)
			}
			return durable
		},
	}

	sink, cleanup := a.autonomousTurnSink(nil, testBaseA)
	if cleanup != nil {
		defer cleanup()
	}

	if _, isNop := sink.(turnevent.NopSink); isNop {
		t.Fatal("autonomousTurnSink(nil conn) fell to NopSink despite DurableTurnSink being wired — " +
			"the adopted turn's output will be discarded with no durable record instead of persisted " +
			"for replay when a client reconnects (clutch #1350 follow-up)")
	}

	want := turnevent.TextBlock{Text: "the subagent's answer, durably persisted"}
	sink.Emit(context.Background(), want)

	if len(durable.events) != 1 || durable.events[0] != want {
		t.Fatalf("durable sink recorded %v, want exactly [%v] — autonomousTurnSink must route Emit "+
			"through the durable fallback, not just resolve it and then discard anyway", durable.events, want)
	}
}
