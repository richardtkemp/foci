package agent

import (
	"context"
	"testing"
	"time"

	"foci/internal/delegator"
)

// testPendingHoldLimit stands in for maxPendingHold (5 minutes in
// production) so a test sees the limit pass in milliseconds.
const testPendingHoldLimit = 300 * time.Millisecond

// TestInbox_PendingWorkHoldEndsAfterLimit is the #2792 repro: a system inject
// (here a spawn result's async_notify) parked behind background work that
// never finishes — a long `until … sleep` watcher — must be delivered once it
// has waited the limit, not held for as long as the work runs. The work stays
// pending for the whole test, and the backend refuses a pending-work system
// begin unless it carries PendingWorkExempt, exactly as ccstream's tryBegin
// does, so the run proves both layers let it through: the inbox gate and the
// backend begin. RED before #2792: the inject stays parked for ever.
func TestInbox_PendingWorkHoldEndsAfterLimit(t *testing.T) {
	initTestConvo(t, "helen")
	const sk = "helen/c2792"

	be := &mockBackendDT{sessionFile: "/tmp/hold-limit.jsonl", refusePending: true}
	be.setAwaiting(true) // background work outstanding for the whole test
	be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
		if h != nil && h.OnTurnComplete != nil {
			h.OnTurnComplete(&delegator.TurnResult{Text: "noted", Model: "claude-sonnet-4-5"})
		}
		return nil, nil
	}
	// No turn is in flight, so a refused begin's retry wait would return at
	// once; pace it, so a begin the backend keeps refusing fails the test at
	// its deadline instead of spinning the retry loop (#2435).
	be.waitForTurnFn = func(ctx context.Context) error {
		select {
		case <-time.After(testGatePoll):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a := newBatchTestAgent(t, be)
	a.pendingHoldMax = testPendingHoldLimit
	if _, err := a.DelegatedManager.Get(context.Background(), sk); err != nil {
		t.Fatalf("pre-register backend: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartInbox(ctx)

	done := make(chan error, 1)
	enqueued := time.Now()
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "async_notify",
		Run: func() {
			done <- a.HandleMessage(WithTrigger(ctx, "async_notify"), sk, []string{"[SPAWN RESULT] x completed"}, nil)
		},
	}})
	if !waitFor(2*time.Second, func() bool { return gateWatcherParked(a, sk) }) {
		t.Fatal("the inject never parked behind the pending background work")
	}

	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the inject was still held long after the pending-work limit (#2792)")
	}
	if err != nil {
		t.Fatalf("delivered turn: %v", err)
	}
	// A lower bound only: load can make delivery later, never earlier.
	if waited := time.Since(enqueued); waited < testPendingHoldLimit {
		t.Fatalf("inject ran after %v, before the %v limit — background work must still hold it until then", waited, testPendingHoldLimit)
	}
	exempt := false
	for _, inj := range be.InjectsRecorded() {
		if inj.PendingWorkExempt {
			exempt = true
		}
	}
	if !exempt {
		t.Fatal("the delivered turn reached the backend without PendingWorkExempt — ccstream would refuse it while work is pending")
	}
}

// TestRunInference_PendingRefusalEndsAfterLimit covers the backend layer on
// its own: a system turn that reaches RunInference without being parked (the
// work started between the gate check and the begin) and is refused for
// pending work retries, and once it has waited the limit it begins
// PendingWorkExempt instead of waiting for as long as the work runs (#2792).
func TestRunInference_PendingRefusalEndsAfterLimit(t *testing.T) {
	t.Parallel()
	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl", refusePending: true}
	be.setAwaiting(true)
	be.waitForTurnFn = func(ctx context.Context) error {
		// No turn is in flight: the retry wait is the loop's own backstop.
		select {
		case <-time.After(testGatePoll):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a := &Agent{Model: "test-model", DelegatedManager: newMockDelegatedManager(t, be), pendingHoldMax: testPendingHoldLimit}

	start := time.Now()
	runSystemTurn(t, a, "scheduled_wake")
	if waited := time.Since(start); waited < testPendingHoldLimit {
		t.Fatalf("system turn began after %v, before the %v limit", waited, testPendingHoldLimit)
	}
	injects := be.InjectsRecorded()
	if len(injects) < 2 {
		t.Fatalf("begin attempts = %d, want refusals then an exempt begin", len(injects))
	}
	if injects[0].PendingWorkExempt {
		t.Error("the first begin was already exempt — the limit must be waited first")
	}
	if !injects[len(injects)-1].PendingWorkExempt {
		t.Error("the begin that succeeded carried PendingWorkExempt=false")
	}
}
