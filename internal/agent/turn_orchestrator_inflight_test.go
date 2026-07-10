package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/session"
)

// rateLimitedStubContract simulates RateLimitGate rejecting the turn
// before any setup runs. Used to verify that the in-flight signal and
// last_activity are NOT bumped when a turn is gated out at the entry.
type rateLimitedStubContract struct{}

func (s *rateLimitedStubContract) RateLimitGate(*TurnState) error {
	return fmt.Errorf("rate-limited")
}
func (s *rateLimitedStubContract) AcquireTurnLock(*TurnState) func()     { return func() {} }
func (s *rateLimitedStubContract) IncrementProcessing(*TurnState) func() { return func() {} }
func (s *rateLimitedStubContract) RegisterTurn(*TurnState) func()        { return func() {} }
func (s *rateLimitedStubContract) CheckStaleContext(*TurnState) error    { return nil }
func (s *rateLimitedStubContract) LogConversationRecv(*TurnState)        {}
func (s *rateLimitedStubContract) TouchActivity(*TurnState)              {}
func (s *rateLimitedStubContract) LoadSessionMeta(*TurnState)            {}
func (s *rateLimitedStubContract) ComposePrompt(*TurnState) error        { return nil }
func (s *rateLimitedStubContract) LoadAndRepairSession(*TurnState) error { return nil }
func (s *rateLimitedStubContract) ResolveModelEffort(*TurnState)         {}
func (s *rateLimitedStubContract) BuildSystemAndTools(*TurnState)        {}
func (s *rateLimitedStubContract) InjectNudges(*TurnState)               {}
func (s *rateLimitedStubContract) RunInference(ts *TurnState) error {
	close(ts.CompletionChan)
	return nil
}
func (s *rateLimitedStubContract) SaveSession(*TurnState) error   { return nil }
func (s *rateLimitedStubContract) UpdateSessionMeta(*TurnState)   {}
func (s *rateLimitedStubContract) LogUsage(*TurnState)            {}
func (s *rateLimitedStubContract) RunCompaction(*TurnState)       {}
func (s *rateLimitedStubContract) LogConversationSent(*TurnState) {}
func (s *rateLimitedStubContract) TouchActivityPost(*TurnState)   {}

// orchestratorTestKey is a representative root session key for orchestrator
// integration tests. Session keys are stable identities: the in-flight counter
// is keyed directly by the session key, and last_activity is keyed by the root
// key — for a root key like this one, both are the key itself.
const orchestratorTestKey = "test-agent/cTEST"

// orchestratorTestBranchKey is a branch child of orchestratorTestKey. Branch
// turns track in-flight under their own key but record last_activity against
// the parent root.
const orchestratorTestBranchKey = orchestratorTestKey + "/b1700000000"

// TestOrchestrator_InFlightRisesAndFalls_API verifies that a synchronous
// (API-path) turn flips IsTurnInFlight(key) from false → true → false
// across the orchestrator call. RunInference closes CompletionChan inline;
// the markInFlight defer runs as the orchestrator returns.
func TestOrchestrator_InFlightRisesAndFalls_API(t *testing.T) {
	a := &Agent{}

	if a.IsTurnInFlight(orchestratorTestKey) {
		t.Fatalf("pre-call: IsTurnInFlight(%s) = true, want false", orchestratorTestKey)
	}

	tc := &stubContract{}
	ts := NewTurnState(context.Background(), orchestratorTestKey, []string{"hi"}, nil)

	_, err := a.OrchestrateFullTurn(context.Background(), tc, ts)
	if err != nil {
		t.Fatalf("OrchestrateFullTurn: %v", err)
	}

	if a.IsTurnInFlight(orchestratorTestKey) {
		t.Fatalf("post-call: IsTurnInFlight(%s) = true, want false", orchestratorTestKey)
	}
}

// TestOrchestrator_InFlightStaysTrueDuringDelegatedWait verifies that for
// a delegated turn whose backend doesn't immediately close CompletionChan,
// IsTurnInFlight remains true for the session key throughout the wait.
// This is the regression that motivates the whole stage: a permission-
// blocked CC turn must keep the gate signal lit until the user actually
// decides.
//
// Plan B3 specifies a 30s wait; we use 200ms here because the property is
// duration-independent (the orchestrator blocks on CompletionChan via
// runPostTurn — same code path regardless of how long the wait actually
// is). 200ms is long enough to sample twice with margin and short enough
// not to slow the suite.
func TestOrchestrator_InFlightStaysTrueDuringDelegatedWait(t *testing.T) {
	a := &Agent{}

	const delay = 200 * time.Millisecond
	tc := &asyncStubContract{completionDelay: delay}
	ts := NewTurnState(context.Background(), orchestratorTestKey, []string{"hi"}, nil)

	// Run the orchestrator in a goroutine and sample IsTurnInFlight while
	// it's blocked in runPostTurn.
	resultErr := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := a.OrchestrateFullTurn(context.Background(), tc, ts)
		resultErr <- err
	}()

	// Give the goroutine a moment to enter OrchestrateFullTurn and bump
	// the counter.
	time.Sleep(20 * time.Millisecond)

	// Sample at 50ms and 120ms relative to start — both should observe
	// the in-flight signal while the asyncStub's goroutine is still
	// asleep (it closes CompletionChan at 200ms). The 80ms safety margin
	// before the 200ms close keeps the test non-flaky under load.
	for _, sampleAt := range []time.Duration{50 * time.Millisecond, 120 * time.Millisecond} {
		if remaining := sampleAt - time.Since(start); remaining > 0 {
			time.Sleep(remaining)
		}
		if !a.IsTurnInFlight(orchestratorTestKey) {
			t.Fatalf("at sample %v during delegated wait: IsTurnInFlight(%s) = false, want true", sampleAt, orchestratorTestKey)
		}
	}

	// Wait for orchestrator to return (CompletionChan close + post-turn).
	select {
	case err := <-resultErr:
		if err != nil {
			t.Fatalf("OrchestrateFullTurn: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("orchestrator did not return within 5s of completion delay %v", delay)
	}

	if a.IsTurnInFlight(orchestratorTestKey) {
		t.Fatalf("after orchestrator return: IsTurnInFlight(%s) = true, want false", orchestratorTestKey)
	}
}

// TestOrchestrator_CacheTouchWritesRow verifies that running a turn through
// OrchestrateFullTurn stamps last_cache_touch on the session's own row. Covers
// the single chokepoint's promise that "every turn-init path participates".
func TestOrchestrator_CacheTouchWritesRow(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{
		AgentID:      "test-agent",
		SessionIndex: idx,
	}
	// Seed the row: in production recordTurnActivity upserts it, but this test
	// drives a stub contract, so create it up front for the cache-touch assertions.
	seedCacheTestRow(idx, orchestratorTestKey)

	// Confirm no prior cache touch.
	if _, ok := idx.LastCacheTouch(orchestratorTestKey); ok {
		t.Fatalf("pre-call: cache touch already present")
	}

	tc := &stubContract{}
	ts := NewTurnState(context.Background(), orchestratorTestKey, []string{"hi"}, nil)

	before := time.Now().Add(-2 * time.Second)
	_, err = a.OrchestrateFullTurn(context.Background(), tc, ts)
	after := time.Now().Add(2 * time.Second)
	if err != nil {
		t.Fatalf("OrchestrateFullTurn: %v", err)
	}

	got, ok := idx.LastCacheTouch(orchestratorTestKey)
	if !ok {
		t.Fatalf("post-call: cache touch not written")
	}
	if got.Before(before) || got.After(after) {
		t.Fatalf("cache touch %v outside expected window [%v, %v]", got, before, after)
	}
}

// TestOrchestrator_CacheTouchWritesEvenOnError verifies that a turn failing
// mid-flight still records the cache touch. The agent attempted a turn — that
// warmed the cache, regardless of inference outcome. Touch happens in Phase 1b
// before RunInference, so the error path doesn't skip it.
func TestOrchestrator_CacheTouchWritesEvenOnError(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{
		AgentID:      "test-agent",
		SessionIndex: idx,
	}
	seedCacheTestRow(idx, orchestratorTestKey)

	tc := &errorStubContract{}
	ts := NewTurnState(context.Background(), orchestratorTestKey, []string{"hi"}, nil)

	_, err = a.OrchestrateFullTurn(context.Background(), tc, ts)
	if err == nil {
		t.Fatalf("expected error from RunInference")
	}

	if _, ok := idx.LastCacheTouch(orchestratorTestKey); !ok {
		t.Fatalf("cache touch not written despite error path")
	}

	// inFlight must still drop back to zero on the error path.
	if a.IsTurnInFlight(orchestratorTestKey) {
		t.Fatalf("after error: IsTurnInFlight(%s) = true, want false", orchestratorTestKey)
	}
}

// TestOrchestrator_RateLimitGateSkipsInFlightAndTouch verifies that when the
// rate-limit gate rejects the turn before it really starts, neither the
// inFlight counter nor the cache touch are bumped. Protects the "rate-limited
// != cache warmed" semantic (the turn never ran, so it touched nothing).
func TestOrchestrator_RateLimitGateSkipsInFlightAndTouch(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{
		AgentID:      "test-agent",
		SessionIndex: idx,
	}
	seedCacheTestRow(idx, orchestratorTestKey)

	tc := &rateLimitedStubContract{}
	ts := NewTurnState(context.Background(), orchestratorTestKey, []string{"hi"}, nil)

	_, err = a.OrchestrateFullTurn(context.Background(), tc, ts)
	if err == nil {
		t.Fatalf("expected rate-limit error")
	}

	if a.IsTurnInFlight(orchestratorTestKey) {
		t.Fatalf("after rate-limit reject: IsTurnInFlight(%s) = true, want false", orchestratorTestKey)
	}
	if _, ok := idx.LastCacheTouch(orchestratorTestKey); ok {
		t.Fatalf("rate-limit reject wrote a cache touch (should be untouched)")
	}
}

// TestOrchestrator_BranchTurnKeysInFlightAndCacheByBranchOnly verifies the key
// semantics for branch turns: the in-flight counter tracks the branch's OWN key
// (branches are distinct identities — a facet turn must not couple to the
// parent), and the cache touch lands on the branch key ONLY. Root is warmed just
// once at branch CREATION (TouchRootCacheForBranch), not on every branch turn.
func TestOrchestrator_BranchTurnKeysInFlightAndCacheByBranchOnly(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{
		AgentID:      "test-agent",
		SessionIndex: idx,
	}
	seedCacheTestRow(idx, orchestratorTestKey)
	seedCacheTestRow(idx, orchestratorTestBranchKey)

	const delay = 100 * time.Millisecond
	tc := &asyncStubContract{completionDelay: delay}
	ts := NewTurnState(context.Background(), orchestratorTestBranchKey, []string{"hi"}, nil)

	resultErr := make(chan error, 1)
	go func() {
		_, err := a.OrchestrateFullTurn(context.Background(), tc, ts)
		resultErr <- err
	}()

	// Sample mid-turn: in-flight must be lit for the branch key only.
	time.Sleep(40 * time.Millisecond)
	if !a.IsTurnInFlight(orchestratorTestBranchKey) {
		t.Errorf("mid-turn: IsTurnInFlight(%s) = false, want true", orchestratorTestBranchKey)
	}
	if a.IsTurnInFlight(orchestratorTestKey) {
		t.Errorf("mid-turn: IsTurnInFlight(%s) = true, want false — branch must not couple to root", orchestratorTestKey)
	}

	select {
	case err := <-resultErr:
		if err != nil {
			t.Fatalf("OrchestrateFullTurn: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("orchestrator did not return")
	}

	// Cache touch lands under the branch key ONLY — the branch turn must not
	// advance root's cache (root is warmed only at branch creation).
	if _, ok := idx.LastCacheTouch(orchestratorTestBranchKey); !ok {
		t.Error("branch turn did not write cache touch under the branch key")
	}
	if _, ok := idx.LastCacheTouch(orchestratorTestKey); ok {
		t.Error("branch turn wrongly wrote cache touch under the root key (should only happen at branch creation)")
	}
}

// TestTouchRootCacheForBranch verifies the one-time creation-time root warm: it
// stamps last_cache_touch on the branch's ROOT (not the branch key), and is a
// no-op when handed a root key.
func TestTouchRootCacheForBranch(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{AgentID: "test-agent", SessionIndex: idx}
	seedCacheTestRow(idx, orchestratorTestKey)
	seedCacheTestRow(idx, orchestratorTestBranchKey)

	a.TouchRootCacheForBranch(orchestratorTestBranchKey)

	if _, ok := idx.LastCacheTouch(orchestratorTestKey); !ok {
		t.Error("TouchRootCacheForBranch did not warm the root's cache")
	}
	if _, ok := idx.LastCacheTouch(orchestratorTestBranchKey); ok {
		t.Error("TouchRootCacheForBranch wrongly touched the branch key (should touch root only)")
	}

	// No-op for a root key: nothing to warm above it.
	a.TouchRootCacheForBranch(orchestratorTestKey)
	if _, ok := idx.LastCacheTouch(orchestratorTestKey); !ok {
		t.Error("sanity: root cache should still be present")
	}
}

// TestOrchestrator_UserActivityGatedByTrigger verifies the split between the two
// signals: EVERY turn bumps last_cache_touch (cache freshness), but only a
// real-time interactive turn bumps last_user_activity (human attention). Both a
// keepalive turn AND an HTTP /send ("user") warm the cache without counting as
// user activity — only interactive input (here: voice) does.
func TestOrchestrator_UserActivityGatedByTrigger(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{AgentID: "test-agent", SessionIndex: idx}
	seedCacheTestRow(idx, orchestratorTestKey)

	// The orchestrator sets ts.Trigger from the context (WithTrigger), so the
	// trigger must be injected there, not on the TurnState struct.

	// Neither a keepalive turn nor an HTTP /send ("user") is interactive input:
	// both warm the cache but must NOT count as user activity.
	for _, trig := range []string{"keepalive", "user"} {
		tc := &stubContract{}
		ctx := WithTrigger(context.Background(), trig)
		ts := NewTurnState(ctx, orchestratorTestKey, []string{"hi"}, nil)
		if _, err := a.OrchestrateFullTurn(ctx, tc, ts); err != nil {
			t.Fatalf("%s turn: %v", trig, err)
		}
		if _, ok := idx.LastCacheTouch(orchestratorTestKey); !ok {
			t.Fatalf("%s turn did not write cache touch", trig)
		}
		if _, ok := idx.LastUserActivityForAgent("test-agent"); ok {
			t.Fatalf("%s turn wrongly wrote last_user_activity", trig)
		}
	}

	// A voice turn IS real-time interactive input → user activity.
	tcV := &stubContract{}
	ctxV := WithTrigger(context.Background(), "voice")
	tsV := NewTurnState(ctxV, orchestratorTestKey, []string{"hi"}, nil)
	if _, err := a.OrchestrateFullTurn(ctxV, tcV, tsV); err != nil {
		t.Fatalf("voice turn: %v", err)
	}
	if _, ok := idx.LastUserActivityForAgent("test-agent"); !ok {
		t.Fatalf("voice turn did not write last_user_activity")
	}
}

// TestOrchestrator_CentralLastMessageTimeWrite verifies OrchestrateFullTurn
// writes lastMessageTime centrally (after ComposePrompt) — the write that used
// to live in each transport. Covers the F2 consolidation.
func TestOrchestrator_CentralLastMessageTimeWrite(t *testing.T) {
	a := &Agent{AgentID: "test-agent"}
	tc := &stubContract{}
	ts := NewTurnState(context.Background(), orchestratorTestKey, []string{"hi"}, nil)
	// Stub LoadSessionMeta is a no-op, so provide the meta the central write targets.
	ts.SessionMeta = &sessionMeta{}

	before := time.Now().Add(-time.Second)
	if _, err := a.OrchestrateFullTurn(context.Background(), tc, ts); err != nil {
		t.Fatalf("OrchestrateFullTurn: %v", err)
	}
	if ts.SessionMeta.lastMessageTime.Before(before) {
		t.Fatalf("central write did not set lastMessageTime: got %v", ts.SessionMeta.lastMessageTime)
	}
}
