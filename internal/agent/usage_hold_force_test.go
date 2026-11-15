package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/platform"
	"foci/internal/turnevent"
	"foci/internal/usagehold"
)

// forceAgent is a started Claude Code agent subscribed to a fresh usage
// registry; told counts the "Held" notices its senders get.
func forceAgent(t *testing.T) (*Agent, *atomic.Int32) {
	t.Helper()
	a, cancel := startedAgent(t)
	t.Cleanup(cancel)
	a.UsageHoldRegistry = usagehold.New()
	a.DelegatedManager = &DelegatedManager{BackendType: "claude-code"}
	a.StartUsageHold()
	t.Cleanup(a.StopUsageHold)
	told := &atomic.Int32{}
	a.TurnQueuedNotifyFunc = func(_, text string) {
		if strings.Contains(text, "Held") {
			told.Add(1)
		}
	}
	return a, told
}

func texts(batch []Envelope) []string {
	out := make([]string, 0, len(batch))
	for _, e := range batch {
		out = append(out, e.Text)
	}
	return out
}

// TestLimitBreak_RunsOneMessagePastTheHold is #3128 requirements 1 and 2: in
// a held session a plain message parks, "/lb <text>" runs <text> (without the
// word) as a turn now, the next plain message parks again, and what was
// parked stays parked until the lift.
func TestLimitBreak_RunsOneMessagePastTheHold(t *testing.T) {
	a, told := forceAgent(t)
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)
	const sk = "test/s"

	a.ObserveUsage(weekly(0.98))
	a.Enqueue(Envelope{SessionKey: sk, Text: "M1", Driver: d})
	if !waitFor(2*time.Second, func() bool { return told.Load() == 1 }) {
		t.Fatalf("M1 was not held (told %d)", told.Load())
	}
	a.Enqueue(Envelope{SessionKey: sk, Text: "/lb hey one last thing", Driver: d})
	if !waitFor(2*time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("the /lb message ran no turn during the hold; calls=%d", d.NumCalls())
	}
	if got := texts(d.Calls()[0]); len(got) != 1 || got[0] != "hey one last thing" {
		t.Fatalf("/lb turn = %q, want only %q", got, "hey one last thing")
	}
	if a.UsageHold().Phase != usagehold.Hold {
		t.Fatalf("phase after /lb = %v, want the hold still on", a.UsageHold().Phase)
	}

	a.Enqueue(Envelope{SessionKey: sk, Text: "M3", Driver: d})
	if !waitFor(2*time.Second, func() bool { return told.Load() == 2 }) {
		t.Fatalf("the plain message after /lb was not held (told %d)", told.Load())
	}
	if d.NumCalls() != 1 {
		t.Fatalf("a held message ran a turn: %d calls", d.NumCalls())
	}

	a.ObserveUsage(weekly(0.10))
	if !waitFor(2*time.Second, func() bool { return d.NumCalls() == 2 }) {
		t.Fatalf("held messages did not run at lift; calls=%d", d.NumCalls())
	}
	if got := texts(d.Calls()[1]); len(got) != 2 || got[0] != "M1" || got[1] != "M3" {
		t.Errorf("lift turn = %q, want [M1 M3]", got)
	}
}

// TestLimitBreak_StopPhase: /lb forces through a usage stop too (the
// ticket's default).
func TestLimitBreak_StopPhase(t *testing.T) {
	a, _ := forceAgent(t)
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)
	a.ObserveUsage(usagehold.Event{Window: "seven_day", Reached: true, ResetAt: time.Now().Add(time.Hour)})
	if a.UsageHold().Phase != usagehold.Stop {
		t.Fatalf("phase = %v, want stop", a.UsageHold().Phase)
	}
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "/limit_break go", Driver: d})
	if !waitFor(2*time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("/limit_break ran no turn during a stop; calls=%d", d.NumCalls())
	}
	if got := texts(d.Calls()[0]); len(got) != 1 || got[0] != "go" {
		t.Errorf("turn = %q, want [go]", got)
	}
}

// TestLimitBreak_NoHold is requirement 5: with no hold, "/lb <text>" runs
// <text> as a normal message.
func TestLimitBreak_NoHold(t *testing.T) {
	a, told := forceAgent(t)
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "/LB  plain\nsecond line ", Driver: d})
	if !waitFor(2*time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatalf("no turn; calls=%d", d.NumCalls())
	}
	if got := texts(d.Calls()[0]); len(got) != 1 || got[0] != "plain\nsecond line" {
		t.Errorf("turn = %q, want [plain\\nsecond line]", got)
	}
	if told.Load() != 0 {
		t.Errorf("sender told %d held notices with no hold", told.Load())
	}
}

// TestLimitBreak_OnlyUserMessages: an automated send or an injection keeps a
// leading /lb as text and gets no pass.
func TestLimitBreak_OnlyUserMessages(t *testing.T) {
	for _, env := range []Envelope{
		{Text: "/lb x", Automated: true},
		{Text: "/lb", Steer: platform.SteerDefault},
		{Text: "please /lb x"},
	} {
		e := env
		markUsageForce(&e)
		if e.usageForce || e.Text != env.Text {
			t.Errorf("markUsageForce(%+v) = force %v text %q, want untouched", env, e.usageForce, e.Text)
		}
	}
}

// gateDriver blocks its first turn until release is closed or its ctx ends,
// and reports which on ended (true = the ctx ended first).
type gateDriver struct {
	recordingDriver
	started chan struct{}
	release chan struct{}
	ended   chan bool
	once    sync.Once
	ctxMu   sync.Mutex
	ctx     context.Context // the first turn's
}

func newGateDriver() *gateDriver {
	return &gateDriver{started: make(chan struct{}, 8), release: make(chan struct{}), ended: make(chan bool, 8)}
}

func (d *gateDriver) Connection() platform.Connection               { return nil }
func (d *gateDriver) NewTurnSink(Envelope) (turnevent.Sink, func()) { return nil, nil }
func (d *gateDriver) WrapTurn(ctx context.Context, fn func() error) error {
	if fire := OnPrimaryWrittenFromContext(ctx); fire != nil {
		fire()
	}
	first := false
	d.once.Do(func() {
		first = true
		d.ctxMu.Lock()
		d.ctx = ctx
		d.ctxMu.Unlock()
	})
	d.started <- struct{}{}
	if first {
		select {
		case <-ctx.Done():
			d.ended <- true
			return nil
		case <-d.release:
			d.ended <- false
		}
	}
	return fn()
}

// TestLimitBreak_NotInterruptedByHoldStart is requirement 3: a limit-break
// turn running when a hold starts is not interrupted by it and gets no resume
// line; it runs to completion.
func TestLimitBreak_NotInterruptedByHoldStart(t *testing.T) {
	a, _ := forceAgent(t)
	d := newGateDriver()
	a.SetTurnObserver(d.recordBatch)
	a.UsageHeldDriver = func(string) Driver { return d }
	const sk = "test/c1"

	a.Enqueue(Envelope{SessionKey: sk, Text: "/lb long job", Driver: d})
	select {
	case <-d.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the /lb turn never started")
	}
	a.ObserveUsage(weekly(0.98))
	if a.UsageHold().Phase != usagehold.Hold {
		t.Fatalf("phase = %v, want hold", a.UsageHold().Phase)
	}
	// The hold's interrupt runs synchronously in ObserveUsage (the phase
	// change): it cancels the turn's context and marks the resume line
	// before ObserveUsage returns, so both are visible now.
	d.ctxMu.Lock()
	turnCtx := d.ctx
	d.ctxMu.Unlock()
	if turnCtx.Err() != nil {
		t.Fatal("the hold start interrupted the /lb turn")
	}
	close(d.release)
	select {
	case interrupted := <-d.ended:
		if interrupted {
			t.Fatal("the /lb turn ended by its context, not by completing")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the /lb turn did not complete")
	}
	if inb := a.pinInbox(sk); true {
		resume := inb.usageResume.Load()
		inb.evictMu.RUnlock()
		if resume {
			t.Error("the hold marked the /lb turn for a resume line")
		}
	}
}

// TestLimitBreak_PlainMessagesDoNotJoin is requirement 2 inside the batch: a
// plain message queued behind a /lb message during the hold does not join
// its turn; it is held. A second /lb message does run.
func TestLimitBreak_PlainMessagesDoNotJoin(t *testing.T) {
	a, told := forceAgent(t)
	d := newGateDriver()
	a.SetTurnObserver(d.recordBatch)
	const sk = "test/c1"
	a.ObserveUsage(weekly(0.98))

	// The first /lb turn blocks, so the next two queue behind it.
	a.Enqueue(Envelope{SessionKey: sk, Text: "/lb first", Driver: d})
	select {
	case <-d.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first /lb turn never started")
	}
	a.Enqueue(Envelope{SessionKey: sk, Text: "/lb second", Driver: d})
	a.Enqueue(Envelope{SessionKey: sk, Text: "plain", Driver: d})
	close(d.release)

	if !waitFor(2*time.Second, func() bool { return d.NumCalls() == 2 && told.Load() == 1 }) {
		t.Fatalf("calls=%d told=%d, want the second /lb turn and one held notice", d.NumCalls(), told.Load())
	}
	if got := texts(d.Calls()[1]); len(got) != 1 || got[0] != "second" {
		t.Errorf("second turn = %q, want only [second]", got)
	}
}
