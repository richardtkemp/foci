package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/platform"
)

// Typing-indicator gate (#2320). Dick: "typing should not be sent for turns
// that no user message started." The DelegatedManager's typing wrapper drops
// the backend's typing calls while the session's current turn is one no human
// started; the agent sets that per turn (SetTypingAllowed).

// typingRecorder collects the manager's TypingFunc calls.
type typingRecorder struct {
	mu    sync.Mutex
	calls []bool
}

func (r *typingRecorder) fn(_ string, typing bool, _ *TypingStage) {
	r.mu.Lock()
	r.calls = append(r.calls, typing)
	r.mu.Unlock()
}

func (r *typingRecorder) got() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.calls...)
}

func (r *typingRecorder) reset() {
	r.mu.Lock()
	r.calls = nil
	r.mu.Unlock()
}

// typingBackendDT is mockBackendDT plus the typing behaviour of ccstream's
// beginTrackedTurn: every successful begin calls typing(true), whatever the
// turn's source. typing() calls the installed (manager-wrapped) func, as the
// backend's later events (OnAssistant, OnToolProgress, completeTurn) do.
type typingBackendDT struct {
	*mockBackendDT
	tmu sync.Mutex
	tf  func(bool)
}

func (b *typingBackendDT) SetTypingFunc(fn func(bool)) {
	b.tmu.Lock()
	b.tf = fn
	b.tmu.Unlock()
}

func (b *typingBackendDT) typing(on bool) {
	b.tmu.Lock()
	fn := b.tf
	b.tmu.Unlock()
	if fn != nil {
		fn(on)
	}
}

func (b *typingBackendDT) ImmediateInject(ctx context.Context, inj delegator.Inject) error {
	inFlight := b.IsTurnInFlight()
	err := b.mockBackendDT.ImmediateInject(ctx, inj)
	if err == nil && !inFlight {
		b.typing(true)
	}
	return err
}

// newTypingTestAgent builds an agent whose manager has TypingFunc wired
// BEFORE the session's backend is created (setBackendCallbacks installs the
// wrapper only when TypingFunc is set at Get time).
func newTypingTestAgent(t *testing.T) (*Agent, *typingBackendDT, *typingRecorder) {
	t.Helper()
	be := &typingBackendDT{mockBackendDT: &mockBackendDT{sessionFile: "/tmp/s.jsonl"}}
	rec := &typingRecorder{}
	mgr := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) { return be, nil },
		Spec:       mockSpecFor(be),
		TypingFunc: rec.fn,
	}
	if _, err := mgr.Get(context.Background(), "test/s"); err != nil {
		t.Fatalf("pre-register backend: %v", err)
	}
	return &Agent{Model: "test-model", DelegatedManager: mgr}, be, rec
}

func runTypingTurn(t *testing.T, a *Agent, ctx context.Context, trigger string) {
	t.Helper()
	ctx = WithTrigger(ctx, trigger)
	ts := NewTurnState(ctx, "test/s", []string{"prompt"}, nil)
	ts.Trigger = trigger
	ts.Prompt = "prompt"
	ts.StartedAt = time.Now()
	if err := (&DelegatedTransport{sharedTurnOps{agent: a}}).RunInference(ts); err != nil {
		t.Fatalf("RunInference(%s): %v", trigger, err)
	}
}

// TestTypingGate_SystemTurnsDriveNoTyping is the #2320 red test: a turn no
// human started (keepalive, reflection, wake, foci send, webhook) never
// reaches TypingFunc — neither the begin's typing(true) nor the backend's
// later typing(true)/typing(false) for that turn.
func TestTypingGate_SystemTurnsDriveNoTyping(t *testing.T) {
	t.Parallel()
	for _, trigger := range []string{TriggerKeepalive, "reflection", "scheduled_wake", "user", "webhook"} {
		t.Run(trigger, func(t *testing.T) {
			a, be, rec := newTypingTestAgent(t)
			runTypingTurn(t, a, context.Background(), trigger)
			be.typing(true)  // mid-turn event (OnAssistant / OnToolProgress)
			be.typing(false) // completeTurn
			if got := rec.got(); len(got) != 0 {
				t.Fatalf("trigger %q drove TypingFunc %v, want no calls (no user message started the turn)", trigger, got)
			}
		})
	}
}

// TestTypingGate_HumanTurnsDriveTyping is the regression arm: a turn a human
// started still drives the indicator, start to finish.
func TestTypingGate_HumanTurnsDriveTyping(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		ctx     context.Context
		trigger string
	}{
		{"voice", context.Background(), "voice"},
		{"ask answer", WithUserInput(context.Background()), "ask_answer"},
		// DECISION POINT (typingAllowedFor): a foci send carrying the Human
		// marker counts as a user message.
		{"human-marked send", WithHumanSource(context.Background()), "user"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, be, rec := newTypingTestAgent(t)
			runTypingTurn(t, a, c.ctx, c.trigger)
			be.typing(false)
			got := rec.got()
			if len(got) != 2 || !got[0] || got[1] {
				t.Fatalf("TypingFunc calls = %v, want [true false]", got)
			}
		})
	}
}

// TestTypingGate_UserInputAfterSystemTurnShowsTyping: a message arriving
// during a running keepalive no longer folds into it (#2591); it waits, begins
// its own turn, and that turn's reply gets the typing indicator.
func TestTypingGate_UserInputAfterSystemTurnShowsTyping(t *testing.T) {
	t.Parallel()
	a, be, rec := newTypingTestAgent(t)
	runTypingTurn(t, a, context.Background(), TriggerKeepalive)
	be.setTurnInFlight(true) // the keepalive turn is still running
	be.waitForTurnFn = func(context.Context) error {
		be.setTurnInFlight(false) // ... and ends
		return nil
	}

	runTypingTurn(t, a, context.Background(), "voice") // waits, then begins
	injects := be.InjectsRecorded()
	if len(injects) != 2 || injects[1].Source != delegator.SourceUser {
		t.Fatalf("injects = %d, want 2 (keepalive begin + the voice turn's own begin)", len(injects))
	}
	be.typing(true) // the reply's OnAssistant
	if got := rec.got(); len(got) != 2 || !got[0] || !got[1] {
		t.Fatalf("TypingFunc calls = %v, want [true true] (the begin, then the reply)", got)
	}
}

// TestTypingGate_WaitingSystemTurnLeavesRunningTurnTyping: a system turn that
// waits for a running user turn must not silence that turn's indicator while
// it waits; it takes the permission only when it begins.
func TestTypingGate_WaitingSystemTurnLeavesRunningTurnTyping(t *testing.T) {
	t.Parallel()
	a, be, rec := newTypingTestAgent(t)
	runTypingTurn(t, a, context.Background(), "voice") // user turn, typing on
	rec.reset()
	be.setTurnInFlight(true)

	var duringWait []bool
	be.waitForTurnFn = func(ctx context.Context) error {
		be.typing(true) // the running user turn's tool progress
		duringWait = rec.got()
		be.typing(false) // ... and its completion
		be.setTurnInFlight(false)
		return nil
	}
	runTypingTurn(t, a, context.Background(), "scheduled_wake")

	if len(duringWait) != 1 || !duringWait[0] {
		t.Fatalf("running user turn's typing while the wake waited = %v, want [true]", duringWait)
	}
	rec.reset()
	be.typing(true) // the wake's own turn, now begun
	if got := rec.got(); len(got) != 0 {
		t.Fatalf("the wake turn drove TypingFunc %v, want none", got)
	}
}

// TestTypingGate_RefusedKeepaliveRestoresTyping: a keepalive refused because a
// turn is in flight is skipped, and hands the typing permission back.
func TestTypingGate_RefusedKeepaliveRestoresTyping(t *testing.T) {
	t.Parallel()
	a, be, rec := newTypingTestAgent(t)
	runTypingTurn(t, a, context.Background(), "voice")
	be.setTurnInFlight(true)
	rec.reset()

	ctx := WithTrigger(context.Background(), TriggerKeepalive)
	ts := NewTurnState(ctx, "test/s", []string{"prompt"}, nil)
	ts.Trigger = TriggerKeepalive
	ts.Prompt = "prompt"
	ts.StartedAt = time.Now()
	_ = (&DelegatedTransport{sharedTurnOps{agent: a}}).RunInference(ts) // ErrKeepaliveSkipped

	be.typing(true)
	if got := rec.got(); len(got) != 1 || !got[0] {
		t.Fatalf("user turn typing after a refused keepalive = %v, want [true]", got)
	}
}

// typingAdopter is adoptingBackend plus ccstream's AdoptRunningTurn typing:
// an adopted run calls typing(true).
type typingAdopter struct {
	*adoptingBackend
	tmu sync.Mutex
	tf  func(bool)
}

func (b *typingAdopter) SetTypingFunc(fn func(bool)) {
	b.tmu.Lock()
	b.tf = fn
	b.tmu.Unlock()
}

func (b *typingAdopter) AdoptRunningTurn(turn *delegator.TurnEvents) bool {
	if !b.adoptingBackend.AdoptRunningTurn(turn) {
		return false
	}
	b.tmu.Lock()
	fn := b.tf
	b.tmu.Unlock()
	if fn != nil {
		fn(true)
	}
	return true
}

// TestTypingGate_AutonomousRuns pins the DECISION POINT in OpenAutonomousTurn:
// a delivering autonomous run shows typing; a record-only one (the follow-up
// of a keepalive's background work) does not.
func TestTypingGate_AutonomousRuns(t *testing.T) {
	for _, c := range []struct {
		name       string
		stamp      bool
		wantTyping bool
	}{
		{"delivering", false, true},
		{"record-only", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			initTestConvo(t, "helen")
			const sk = "helen/c42"
			be := &typingAdopter{adoptingBackend: &adoptingBackend{}}
			be.sessionFile = "/tmp/typing-auto.jsonl"
			a := newBatchTestAgent(t, be)
			rec := &typingRecorder{}
			a.DelegatedManager.TypingFunc = rec.fn
			a.ResolveLateConn = func(string) platform.Connection { return &leakConn{} }
			if _, err := a.DelegatedManager.Get(context.Background(), sk); err != nil {
				t.Fatalf("Get: %v", err)
			}
			var triggers []string
			if c.stamp {
				a.stampBgOrigin(sk, "toolu_BG", "keepalive", nil)
				triggers = []string{"toolu_BG"}
			}

			a.OpenAutonomousTurn(sk, be, triggers)
			defer be.completeAdopted("done")

			got := rec.got()
			if c.wantTyping && (len(got) != 1 || !got[0]) {
				t.Fatalf("delivering autonomous run TypingFunc calls = %v, want [true]", got)
			}
			if !c.wantTyping && len(got) != 0 {
				t.Fatalf("record-only autonomous run TypingFunc calls = %v, want none", got)
			}
		})
	}
}

// TestTypingWrapper_StallWarningNamesStage: a TypingFunc overrunning the
// typingFuncTimeout bound is logged with the step it stalled in and the
// session key, and no longer blames Telegram whatever the connection (#2320).
func TestTypingWrapper_StallWarningNamesStage(t *testing.T) {
	mgr, mocks := newTestManager(t, nil)
	hang := make(chan struct{})
	defer close(hang)
	// The bound is fired by the test (typingTimer seam, #2537) once the call
	// has entered the stage under test, instead of waiting out the real 2s.
	staged := make(chan struct{}, 1)
	mgr.TypingFunc = func(_ string, _ bool, stage *TypingStage) {
		stage.Set("connection lookup (test marker 2320)")
		staged <- struct{}{}
		<-hang
	}
	expire := make(chan time.Time, 1)
	mgr.typingTimer = func() <-chan time.Time { return expire }
	if _, err := mgr.Get(context.Background(), "test-agent/c2320"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	mock := (*mocks)[0]
	mock.mu.Lock()
	tf := mock.typingFunc
	mock.mu.Unlock()

	warned := make(chan string, 4)
	remove := log.AddTap(log.WARN, func(_ log.Level, _ string, msg string) {
		if strings.Contains(msg, "test-agent/c2320") {
			warned <- msg
		}
	})
	defer remove()

	go func() {
		<-staged
		expire <- time.Time{}
	}()
	// On its own goroutine, so a wrapper that never honours its bound fails
	// this test at the hang-guard below rather than hanging the package.
	go tf(true)
	select {
	case msg := <-warned:
		if !strings.Contains(msg, "stalled in: connection lookup (test marker 2320)") {
			t.Errorf("warning %q does not name the stalled stage", msg)
		}
		if strings.Contains(msg, "Telegram") {
			t.Errorf("warning %q still blames Telegram", msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no stall warning logged")
	}
}

// TestTypingGate_SteerIntoTypinglessRunShowsTyping: an urgent steer (the
// inbox's SourceSteer fold) into a running turn whose typing is off gets its
// reply's typing. A system turn is never steered (#2591), but the record-only
// run of a keepalive's background work is: the keepalive has completed, its
// typing is still suppressed, and the run is in flight.
func TestTypingGate_SteerIntoTypinglessRunShowsTyping(t *testing.T) {
	t.Parallel()
	a, be, rec := newTypingTestAgent(t)
	a.inboxCtx = context.Background()
	runTypingTurn(t, a, context.Background(), TriggerKeepalive)
	a.DelegatedManager.SetSystemTurn("test/s", false) // the keepalive completed
	be.setTurnInFlight(true)                          // its background run is in flight

	if !a.steerToBackend(be, Envelope{SessionKey: "test/s", Text: "also this"}) {
		t.Fatal("steer did not fold into the running turn")
	}
	be.typing(true) // the reply's OnAssistant
	if got := rec.got(); len(got) != 1 || !got[0] {
		t.Fatalf("TypingFunc calls after the steer = %v, want [true]", got)
	}
}
