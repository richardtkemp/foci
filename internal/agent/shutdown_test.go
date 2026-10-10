package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
)

// The #2059 drain gate lives at the backend-agnostic layer — the orchestrator,
// the inbox, post-turn and the DelegatedManager — so these tests drive it
// through a stub TurnContract / generic mock Delegator rather than any one
// backend. Whatever transport or delegated backend (claude-code, opencode,
// codex) sits underneath, the refusal happens before it is reached.

// countingContract is a stubContract that records whether the orchestrator
// reached RunInference and RunCompaction.
type countingContract struct {
	stubContract
	inference  atomic.Int32
	compaction atomic.Int32
}

func (c *countingContract) RunInference(ts *TurnState) error {
	c.inference.Add(1)
	close(ts.CompletionChan)
	return nil
}
func (c *countingContract) RunCompaction(*TurnState) { c.compaction.Add(1) }

func TestOrchestrateFullTurn_RefusesSystemTurnWhileShuttingDown(t *testing.T) {
	a := &Agent{}
	a.BeginShutdown()

	tc := &countingContract{}
	ctx := WithTrigger(context.Background(), "scheduled_wake")
	ts := NewTurnState(ctx, orchestratorTestKey, []string{"wake"}, nil)
	_, err := a.OrchestrateFullTurn(ctx, tc, ts)
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("OrchestrateFullTurn err = %v, want ErrShuttingDown", err)
	}
	if n := tc.inference.Load(); n != 0 {
		t.Fatalf("RunInference called %d time(s) for a system turn during shutdown; want 0", n)
	}
	if a.IsTurnInFlight(orchestratorTestKey) {
		t.Fatal("refused turn left the session marked in flight")
	}
}

// Real-time user input is not refused by the drain gate: it has no durable
// record to fall back on, so refusing it would lose the message.
func TestOrchestrateFullTurn_InteractiveTurnRunsWhileShuttingDown(t *testing.T) {
	a := &Agent{}
	a.BeginShutdown()

	tc := &countingContract{}
	ctx := WithTrigger(context.Background(), "voice")
	ts := NewTurnState(ctx, orchestratorTestKey, []string{"hi"}, nil)
	if _, err := a.OrchestrateFullTurn(ctx, tc, ts); err != nil {
		t.Fatalf("OrchestrateFullTurn: %v", err)
	}
	if n := tc.inference.Load(); n != 1 {
		t.Fatalf("RunInference called %d time(s) for an interactive turn; want 1", n)
	}
}

// A turn already running when shutdown begins finishes, but its post-turn
// compaction is skipped — it would run on a backend about to be closed.
func TestRunPostTurn_SkipsCompactionWhileShuttingDown(t *testing.T) {
	for _, shuttingDown := range []bool{false, true} {
		a := &Agent{}
		if shuttingDown {
			a.BeginShutdown()
		}
		tc := &countingContract{}
		ctx := WithTrigger(context.Background(), "voice")
		ts := NewTurnState(ctx, orchestratorTestKey, []string{"hi"}, nil)
		if _, err := a.OrchestrateFullTurn(ctx, tc, ts); err != nil {
			t.Fatalf("shuttingDown=%v: OrchestrateFullTurn: %v", shuttingDown, err)
		}
		want := int32(1)
		if shuttingDown {
			want = 0
		}
		if n := tc.compaction.Load(); n != want {
			t.Errorf("shuttingDown=%v: RunCompaction called %d time(s), want %d", shuttingDown, n, want)
		}
	}
}

// An injection reaching the inbox worker after shutdown began is not run, and
// a caller waiting on it is released with ErrShuttingDown.
func TestInbox_InjectionRefusedWhileShuttingDown(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.BeginShutdown()

	var ran atomic.Bool
	ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	err := a.EnqueueInjectWait(ctx, "test/imain", "scheduled_wake", func() { ran.Store(true) })
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("EnqueueInjectWait err = %v, want ErrShuttingDown", err)
	}
	if ran.Load() {
		t.Fatal("injection ran after shutdown began")
	}
}

// An injection held at the inject gate (behind background work that will not
// clear) is released by shutdown and refused, not left holding the worker.
func TestInbox_InjectGateReleasedByShutdown(t *testing.T) {
	be := &mockBackendDT{}
	be.setAwaiting(true) // background work pending — the gate holds
	a, cancel := startedAgent(t)
	defer cancel()
	a.DelegatedManager = newMockDelegatedManager(t, be)
	const sk = "test/s"

	var ran atomic.Bool
	errc := make(chan error, 1)
	go func() {
		errc <- a.EnqueueInjectWait(context.Background(), sk, "scheduled_wake", func() { ran.Store(true) })
	}()
	// Wait until the worker is actually holding it at the gate.
	if !waitFor(10*time.Second, func() bool { return a.lookupInbox(sk) != nil && len(a.lookupInbox(sk).ch) == 0 }) {
		t.Fatal("worker never dequeued the injection")
	}
	if ran.Load() {
		t.Fatal("premise failed: injection ran past a held gate")
	}
	a.BeginShutdown()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("EnqueueInjectWait err = %v, want ErrShuttingDown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("inject gate still holding 10s after shutdown began")
	}
	if ran.Load() {
		t.Fatal("injection ran after shutdown began")
	}
}

// The three tests below pin the ctx-done drop sites (#2059): an injection
// that is inside a gate when the inbox ctx ends — not via BeginShutdown,
// which opens the inject gate so runInject can refuse — must have Refused
// called by the dropping path, releasing an EnqueueInjectWait caller instead
// of leaving it to wait out its own ctx. One test per site: the dequeue
// re-login gate, the dequeue inject gate, and the post-batch held-injects
// gate. Each hands the worker a gated injection, ends the ctx, and asserts
// the caller is released promptly (a silent drop blocks until the caller's
// own ctx expires, so a short bound fails fast on a regression).

// Dequeue path: the injection is held in the re-login gate when the inbox
// ctx ends.
func TestInbox_InjectionRefusedWhenCtxEndsInReloginGate(t *testing.T) {
	a, cancel := startedAgent(t)
	a.DelegatedManager = &DelegatedManager{}
	claimRelogin(t)
	const sk = "test/s"

	var ran atomic.Bool
	errc := make(chan error, 1)
	go func() {
		ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		errc <- a.EnqueueInjectWait(ctx, sk, "scheduled_wake", func() { ran.Store(true) })
	}()
	// Wait until the worker has pulled the injection off the channel.
	if !waitFor(10*time.Second, func() bool { return a.lookupInbox(sk) != nil && len(a.lookupInbox(sk).ch) == 0 }) {
		t.Fatal("worker never dequeued the injection")
	}
	if ran.Load() {
		t.Fatal("premise failed: injection ran while the re-login gate was closed")
	}

	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("EnqueueInjectWait err = %v, want ErrShuttingDown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("injection dropped without Refused: caller never released after the ctx ended inside the re-login gate")
	}
	if ran.Load() {
		t.Fatal("injection ran although its gate never opened")
	}
}

// Dequeue path: the injection is held in the inject gate (a delivering
// autonomous run in flight) when the inbox ctx ends.
func TestInbox_InjectionRefusedWhenCtxEndsInInjectGate(t *testing.T) {
	a, cancel := startedAgent(t)
	const sk = "test/s"

	// Close the inject gate: a delivering autonomous run is in flight.
	releaseInFlight := a.markInFlight(sk, true)
	defer releaseInFlight()

	var ran atomic.Bool
	errc := make(chan error, 1)
	go func() {
		ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		errc <- a.EnqueueInjectWait(ctx, sk, "scheduled_wake", func() { ran.Store(true) })
	}()
	// inFlightChanged[sk] appears exactly when the worker enters the gate's
	// wait loop (InFlightWaitCh creates the entry; nothing else creates it
	// here) — proof the injection is parked INSIDE the gate, not merely
	// dequeued.
	if !waitFor(10*time.Second, func() bool {
		a.inFlightMu.Lock()
		defer a.inFlightMu.Unlock()
		_, gated := a.inFlightChanged[sk]
		return gated
	}) {
		t.Fatal("the injection never entered the inject gate")
	}
	if ran.Load() {
		t.Fatal("premise failed: injection ran past a held gate")
	}

	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("EnqueueInjectWait err = %v, want ErrShuttingDown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("injection dropped without Refused: caller never released after the ctx ended inside the inject gate")
	}
	if ran.Load() {
		t.Fatal("injection ran although its gate never opened")
	}
}

// Post-batch path: an injection drained alongside a user batch is held at
// the re-login gate — which closed while the batch's turn ran — when the
// inbox ctx ends.
func TestInbox_HeldInjectRefusedWhenCtxEndsInPostBatchGate(t *testing.T) {
	a, cancel := startedAgent(t)
	a.DelegatedManager = &DelegatedManager{} // the re-login hold is delegated-only
	const sk = "test/s"

	releaseTurn := make(chan struct{})
	d := &driverGated{ready: make(chan struct{}, 1), release: releaseTurn}
	a.Enqueue(Envelope{SessionKey: sk, Text: "turn", Driver: d})
	if !waitFor(10*time.Second, func() bool {
		select {
		case <-d.ready:
			return true
		default:
			return false
		}
	}) {
		t.Fatal("the user turn never started")
	}

	// An injection lands while the turn runs; a re-login then starts, so the
	// post-batch gate holds the injection once the turn returns.
	var ran atomic.Bool
	errc := make(chan error, 1)
	go func() {
		ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		errc <- a.EnqueueInjectWait(ctx, sk, "scheduled_wake", func() { ran.Store(true) })
	}()
	if !waitFor(10*time.Second, func() bool { return len(a.lookupInbox(sk).ch) == 1 }) {
		t.Fatal("the injection never reached the session channel")
	}
	claimRelogin(t)
	close(releaseTurn)
	// The worker drains the injection into the post-batch held set and parks
	// it at the now-closed re-login gate.
	if !waitFor(10*time.Second, func() bool { return len(a.lookupInbox(sk).ch) == 0 }) {
		t.Fatal("the injection was never drained into the post-batch hold")
	}
	if ran.Load() {
		t.Fatal("premise failed: the held injection ran while the re-login gate was closed")
	}

	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("EnqueueInjectWait err = %v, want ErrShuttingDown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("held injection dropped without Refused: caller never released after the ctx ended inside the post-batch gate")
	}
	if ran.Load() {
		t.Fatal("injection ran although its gate never opened")
	}
}

// The observed #2059 shape: a system turn is waiting in RunInference for an
// in-flight turn to finish; shutdown begins; the in-flight turn then ends. The
// system turn must not be dispatched into the draining session.
func TestDelegatedRunInference_SystemTurnWaitingIsRefusedOnShutdown(t *testing.T) {
	be := &mockBackendDT{turnInFlight: true}
	var dispatched atomic.Bool
	be.sendToPaneFn = func(_ context.Context, _ string, _ *mockHandler) (*delegator.TurnResult, error) {
		dispatched.Store(true)
		return nil, nil
	}
	waiting := make(chan struct{}, 1)
	be.waitForTurnFn = func(ctx context.Context) error {
		select {
		case waiting <- struct{}{}:
		default:
		}
		<-ctx.Done() // the in-flight turn outlasts every bounded wait
		return ctx.Err()
	}
	a := &Agent{Model: "test-model", DelegatedManager: newMockDelegatedManager(t, be)}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"wake"}, nil)
	ts.Prompt = "wake"
	ts.Trigger = "scheduled_wake"

	errc := make(chan error, 1)
	go func() { errc <- tr.RunInference(ts) }()
	select {
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("system turn never started waiting for the in-flight turn")
	}
	a.BeginShutdown()
	// The in-flight turn now ends: without the gate, the next retry dispatches.
	be.mu.Lock()
	be.turnInFlight = false
	be.mu.Unlock()

	select {
	case err := <-errc:
		if !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("RunInference err = %v, want ErrShuttingDown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunInference still waiting 10s after shutdown began")
	}
	if dispatched.Load() {
		t.Fatal("system turn was dispatched into the draining session")
	}
}

func TestDelegatedManager_NoBackendCreatedWhileShuttingDown(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(a *Agent)
	}{
		{"BeginShutdown", func(a *Agent) { a.BeginShutdown() }},
		// Production shutdown order: BeginShutdown, then Close.
		{"BeginShutdownThenClose", func(a *Agent) { a.BeginShutdown(); a.DelegatedManager.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			running := &mockBackendDT{}
			var created atomic.Int32
			mgr := &DelegatedManager{
				NewBackend: func() (delegator.Delegator, error) {
					created.Add(1)
					return running, nil
				},
			}
			a := &Agent{DelegatedManager: mgr}
			if _, err := mgr.Get(context.Background(), "test/live"); err != nil {
				t.Fatalf("premise Get: %v", err)
			}
			created.Store(0)
			tc.stop(a)

			if _, err := mgr.Get(context.Background(), "test/new"); !errors.Is(err, ErrShuttingDown) {
				t.Fatalf("Get(new session) err = %v, want ErrShuttingDown", err)
			}
			if n := created.Load(); n != 0 {
				t.Fatalf("NewBackend called %d time(s) after %s; want 0", n, tc.name)
			}
		})
	}
}

// Draining refuses NEW backends only: a turn already in flight keeps the
// running backend it is using.
func TestDelegatedManager_RunningBackendStillServedWhileShuttingDown(t *testing.T) {
	running := &mockBackendDT{}
	mgr := &DelegatedManager{NewBackend: func() (delegator.Delegator, error) { return running, nil }}
	a := &Agent{DelegatedManager: mgr}
	if _, err := mgr.Get(context.Background(), "test/live"); err != nil {
		t.Fatalf("premise Get: %v", err)
	}
	a.BeginShutdown()
	be, err := mgr.Get(context.Background(), "test/live")
	if err != nil || be != running {
		t.Fatalf("Get(running session) = %v, %v; want the running backend", be, err)
	}
}

func TestMarkTurnDispatched_StampsOnce(t *testing.T) {
	a := &Agent{}
	a.markTurnDispatched(nil) // nil-safe
	td := &TurnDetail{StartTime: time.Now().Add(-time.Minute)}
	a.markTurnDispatched(td)
	first := td.DispatchedAt
	if first.IsZero() {
		t.Fatal("DispatchedAt not stamped")
	}
	a.markTurnDispatched(td)
	if !td.DispatchedAt.Equal(first) {
		t.Fatal("DispatchedAt re-stamped by a second call")
	}
}

// Close alone is "the gateway closed its backends" (the L2 harness's
// CloseAgentBackend), not shutdown: the next Get must respawn. Only
// BeginShutdown latches refusal (#2059 regression: Close used to latch it,
// failing TestL2_Failures_BackendKilledMidTurnByGateway).
func TestDelegatedManager_CloseAloneAllowsRespawn(t *testing.T) {
	var created atomic.Int32
	mgr := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			created.Add(1)
			return &mockBackendDT{}, nil
		},
	}
	if _, err := mgr.Get(context.Background(), "test/s"); err != nil {
		t.Fatalf("premise Get: %v", err)
	}
	mgr.Close()
	if _, err := mgr.Get(context.Background(), "test/s"); err != nil {
		t.Fatalf("Get after Close = %v, want a respawned backend", err)
	}
	if n := created.Load(); n != 2 {
		t.Fatalf("NewBackend called %d times, want 2 (initial + respawn)", n)
	}
}
