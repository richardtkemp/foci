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
	t.Parallel()
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
// Plan B3 specifies a 30s wait; the property is duration-independent (the
// orchestrator blocks on CompletionChan via runPostTurn — same code path
// regardless of how long the wait actually is), so the test holds the
// delegated turn open on a `gate` channel it controls directly rather than
// timing a real wait. This used to sample IsTurnInFlight at fixed wall-clock
// offsets (50ms/120ms into a 200ms delay) and flaked under load (#1991):
// the margins assumed the orchestrator goroutine got scheduled promptly,
// which a loaded host doesn't guarantee. Gating on a channel makes the
// property hold regardless of scheduling delay — nothing can advance the
// stub's completion until the test says so.
func TestOrchestrator_InFlightStaysTrueDuringDelegatedWait(t *testing.T) {
	t.Parallel()
	a := &Agent{}

	started := make(chan struct{})
	release := make(chan struct{})
	tc := &asyncStubContract{
		onRunInference: func() { close(started) },
		gate:           release,
	}
	ts := NewTurnState(context.Background(), orchestratorTestKey, []string{"hi"}, nil)

	// Run the orchestrator in a goroutine; it will block in runPostTurn
	// until we close `release`.
	resultErr := make(chan error, 1)
	go func() {
		_, err := a.OrchestrateFullTurn(context.Background(), tc, ts)
		resultErr <- err
	}()

	// Wait for RunInference to be entered. markInFlight runs in Phase 1,
	// strictly before RunInference (Phase 3) — so by the time this fires,
	// the in-flight counter is already bumped. No sleep-and-hope needed.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("RunInference was not entered within 5s")
	}

	if !a.IsTurnInFlight(orchestratorTestKey) {
		t.Fatalf("on entering the delegated wait: IsTurnInFlight(%s) = false, want true", orchestratorTestKey)
	}

	// The stub is gated on `release`, so CompletionChan cannot close yet —
	// this second check (after a real, but non-load-bearing, pause) proves
	// the signal stays lit for the DURATION of the wait, not just at entry,
	// while remaining immune to scheduler jitter: nothing times out here.
	time.Sleep(20 * time.Millisecond)
	if !a.IsTurnInFlight(orchestratorTestKey) {
		t.Fatalf("mid delegated wait: IsTurnInFlight(%s) = false, want true", orchestratorTestKey)
	}

	close(release)

	// Wait for orchestrator to return (CompletionChan close + post-turn).
	select {
	case err := <-resultErr:
		if err != nil {
			t.Fatalf("OrchestrateFullTurn: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("orchestrator did not return within 5s of release")
	}

	if a.IsTurnInFlight(orchestratorTestKey) {
		t.Fatalf("after orchestrator return: IsTurnInFlight(%s) = true, want false", orchestratorTestKey)
	}
}

// TestOrchestrator_TurnAloneDoesNotStampCacheTouch: running a turn through
// OrchestrateFullTurn does not stamp last_cache_touch by itself. Only a model
// response's cache usage does (recordCacheEvidence, #2509); this stub
// contract reports none.
func TestOrchestrator_TurnAloneDoesNotStampCacheTouch(t *testing.T) {
	t.Parallel()
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

	tc := &stubContract{}
	ts := NewTurnState(context.Background(), orchestratorTestKey, []string{"hi"}, nil)
	if _, err := a.OrchestrateFullTurn(context.Background(), tc, ts); err != nil {
		t.Fatalf("OrchestrateFullTurn: %v", err)
	}
	if got, ok := idx.LastCacheTouch(orchestratorTestKey); ok {
		t.Fatalf("a turn with no model usage stamped last_cache_touch = %v", got)
	}
}

// TestOrchestrator_FailedTurnLeavesCacheTouch is #2509 on the own key: a
// turn whose model call fails has no evidence that it warmed anything, so
// last_cache_touch keeps its old value (here: none).
func TestOrchestrator_FailedTurnLeavesCacheTouch(t *testing.T) {
	t.Parallel()
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

	if got, ok := idx.LastCacheTouch(orchestratorTestKey); ok {
		t.Fatalf("a failed turn stamped last_cache_touch = %v", got)
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
	t.Parallel()
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
// parent). A branch turn with no model usage stamps no cache touch on either
// key: the root is warmed only by an inheriting branch's first evidenced
// response (NoteBranchInheritsRoot, #2509).
func TestOrchestrator_BranchTurnKeysInFlightAndCacheByBranchOnly(t *testing.T) {
	t.Parallel()
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

	started := make(chan struct{})
	release := make(chan struct{})
	tc := &asyncStubContract{
		onRunInference: func() { close(started) },
		gate:           release,
	}
	ts := NewTurnState(context.Background(), orchestratorTestBranchKey, []string{"hi"}, nil)

	resultErr := make(chan error, 1)
	go func() {
		_, err := a.OrchestrateFullTurn(context.Background(), tc, ts)
		resultErr <- err
	}()

	// Wait for RunInference to be entered — markInFlight (Phase 1) has already
	// run by then, so the in-flight counter is deterministically set. No
	// sleep-and-sample: the stub is held open on `release` until we say so.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("RunInference was not entered within 5s")
	}

	// In-flight must be lit for the branch key only.
	if !a.IsTurnInFlight(orchestratorTestBranchKey) {
		t.Errorf("mid-turn: IsTurnInFlight(%s) = false, want true", orchestratorTestBranchKey)
	}
	if a.IsTurnInFlight(orchestratorTestKey) {
		t.Errorf("mid-turn: IsTurnInFlight(%s) = true, want false — branch must not couple to root", orchestratorTestKey)
	}

	close(release)

	select {
	case err := <-resultErr:
		if err != nil {
			t.Fatalf("OrchestrateFullTurn: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("orchestrator did not return")
	}

	// No model usage, no evidence: neither key is stamped.
	if _, ok := idx.LastCacheTouch(orchestratorTestBranchKey); ok {
		t.Error("branch turn with no model usage wrote a cache touch under the branch key")
	}
	if _, ok := idx.LastCacheTouch(orchestratorTestKey); ok {
		t.Error("branch turn wrongly wrote cache touch under the root key")
	}
}

// TestOrchestrator_UserActivityGatedByTrigger verifies the split between the two
// last_user_activity (human attention) is bumped only by a real-time
// interactive turn. A keepalive turn and an HTTP /send ("user") do not count
// as user activity; only interactive input (here: voice) does. (Cache warmth
// is stamped by model usage, not by the turn: recordCacheEvidence, #2509.)
func TestOrchestrator_UserActivityGatedByTrigger(t *testing.T) {
	t.Parallel()
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
	// neither may count as user activity.
	for _, trig := range []string{"keepalive", "user"} {
		tc := &stubContract{}
		ctx := WithTrigger(context.Background(), trig)
		ts := NewTurnState(ctx, orchestratorTestKey, []string{"hi"}, nil)
		if _, err := a.OrchestrateFullTurn(ctx, tc, ts); err != nil {
			t.Fatalf("%s turn: %v", trig, err)
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
	t.Parallel()
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
