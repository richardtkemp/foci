package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"foci/internal/platform"
	"foci/internal/turnevent"
	"foci/internal/usagehold"
)

// blockingDriver is a Driver whose FIRST turn blocks until its ctx ends (a
// chat turn in flight), recording every batch it is given.
type blockingDriver struct {
	mu      sync.Mutex
	calls   [][]Envelope
	started chan struct{}
	ended   chan error
}

func newBlockingDriver() *blockingDriver {
	return &blockingDriver{started: make(chan struct{}, 8), ended: make(chan error, 8)}
}

func (d *blockingDriver) Connection() platform.Connection { return nil }
func (d *blockingDriver) NewTurnSink(Envelope) (turnevent.Sink, func()) {
	return nil, nil
}
func (d *blockingDriver) WrapTurn(ctx context.Context, fn func() error) error {
	d.mu.Lock()
	first := len(d.calls) == 1
	d.mu.Unlock()
	if fire := OnPrimaryWrittenFromContext(ctx); fire != nil {
		fire()
	}
	d.started <- struct{}{}
	if first {
		<-ctx.Done()
		d.ended <- ctx.Err()
		return nil
	}
	return fn()
}
func (d *blockingDriver) record(_ string, batch []Envelope) {
	d.mu.Lock()
	d.calls = append(d.calls, batch)
	d.mu.Unlock()
}
func (d *blockingDriver) Calls() [][]Envelope {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]Envelope(nil), d.calls...)
}

// TestUsageHold_InterruptAndFold is holdout H6 and worked example C: a chat
// turn in flight when the 0.98 event arrives is interrupted; M1 and M2 sent
// during the hold are held (the sender is told) and run no turn; at lift ONE
// turn runs whose input is the resume line, then M1, then M2.
func TestUsageHold_InterruptAndFold(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.UsageHoldRegistry = usagehold.New()
	a.DelegatedManager = &DelegatedManager{BackendType: "claude-code"}
	a.StartUsageHold()
	defer a.StopUsageHold()
	d := newBlockingDriver()
	a.SetTurnObserver(d.record)
	a.UsageHeldDriver = func(string) Driver { return d }
	var mu sync.Mutex
	var told []string
	a.TurnQueuedNotifyFunc = func(_, text string) { mu.Lock(); told = append(told, text); mu.Unlock() }

	const sk = "test/c1"
	a.Enqueue(Envelope{SessionKey: sk, Text: "M0", Driver: d})
	select {
	case <-d.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first turn never started")
	}

	a.ObserveUsage(weekly(0.98))
	select {
	case err := <-d.ended:
		if err == nil {
			t.Fatal("turn ended without its context ending")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the turn in flight was not interrupted by the hold")
	}
	a.Enqueue(Envelope{SessionKey: sk, Text: "M1", Driver: d})
	a.Enqueue(Envelope{SessionKey: sk, Text: "M2", Driver: d})
	if waitFor(300*time.Millisecond, func() bool { return len(d.Calls()) > 1 }) {
		t.Fatalf("a turn ran during the hold: %+v", d.Calls()[1:])
	}
	if !waitFor(time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(told) == 2 }) {
		t.Errorf("sender told %q, want one held notice per message (none for the resume line)", told)
	}

	a.ObserveUsage(weekly(0.10))
	if !waitFor(2*time.Second, func() bool { return len(d.Calls()) == 2 }) {
		t.Fatalf("turns after lift = %d, want exactly one more", len(d.Calls())-1)
	}
	got := d.Calls()[1]
	if len(got) != 3 || !got[0].usageResume || got[0].Text != UsageResumeLine || got[1].Text != "M1" || got[2].Text != "M2" {
		var texts []string
		for _, e := range got {
			texts = append(texts, e.Text)
		}
		t.Fatalf("lift turn = %q, want [resume line, M1, M2]", texts)
	}
	if got[0].Driver != d {
		t.Error("the resume line did not take the held messages' driver")
	}
	if waitFor(300*time.Millisecond, func() bool { return len(d.Calls()) > 2 }) {
		t.Errorf("more than one turn at lift: %d", len(d.Calls()))
	}
}

// TestUsageHold_ResumeOnlyTurn: an interrupted session with no held messages
// still gets the resume line alone at lift; a session with neither gets no
// turn (requirement 4).
func TestUsageHold_ResumeOnlyTurn(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.UsageHoldRegistry = usagehold.New()
	a.DelegatedManager = &DelegatedManager{BackendType: "claude-code"}
	a.StartUsageHold()
	defer a.StopUsageHold()
	d := newBlockingDriver()
	a.SetTurnObserver(d.record)
	a.UsageHeldDriver = func(string) Driver { return d }

	a.Enqueue(Envelope{SessionKey: "test/c1", Text: "M0", Driver: d})
	select {
	case <-d.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first turn never started")
	}
	a.ObserveUsage(weekly(0.98))
	select {
	case <-d.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("the turn in flight was not interrupted by the hold")
	}
	a.ObserveUsage(weekly(0.10))
	if !waitFor(2*time.Second, func() bool { return len(d.Calls()) == 2 }) {
		t.Fatalf("turns = %d, want the interrupted one and its resume", len(d.Calls()))
	}
	if got := d.Calls()[1]; len(got) != 1 || !got[0].usageResume || got[0].SessionKey != "test/c1" {
		t.Errorf("resume turn = %+v, want the resume line alone on test/c1", got)
	}
}

// TestUsageHold_TurnRegistry: a turn registered through UsageHoldTurn is
// interrupted at a hold start and says so; one that ends before the hold is
// not; a hold that starts while the account already holds interrupts nothing.
func TestUsageHold_TurnRegistry(t *testing.T) {
	a := holdAgent(usagehold.New(), "claude-code")
	a.StartUsageHold()
	defer a.StopUsageHold()

	early, doneEarly := a.UsageHoldTurn(context.Background(), "test/c9", "spawn")
	if doneEarly() {
		t.Error("a turn that ended before the hold reads as interrupted")
	}
	if early.Err() == nil {
		t.Error("the context of an ended registration is still live")
	}

	ctx, done := a.UsageHoldTurn(context.Background(), "test/c2/b1", "spawn")
	a.ObserveUsage(weekly(0.98))
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the registered turn was not interrupted")
	}
	if !errors.Is(context.Cause(ctx), ErrUsageHoldInterrupted) {
		t.Errorf("cause = %v, want ErrUsageHoldInterrupted", context.Cause(ctx))
	}
	if !done() {
		t.Error("done() = false for an interrupted turn")
	}

	late, doneLate := a.UsageHoldTurn(context.Background(), "test/c3", "spawn")
	a.ObserveUsage(weekly(0.99)) // still holding: no new start
	time.Sleep(50 * time.Millisecond)
	if late.Err() != nil || doneLate() {
		t.Error("a usage event during the hold interrupted a turn")
	}

	if a.WaitUsageLift(canceledCtx()) {
		t.Error("WaitUsageLift returned true while held")
	}
	a.ObserveUsage(weekly(0.10))
	if !a.WaitUsageLift(context.Background()) {
		t.Error("WaitUsageLift returned false after the lift")
	}
}

func canceledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestUsageHold_ResumeLineHiddenFromRecord: the conversation record (the
// clients' history) of the lift turn holds the held messages and not the
// resume line; a turn of the resume line alone records nothing.
func TestUsageHold_ResumeLineHiddenFromRecord(t *testing.T) {
	const agentID = "hiddenresume"
	rows := openSteerRecvLog(t, agentID)
	ops := &sharedTurnOps{}
	ctx := withHiddenLeadingTexts(context.Background(), 1)
	ts := NewTurnState(ctx, agentID+"/c1", []string{UsageResumeLine, "M1", "M2"}, nil)
	ts.Trigger = "app"
	ts.Meta = &TurnMetadata{}
	ops.LogConversationRecv(ts)
	ts2 := NewTurnState(ctx, agentID+"/c1", []string{UsageResumeLine}, nil)
	ts2.Trigger = "app"
	ts2.Meta = &TurnMetadata{}
	ops.LogConversationRecv(ts2)
	got := rows()
	if len(got) != 1 || got[0].Text != "M1\nM2" {
		t.Fatalf("recv rows = %+v, want one row \"M1\\nM2\"", got)
	}
	if n := leadingUsageResume([]Envelope{{usageResume: true}, {Text: "M1"}}); n != 1 {
		t.Errorf("leadingUsageResume = %d, want 1", n)
	}
}

// TestUsageHold_OrchestratorBackstop is the fork-mode flag of the handoff: a
// periodic turn that reaches the backend outside the inbox (a fork-mode
// reflection, consolidation or background turn) does not start in a hold
// unless the hold started it; a keepalive runs in a hold and not in a stop;
// user turns and remind fires are not this backstop's business.
func TestUsageHold_OrchestratorBackstop(t *testing.T) {
	a := holdAgent(usagehold.New(), "claude-code")
	turn := func(trigger string, permitted bool) error {
		ctx := context.Background()
		if permitted {
			ctx = usagehold.WithPermit(ctx)
		}
		ts := NewTurnState(ctx, "claude-code-agent/c1", []string{"x"}, nil)
		ts.Trigger = trigger
		return a.usageHoldRefusesTurn(ts)
	}
	for _, tr := range []string{"reflection", "consolidation", "background", "keepalive"} {
		if err := turn(tr, false); err != nil {
			t.Errorf("open account: %s refused: %v", tr, err)
		}
	}
	a.ObserveUsage(weekly(0.98))
	for _, tr := range []string{"reflection", "consolidation", "background"} {
		if !errors.Is(turn(tr, false), ErrUsageHeld) {
			t.Errorf("hold: %s without the permit started", tr)
		}
	}
	if err := turn("reflection", true); err != nil {
		t.Errorf("hold: the hold's own reflection refused: %v", err)
	}
	if err := turn("keepalive", false); err != nil {
		t.Errorf("hold: keepalive refused: %v", err)
	}
	if err := turn("app", false); err != nil {
		t.Errorf("hold: a user turn refused by the backstop (the inbox holds it): %v", err)
	}
	a.ObserveUsage(usagehold.Event{Window: "seven_day", Reached: true, ResetAt: time.Now().Add(time.Hour)})
	if !errors.Is(turn("keepalive", false), ErrUsageHeld) {
		t.Error("stop: a keepalive started")
	}
	if !errors.Is(turn("reflection", true), ErrUsageHeld) {
		t.Error("stop: a permitted reflection started")
	}
}

// TestUsageHold_RemindRunsWithoutHeldMessage is holdout H8's first half and
// worked example C: with M1 held on session C, a remind fire on C runs as its
// own turn without M1; at lift M1 runs.
func TestUsageHold_RemindRunsWithoutHeldMessage(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	a.UsageHoldRegistry = usagehold.New()
	a.DelegatedManager = &DelegatedManager{BackendType: "claude-code"}
	a.StartUsageHold()
	defer a.StopUsageHold()
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)

	const sk = "test/c1"
	a.ObserveUsage(weekly(0.98))
	a.Enqueue(Envelope{SessionKey: sk, Text: "M1", Driver: d})
	var wake sync.WaitGroup
	wake.Add(1)
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{Trigger: "scheduled_wake", Run: func() { wake.Done() }}})
	done := make(chan struct{})
	go func() { wake.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the remind fire did not run during the hold")
	}
	if n := d.NumCalls(); n != 0 {
		t.Fatalf("the held message ran with or beside the remind fire: %d user turns", n)
	}
	a.ObserveUsage(weekly(0.10))
	if !waitFor(2*time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("M1 did not run at lift; turns=%d", d.NumCalls())
	}
	if got := d.Calls()[0]; len(got) != 1 || got[0].Text != "M1" {
		t.Errorf("lift turn = %+v, want M1 alone", got)
	}
}
