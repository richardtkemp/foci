package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/usagehold"
	"foci/internal/workspace"
)

// failingClient is a backend that cannot run a turn: every call errors.
type failingClient struct {
	cannedClient
	err error
}

func (c *failingClient) SendMessage(context.Context, *provider.MessageRequest) (*provider.MessageResponse, error) {
	return nil, c.err
}

// unavailableFixture is a chit fixture whose caller side records what the
// caller's agent receives: each notice runs a turn on the caller.
func unavailableFixture(t *testing.T) *chitFixture {
	t.Helper()
	f := newChitFixture(t)
	f.callerCl.entered = make(chan string, 8)
	return f
}

// waitCallerNotice waits for a turn on the caller and returns its text.
func waitCallerNotice(t *testing.T, f *chitFixture) string {
	t.Helper()
	select {
	case text := <-f.callerCl.entered:
		return text
	case <-time.After(10 * time.Second):
		t.Fatal("the caller was told nothing")
		return ""
	}
}

// expectNoCallerTurn fails if the caller gets a turn within a short window.
func expectNoCallerTurn(t *testing.T, f *chitFixture, why string) {
	t.Helper()
	select {
	case text := <-f.callerCl.entered:
		t.Errorf("the caller got a turn (%s): %q", why, text)
	case <-time.After(300 * time.Millisecond):
	}
}

// putOnHold makes ag a delegated-account agent whose account holds.
func putOnHold(t *testing.T, ag *agent.Agent, reached bool) {
	t.Helper()
	ag.UsageHoldRegistry = usagehold.New()
	ag.DelegatedManager = &agent.DelegatedManager{BackendType: "claude-code"}
	ag.ObserveUsage(usagehold.Event{Window: "seven_day", Util: 0.99, HasUtil: true, Reached: reached, ResetAt: time.Now().Add(time.Hour)})
	if ag.UsageHold().Phase == usagehold.Open {
		t.Fatal("the fixture's account did not hold")
	}
}

func callerOrigin(text string) tools.AgentOrigin {
	o := tools.NewAgentOrigin("clutch/c1", text)
	o.NotifyCaller = true
	return o
}

func ptrOrigin(o tools.AgentOrigin) *tools.AgentOrigin { return &o }

// waitCond polls cond until it holds or d passes.
func waitCond(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// setCallerWatchTiming shortens the watch's times for one test.
func setCallerWatchTiming(poll, stall time.Duration) func() {
	callerWatchSeams.mu.Lock()
	defer callerWatchSeams.mu.Unlock()
	oldPoll, oldStall := callerWatchSeams.poll, callerWatchSeams.stall
	callerWatchSeams.poll, callerWatchSeams.stall = poll, stall
	return func() {
		callerWatchSeams.mu.Lock()
		defer callerWatchSeams.mu.Unlock()
		callerWatchSeams.poll, callerWatchSeams.stall = oldPoll, oldStall
	}
}

// setTargetActivity replaces the target activity read for one test.
func setTargetActivity(fn func(*agent.Agent, string) time.Time) func() {
	callerWatchSeams.mu.Lock()
	defer callerWatchSeams.mu.Unlock()
	old := callerWatchSeams.activity
	callerWatchSeams.activity = fn
	return func() {
		callerWatchSeams.mu.Lock()
		defer callerWatchSeams.mu.Unlock()
		callerWatchSeams.activity = old
	}
}

// TestRelay_DeadBackend_CallerToldAtOnce: reply_to=caller, and the target's
// backend fails the turn. The caller gets a turn saying so.
func TestRelay_DeadBackend_CallerToldAtOnce(t *testing.T) {
	f := unavailableFixture(t)
	idx, err := session.NewSessionIndex(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	dead := &agent.Agent{
		Client:    &failingClient{err: errors.New("backend is gone")},
		Sessions:  session.NewStore(t.TempDir()),
		Tools:     tools.NewRegistry(),
		Bootstrap: workspace.NewBootstrap(t.TempDir(), nil),
		Model:     "test-model",
	}
	dead.SessionIndex = idx
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dead.StartInbox(ctx)
	f.resolver = func(id string) *agentInstance {
		if id == "scout" {
			return &agentInstance{id: "scout", ag: dead}
		}
		return &agentInstance{id: "clutch", ag: f.caller}
	}

	n := newAsyncNotifier(func() *agent.Agent { return f.caller }, "clutch", f.resolver, f.ctx, f.cm, f.rec.deliver)
	n.InjectAgentMessage("scout/c42", "body", "clutch/c1", callerOrigin("hello?"))

	got := waitCallerNotice(t, f)
	if !strings.Contains(got, "SESSION UNAVAILABLE") || !strings.Contains(got, "scout/c42") {
		t.Errorf("caller told %q, want a notice naming the target", got)
	}
	if !strings.Contains(got, "send it again") {
		t.Errorf("caller told %q, want it to say the message was not delivered", got)
	}
}

// TestRelay_HoldAtSend_ToolToldNoTurnAndDeliveredAtLift: a hold known at send
// is told in the tool result, so no extra turn runs on the caller, and the
// message still runs at lift.
func TestRelay_HoldAtSend_ToolToldNoTurnAndDeliveredAtLift(t *testing.T) {
	f := unavailableFixture(t)
	putOnHold(t, f.target, false)
	f.target.StartUsageHold()
	t.Cleanup(f.target.StopUsageHold)
	n := newAsyncNotifier(func() *agent.Agent { return f.caller }, "clutch", f.resolver, f.ctx, f.cm, f.rec.deliver)
	reason, err := n.CheckTarget("scout/c42")
	if err != nil || !strings.Contains(reason, "usage hold") {
		t.Fatalf("CheckTarget = %q, %v; want the hold named", reason, err)
	}
	o := callerOrigin("hello?")
	o.CallerTold = true
	n.InjectAgentMessage("scout/c42", "body", "clutch/c1", o)
	expectNoCallerTurn(t, f, "the tool result already told it")
	if got := f.rec.snapshot(); len(got) != 0 {
		t.Fatalf("chits %v during the hold: the message ran", got)
	}
	f.target.ObserveUsage(usagehold.Event{Window: "seven_day", Util: 0.1, HasUtil: true, ResetAt: time.Now().Add(time.Hour)})
	if !waitCond(10*time.Second, func() bool { return len(f.rec.snapshot()) > 0 }) {
		t.Fatal("the held message did not run at lift")
	}
}

// TestRelay_HoldAfterSend_CallerTold: the hold begins after the send, while
// the message waits behind a running turn: the caller is told it waits.
func TestRelay_HoldAfterSend_CallerTold(t *testing.T) {
	defer setCallerWatchTiming(10*time.Millisecond, time.Hour)()
	f := unavailableFixture(t)
	f.target.UsageHoldRegistry = usagehold.New()
	f.target.DelegatedManager = &agent.DelegatedManager{BackendType: "claude-code"}
	release, running := make(chan struct{}), make(chan struct{})
	defer close(release)
	f.target.Enqueue(agent.Envelope{SessionKey: "scout/c42", Inject: &agent.InjectMeta{Trigger: "probe", Run: func() {
		close(running)
		<-release
	}}})
	<-running
	n := newAsyncNotifier(func() *agent.Agent { return f.caller }, "clutch", f.resolver, f.ctx, f.cm, f.rec.deliver)
	n.InjectAgentMessage("scout/c42", "body", "clutch/c1", callerOrigin("hello?"))
	f.target.ObserveUsage(usagehold.Event{Window: "seven_day", Reached: true, ResetAt: time.Now().Add(time.Hour)})
	got := waitCallerNotice(t, f)
	if !strings.Contains(got, "usage limit reached") || !strings.Contains(got, "delivered when") {
		t.Errorf("caller told %q, want the limit named and the delivery promise", got)
	}
}

// TestRelay_CallerOnSameHold_NoTurn: the caller's own account holds as well,
// so a notice could only run after the lift, beside the reply itself.
func TestRelay_CallerOnSameHold_NoTurn(t *testing.T) {
	defer setCallerWatchTiming(10*time.Millisecond, time.Hour)()
	f := unavailableFixture(t)
	putOnHold(t, f.target, false)
	putOnHold(t, f.caller, false)
	// A notice would park on the caller's held list and run no turn, so watch
	// the enqueue: no turn alone cannot tell "not sent" from "parked".
	var mu sync.Mutex
	enqueued := 0
	f.caller.SetEnqueueObserver(func(agent.Envelope) { mu.Lock(); enqueued++; mu.Unlock() })
	n := newAsyncNotifier(func() *agent.Agent { return f.caller }, "clutch", f.resolver, f.ctx, f.cm, f.rec.deliver)
	n.InjectAgentMessage("scout/c42", "body", "clutch/c1", callerOrigin("hello?"))
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if enqueued != 0 {
		t.Errorf("%d injections queued on the caller, want none: its own account holds", enqueued)
	}
}

// TestRelay_QueueNeverDrains_TellsOnce: the message cannot start and the
// target shows no activity. The caller is told once, and the message stays.
func TestRelay_QueueNeverDrains_TellsOnce(t *testing.T) {
	defer setCallerWatchTiming(10*time.Millisecond, 50*time.Millisecond)()
	f := unavailableFixture(t)
	f.targetCl.entered = make(chan string, 4)
	f.targetCl.proceed = make(chan struct{}, 4)
	deliverToSessionChat(f.target, f.ctx, "async_notify", f.cm, "scout", "scout/c42", "wedged turn", nil, "")
	waitTurnStart(t, f.targetCl)
	n := newAsyncNotifier(func() *agent.Agent { return f.caller }, "clutch", f.resolver, f.ctx, f.cm, f.rec.deliver)
	n.InjectAgentMessage("scout/c42", "body", "clutch/c1", callerOrigin("hello?"))
	got := waitCallerNotice(t, f)
	if !strings.Contains(got, "no activity") || !strings.Contains(got, "still queued") {
		t.Errorf("caller told %q, want no activity and still queued", got)
	}
	expectNoCallerTurn(t, f, "the stall is told once")
	f.targetCl.proceed <- struct{}{}
	f.targetCl.proceed <- struct{}{}
}

// TestRelay_SlowTurnWithActivity_NoNotice: Dick's ruling, a slow turn does
// not count. A target that keeps showing activity gets no notice however long
// the message waits.
func TestRelay_SlowTurnWithActivity_NoNotice(t *testing.T) {
	defer setCallerWatchTiming(10*time.Millisecond, 50*time.Millisecond)()
	defer setTargetActivity(func(*agent.Agent, string) time.Time { return time.Now() })()
	f := unavailableFixture(t)
	f.targetCl.entered = make(chan string, 4)
	f.targetCl.proceed = make(chan struct{}, 4)
	deliverToSessionChat(f.target, f.ctx, "async_notify", f.cm, "scout", "scout/c42", "slow turn", nil, "")
	waitTurnStart(t, f.targetCl)
	n := newAsyncNotifier(func() *agent.Agent { return f.caller }, "clutch", f.resolver, f.ctx, f.cm, f.rec.deliver)
	n.InjectAgentMessage("scout/c42", "body", "clutch/c1", callerOrigin("hello?"))
	expectNoCallerTurn(t, f, "a slow turn does not count")
	f.targetCl.proceed <- struct{}{}
	f.targetCl.proceed <- struct{}{}
}

// TestRelay_Working_NoNotice: the plain exchange gets the reply and no notice.
func TestRelay_Working_NoNotice(t *testing.T) {
	f := unavailableFixture(t)
	n := newAsyncNotifier(func() *agent.Agent { return f.caller }, "clutch", f.resolver, f.ctx, f.cm, f.rec.deliver)
	n.InjectAgentMessage("scout/c42", "body", "clutch/c1", callerOrigin("hello?"))
	got := waitCallerNotice(t, f)
	if strings.Contains(got, "SESSION UNAVAILABLE") || !strings.Contains(got, "SESSION RESPONSE") {
		t.Errorf("caller got %q, want the reply only", got)
	}
	expectNoCallerTurn(t, f, "one reply, no notice")
}

// TestSessionNotify_FullInbox_CallerTold: reply_to=session, and the target's
// inbox refuses the message. The caller is told it was not delivered, though
// a reply was never coming to it.
func TestSessionNotify_FullInbox_CallerTold(t *testing.T) {
	f := unavailableFixture(t)
	f.targetCl.entered = make(chan string, 200)
	f.targetCl.proceed = make(chan struct{}, 200)
	deliverToSessionChat(f.target, f.ctx, "session_notify", f.cm, "scout", "scout/c42", "busy", nil, "")
	waitTurnStart(t, f.targetCl)
	for i := 0; i < 64; i++ {
		deliverToSessionChat(f.target, f.ctx, "session_notify", f.cm, "scout", "scout/c42", "filler", nil, "")
	}
	notify := newSessionNotifyFn(f.resolver, f.ctx, f.cm, "session_notify", f.rec.deliver)
	o := callerOrigin("hello?")
	notify("scout/c42", "body", &o)
	got := waitCallerNotice(t, f)
	if !strings.Contains(got, "not delivered") {
		t.Errorf("caller told %q, want not delivered", got)
	}
	for i := 0; i < 66; i++ {
		f.targetCl.proceed <- struct{}{}
	}
}

// TestSessionNotify_NoCallerToTell_NoTurn: an agent message with no caller to
// tell (a oneshot's, a spawn's) never produces a caller turn.
func TestSessionNotify_NoCallerToTell_NoTurn(t *testing.T) {
	f := unavailableFixture(t)
	putOnHold(t, f.target, false)
	notify := newSessionNotifyFn(f.resolver, f.ctx, f.cm, "session_notify", f.rec.deliver)
	o := tools.NewAgentOrigin("clutch/b123", "hello?")
	notify("scout/c42", "body", &o)
	expectNoCallerTurn(t, f, "NotifyCaller is off")
}

// TestCheckTarget_UnknownAgentRefused: a key that names no agent is an error,
// where the tool used to report it as sent.
func TestCheckTarget_UnknownAgentRefused(t *testing.T) {
	f := unavailableFixture(t)
	f.resolver = func(string) *agentInstance { return nil }
	n := newAsyncNotifier(func() *agent.Agent { return f.caller }, "clutch", f.resolver, f.ctx, f.cm, f.rec.deliver)
	if _, err := n.CheckTarget("ghost/c9"); err == nil {
		t.Error("CheckTarget(unknown agent) = nil error, want a refusal")
	}
	if _, err := n.CheckTarget("clutch/c1"); err != nil {
		t.Errorf("CheckTarget(own agent) = %v, want nil", err)
	}
}

// TestRelay_HeldSurvivesRestart: a reply_to=caller message a hold parks is
// stored, and a new process's copy still takes its reply to the caller. Before
// #2892 it had no durable form, so a restart during a hold lost it.
func TestRelay_HeldSurvivesRestart(t *testing.T) {
	f := unavailableFixture(t)
	f.target.AgentID = "scout"
	putOnHold(t, f.target, false)
	n := newAsyncNotifier(func() *agent.Agent { return f.caller }, "clutch", f.resolver, f.ctx, f.cm, f.rec.deliver)
	o := callerOrigin("hello?")
	o.CallerTold = true
	n.InjectAgentMessage("scout/c42", "body", "clutch/c1", o)
	idx := f.target.SessionIndex
	if !waitCond(10*time.Second, func() bool {
		keys, err := idx.SessionKeysWithMetadata(session.MetaKeyUsageHeld)
		return err == nil && len(keys) == 1
	}) {
		t.Fatal("the parked message was not stored")
	}

	next := newChitAgent(t, &cannedClient{reply: "the answer after restart"}, idx)
	next.AgentID = "scout"
	resolver := func(id string) *agentInstance {
		if id == "scout" {
			return &agentInstance{id: "scout", ag: next}
		}
		return &agentInstance{id: "clutch", ag: f.caller}
	}
	wireUsageHeldRestore(next, f.ctx, f.cm, "scout", resolver, f.rec.deliver)
	next.RestoreUsageHeld()
	got := waitCallerNotice(t, f)
	if !strings.Contains(got, "SESSION RESPONSE") || !strings.Contains(got, "the answer after restart") {
		t.Errorf("caller got %q, want the restored message's captured reply", got)
	}
	if !waitCond(10*time.Second, func() bool {
		keys, err := idx.SessionKeysWithMetadata(session.MetaKeyUsageHeld)
		return err == nil && len(keys) == 0
	}) {
		t.Error("the stored record stayed after the restored message ran")
	}
}

// TestCallerWatch_TurnEnded covers each end of a target's turn, and the
// refusal arm: what the caller is told, or that it is told nothing.
func TestCallerWatch_TurnEnded(t *testing.T) {
	limit := &agent.RateLimitedError{Until: time.Now().Add(time.Hour), Endpoint: "ep"}
	for _, tc := range []struct {
		name     string
		hold     bool
		shutdown bool
		act      func(*callerWatch)
		want     string // "" = no caller turn
	}{
		{name: "reply", act: func(w *callerWatch) { w.turnEnded(nil, true) }},
		{name: "empty reply, available", act: func(w *callerWatch) { w.turnEnded(nil, false) }},
		{name: "rate limited", act: func(w *callerWatch) { w.turnEnded(limit, false) }, want: "will not come to you"},
		{name: "failed", act: func(w *callerWatch) { w.turnEnded(errors.New("boom"), false) }, want: "boom"},
		{name: "empty reply in a hold", hold: true, act: func(w *callerWatch) { w.turnEnded(nil, false) }, want: "stopped working"},
		{name: "failed in a hold", hold: true, act: func(w *callerWatch) { w.turnEnded(errors.New("boom"), false) }, want: "stopped working"},
		{name: "shutting down, open", shutdown: true, act: func(w *callerWatch) { w.turnEnded(agent.ErrShuttingDown, false) }, want: "not delivered"},
		{name: "refused at shutdown, stored by the hold", hold: true, shutdown: true, act: func(w *callerWatch) { w.refused() }},
		{name: "refused, full inbox in a hold", hold: true, act: func(w *callerWatch) { w.refused() }, want: "not delivered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := unavailableFixture(t)
			if tc.hold {
				putOnHold(t, f.target, false)
			}
			if tc.shutdown {
				f.target.BeginShutdown()
			}
			tc.act(newCallerWatch(f.ctx, f.cm, "async_notify", f.caller, "clutch", "clutch/c1", f.target, "scout/c42", true, false))
			if tc.want == "" {
				expectNoCallerTurn(t, f, "nothing to tell")
				return
			}
			if got := waitCallerNotice(t, f); !strings.Contains(got, tc.want) {
				t.Errorf("caller told %q, want %q", got, tc.want)
			}
		})
	}
}
