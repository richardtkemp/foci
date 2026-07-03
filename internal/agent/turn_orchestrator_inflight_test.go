package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
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
func (s *rateLimitedStubContract) RegisterSessionIndex(*TurnState)       {}
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

// TestOrchestrator_TouchLastActivityWritesRow verifies that running a turn
// through OrchestrateFullTurn writes the last_activity row keyed by the
// session's root key. Covers Stage B's promise that "every turn-init path
// participates" via the single chokepoint, and that the row is keyed
// correctly for the gate to consult later.
func TestOrchestrator_TouchLastActivityWritesRow(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{
		AgentID:      "test-agent",
		SessionIndex: idx,
	}

	// Confirm no prior row.
	if raw, _ := idx.GetSessionMetadata(orchestratorTestKey, sessionMetaLastActivity); raw != "" {
		t.Fatalf("pre-call: last_activity = %q, want empty", raw)
	}

	tc := &stubContract{}
	ts := NewTurnState(context.Background(), orchestratorTestKey, []string{"hi"}, nil)

	before := time.Now().Unix()
	_, err = a.OrchestrateFullTurn(context.Background(), tc, ts)
	after := time.Now().Unix()
	if err != nil {
		t.Fatalf("OrchestrateFullTurn: %v", err)
	}

	raw, err := idx.GetSessionMetadata(orchestratorTestKey, sessionMetaLastActivity)
	if err != nil {
		t.Fatalf("GetSessionMetadata: %v", err)
	}
	if raw == "" {
		t.Fatalf("post-call: last_activity not written")
	}
	got, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("parse last_activity %q: %v", raw, err)
	}
	if got < before || got > after {
		t.Fatalf("last_activity %d outside expected window [%d, %d]", got, before, after)
	}
}

// TestOrchestrator_TouchLastActivityWritesEvenOnError verifies that a turn
// failing mid-flight still records last_activity. The agent attempted a
// turn — that's activity, regardless of inference outcome. Touch happens
// in Phase 1b before RunInference, so the error path doesn't skip it.
func TestOrchestrator_TouchLastActivityWritesEvenOnError(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{
		AgentID:      "test-agent",
		SessionIndex: idx,
	}

	tc := &errorStubContract{}
	ts := NewTurnState(context.Background(), orchestratorTestKey, []string{"hi"}, nil)

	_, err = a.OrchestrateFullTurn(context.Background(), tc, ts)
	if err == nil {
		t.Fatalf("expected error from RunInference")
	}

	raw, _ := idx.GetSessionMetadata(orchestratorTestKey, sessionMetaLastActivity)
	if raw == "" {
		t.Fatalf("last_activity not written despite error path")
	}

	// inFlight must still drop back to zero on the error path.
	if a.IsTurnInFlight(orchestratorTestKey) {
		t.Fatalf("after error: IsTurnInFlight(%s) = true, want false", orchestratorTestKey)
	}
}

// TestOrchestrator_RateLimitGateSkipsInFlightAndTouch verifies that when
// the rate-limit gate rejects the turn before it really starts, neither
// the inFlight counter nor last_activity are touched. This protects the
// "rate-limited != activity" semantic.
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

	tc := &rateLimitedStubContract{}
	ts := NewTurnState(context.Background(), orchestratorTestKey, []string{"hi"}, nil)

	_, err = a.OrchestrateFullTurn(context.Background(), tc, ts)
	if err == nil {
		t.Fatalf("expected rate-limit error")
	}

	if a.IsTurnInFlight(orchestratorTestKey) {
		t.Fatalf("after rate-limit reject: IsTurnInFlight(%s) = true, want false", orchestratorTestKey)
	}
	if raw, _ := idx.GetSessionMetadata(orchestratorTestKey, sessionMetaLastActivity); raw != "" {
		t.Fatalf("rate-limit reject wrote last_activity = %q (should be untouched)", raw)
	}
}

// TestOrchestrator_BranchTurnKeysInFlightByBranchAndActivityByRoot verifies
// the split key semantics for branch turns: the in-flight counter tracks the
// branch's OWN key (branches are distinct identities — a facet turn must not
// couple to the parent), while last_activity is recorded against the parent
// ROOT key (so keepalive/CLI gates targeting the main session see the
// activity).
func TestOrchestrator_BranchTurnKeysInFlightByBranchAndActivityByRoot(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	a := &Agent{
		AgentID:      "test-agent",
		SessionIndex: idx,
	}

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

	// last_activity lands under the ROOT key, not the branch key.
	if raw, _ := idx.GetSessionMetadata(orchestratorTestKey, sessionMetaLastActivity); raw == "" {
		t.Error("branch turn did not write last_activity under the root key")
	}
	if raw, _ := idx.GetSessionMetadata(orchestratorTestBranchKey, sessionMetaLastActivity); raw != "" {
		t.Errorf("branch turn wrote last_activity under the branch key = %q, want empty", raw)
	}
}
