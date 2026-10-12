package main

// #1286, gateway half: through the real notify wiring (newAsyncNotifier /
// newSessionNotifyFn / deliverToSessionChatThen, with agents whose turns
// actually run), these tests record what reaches the app chit deliverer and
// its order against the agents' turns:
//
//   - reply_to=caller: one target chit (raw message, caller ids) before the
//     target's turn; one caller chit (captured reply, target ids) before the
//     caller's relay turn; nothing from the target's reply in the target's
//     own chat (#480).
//   - reply_to=session and the oneshot downgrade: one target chit, no caller
//     chit — the downgrade's chit stays keyed to the target session even when
//     the turn's output falls back to the agent's primary connection.
//   - empty/silent replies: no caller chit, relay otherwise unchanged.
//   - plain async_notify results and ask_grader deliveries: no chit.
//   - an injection the inbox refuses during shutdown drain: no chit.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// ─── harness ───────────────────────────────────────────────────────────────

// chitEvent is one delivery the app chit deliverer received: the chat
// (session key) it was addressed to, and the agent message it carries.
type chitEvent struct {
	chat string
	msg  tools.AgentOrigin
}

// chitRecorder stands in for app.DeliverAgentMessage, recording every chit in
// arrival order; a test reads the snapshot at the moment it can prove the
// turn ordering (its backend parked mid-turn), so a chit that has NOT fired
// by then is caught.
type chitRecorder struct {
	mu     sync.Mutex
	events []chitEvent
}

func newChitRecorder() *chitRecorder {
	return &chitRecorder{}
}

func (r *chitRecorder) deliver(sessionKey string, msg tools.AgentOrigin) {
	r.mu.Lock()
	r.events = append(r.events, chitEvent{chat: sessionKey, msg: msg})
	r.mu.Unlock()
}

func (r *chitRecorder) snapshot() []chitEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]chitEvent(nil), r.events...)
}

// cannedClient is a provider.Client whose turns return a fixed reply.
// entered fires (with the turn's user text) the moment a turn reaches the
// backend, and proceed (when non-nil) parks it there — together they mark and
// hold the turn, so a test can prove what happened strictly BEFORE it started.
type cannedClient struct {
	mu       sync.Mutex
	reply    string
	lastText string
	calls    int
	entered  chan string
	proceed  chan struct{}
}

func (c *cannedClient) setReply(reply string) {
	c.mu.Lock()
	c.reply = reply
	c.mu.Unlock()
}

func (c *cannedClient) SendMessage(_ context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	var text string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			text = provider.TextOf(req.Messages[i].Content)
			break
		}
	}
	c.mu.Lock()
	c.lastText = text
	reply := c.reply
	c.calls++
	c.mu.Unlock()
	if c.entered != nil {
		c.entered <- text
	}
	if c.proceed != nil {
		<-c.proceed
	}
	return &provider.MessageResponse{
		ID:         "msg_canned",
		Type:       "message",
		Role:       "assistant",
		Content:    provider.TextContent(reply),
		StopReason: "end_turn",
	}, nil
}

func (c *cannedClient) CountTokens(context.Context, *provider.MessageRequest) (int, error) {
	return 10, nil
}

func (c *cannedClient) IsCachingAvailable() bool { return false }

// RetryBaseDelay satisfies provider.retryableClient so provider.Send keeps
// fast retries in tests.
func (c *cannedClient) RetryBaseDelay() time.Duration { return time.Millisecond }

func (c *cannedClient) stats() (lastText string, calls int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastText, c.calls
}

// chitConnMgr resolves connections per agent: sessionConns maps a FULL
// session key to a connection of its own (route.ConnFor's first rung),
// agentConns maps an agent id to its primary connection (the fallback rung).
// Leaving a session out of sessionConns forces the PolicyFallback rung, so a
// test can pin that a chit stays keyed to the session even when the turn's
// OUTPUT falls back to the agent's primary chat.
type chitConnMgr struct {
	sessionConns map[string]*recordingConn
	agentConns   map[string]*recordingConn
}

func (m chitConnMgr) Primary(string) platform.Connection { return nil }
func (m chitConnMgr) AllForAgent(agentID string) []platform.Connection {
	if c, ok := m.agentConns[agentID]; ok {
		return []platform.Connection{c}
	}
	return nil
}
func (m chitConnMgr) ForSession(key string) platform.Connection {
	if c, ok := m.sessionConns[key]; ok {
		return c
	}
	return nil // untyped nil — a missing entry must read as "no connection"
}
func (m chitConnMgr) ForSessionOrPrimary(_ string, agentID string) platform.Connection {
	if c, ok := m.agentConns[agentID]; ok {
		return c
	}
	return nil
}
func (m chitConnMgr) AcquireFacet(string) (platform.Connection, bool) { return nil, false }
func (m chitConnMgr) HasFacet(string) bool                            { return false }
func (m chitConnMgr) StartAll(context.Context)                        {}
func (m chitConnMgr) Wait()                                           {}

// newChitAgent builds a working API-loop agent whose turns produce client's
// canned reply — the same minimal wiring httpTestSetup proves a full turn
// needs — plus the shared session index, with its inbox workers running.
func newChitAgent(t *testing.T, client *cannedClient, idx *session.SessionIndex) *agent.Agent {
	t.Helper()
	ag := &agent.Agent{
		Client:    client,
		Sessions:  session.NewStore(t.TempDir()),
		Tools:     tools.NewRegistry(),
		Bootstrap: workspace.NewBootstrap(t.TempDir(), nil),
		Model:     "test-model",
	}
	ag.SessionIndex = idx
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ag.StartInbox(ctx)
	return ag
}

// chitFixture wires the two agents (caller "clutch", target "scout") through
// the real notify constructors, with a recorder standing in for
// app.DeliverAgentMessage.
type chitFixture struct {
	rec        *chitRecorder
	target     *agent.Agent
	targetCl   *cannedClient
	targetConn *recordingConn
	caller     *agent.Agent
	callerCl   *cannedClient
	callerConn *recordingConn
	resolver   func(agentID string) *agentInstance
	cm         chitConnMgr
	ctx        context.Context
}

// newChitFixture wires both agents' own connections (ForSession resolves), so
// turn output delivers to each session's own chat.
func newChitFixture(t *testing.T) *chitFixture {
	t.Helper()
	return newChitFixtureMode(t, true)
}

// newChitFixtureMode(withSessionConn=false) leaves the sessions without
// connections of their own, so turn output falls back to the agents' primary
// connections (route.PolicyFallback) while chits stay keyed to the sessions.
func newChitFixtureMode(t *testing.T, withSessionConn bool) *chitFixture {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	f := &chitFixture{
		rec:        newChitRecorder(),
		targetCl:   &cannedClient{reply: "the answer"},
		callerCl:   &cannedClient{reply: "relayed to the human"},
		targetConn: newRecordingConn("scout/c42"),
		callerConn: newRecordingConn("clutch/c1"),
		ctx:        context.Background(),
	}
	f.target = newChitAgent(t, f.targetCl, idx)
	f.caller = newChitAgent(t, f.callerCl, idx)

	sessionConns := map[string]*recordingConn{}
	if withSessionConn {
		sessionConns["scout/c42"] = f.targetConn
		sessionConns["clutch/c1"] = f.callerConn
	}
	cm := chitConnMgr{
		sessionConns: sessionConns,
		agentConns:   map[string]*recordingConn{"scout": f.targetConn, "clutch": f.callerConn},
	}

	f.resolver = func(agentID string) *agentInstance {
		switch agentID {
		case "scout":
			return &agentInstance{id: "scout", ag: f.target}
		case "clutch":
			return &agentInstance{id: "clutch", ag: f.caller}
		}
		return nil
	}
	f.cm = cm
	return f
}

// probeInbox enqueues a no-op injection behind everything already queued on
// ag's session and blocks until the inbox has drained past it — the inbox is
// FIFO per session, so a completed probe means everything before it ran. The
// deadline is a hang guard only.
func probeInbox(t *testing.T, ag *agent.Agent, sessionKey string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ag.EnqueueInjectWait(ctx, sessionKey, "probe", func() {}); err != nil {
		t.Fatalf("probe on %s: %v", sessionKey, err)
	}
}

// waitTurnStart blocks until client's backend is entered (a turn reached it),
// proving the turn started; pair with a non-nil proceed channel to hold it.
func waitTurnStart(t *testing.T, c *cannedClient) string {
	t.Helper()
	select {
	case text := <-c.entered:
		return text
	case <-time.After(10 * time.Second):
		t.Fatal("no turn reached the backend within 10s")
		return ""
	}
}

// ─── reply_to=caller: both chits, in order, #480 intact ────────────────────

// TestNewAsyncNotifier_AgentMessage_TargetChitBeforeTurnNoCallerChit drives
// the full reply_to=caller exchange through the real notify wiring:
//
//  1. the target chit (the caller's RAW message + caller ids) fires BEFORE
//     the target's turn — proven by holding the target's backend and seeing
//     the chit while the turn is parked;
//  2. the caller chit (the captured reply + target ids) fires BEFORE the
//     caller's relay turn — same proof on the caller's backend;
//  3. the target's chat sees NOTHING from its own reply (#480): the capture
//     sink keeps it out, so the target connection records no sends at all —
//     the chit is the only thing the target chat learns;
//  4. the relay itself is unchanged: the caller's agent receives the wrapped
//     "[SESSION RESPONSE]" injection, reply text verbatim.
func TestNewAsyncNotifier_AgentMessage_TargetChitBeforeTurnNoCallerChit(t *testing.T) {
	f := newChitFixture(t)
	// Hold both turns at their backend so each ordering assertion is made
	// while the turn it precedes is demonstrably in flight.
	f.targetCl.entered = make(chan string, 4)
	f.targetCl.proceed = make(chan struct{})
	f.callerCl.entered = make(chan string, 4)
	f.callerCl.proceed = make(chan struct{})

	notifier := newAsyncNotifier(func() *agent.Agent { return f.caller }, "clutch", f.resolver, f.ctx, f.cm, f.rec.deliver)
	notifier.InjectAgentMessage("scout/c42", "tagged injection body",
		"clutch/c1", tools.NewAgentOrigin("clutch/c1", "what changed overnight?"))

	// (1) Target turn reached its backend while parked — the chit must
	// already be recorded, and be the ONLY chit so far.
	waitTurnStart(t, f.targetCl)
	got := f.rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("chits while target turn in flight = %v, want exactly the target chit", got)
	}
	ev := got[0]
	if ev.chat != "scout/c42" {
		t.Errorf("target chit addressed to %q, want scout/c42 (the receiving session's chat)", ev.chat)
	}
	if ev.msg.Text != "what changed overnight?" {
		t.Errorf("target chit text = %q, want the caller's raw message", ev.msg.Text)
	}
	if ev.msg.AgentID != "clutch" || ev.msg.SessionKey != "clutch/c1" {
		t.Errorf("target chit ids = %q/%q, want clutch + clutch/c1", ev.msg.AgentID, ev.msg.SessionKey)
	}

	// Release the target; its reply is captured and relayed, which starts
	// the caller's relay turn.
	f.targetCl.proceed <- struct{}{}
	// (2) Caller turn reached its backend — the caller chit must already be
	// recorded (it fires at the start of that turn, before HandleMessage).
	relayText := waitTurnStart(t, f.callerCl)
	if !strings.Contains(relayText, "Response from session scout/c42") || !strings.Contains(relayText, "SESSION RESPONSE") {
		t.Errorf("caller's agent received %q, want the unchanged wrapped [SESSION RESPONSE] injection", relayText)
	}
	if !strings.Contains(relayText, "the answer") {
		t.Errorf("caller's relay text = %q, want it to carry the captured reply verbatim", relayText)
	}
	// The caller chit fires at the START of the caller's relay turn — the
	// turn is demonstrably in flight, so the chit must already be recorded.
	got = f.rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("chits = %d events, want 2 (target + caller) while the relay turn is in flight", len(got))
	}
	ev = got[1]
	if ev.chat != "clutch/c1" {
		t.Errorf("caller chit addressed to %q, want clutch/c1 (the asking session's chat)", ev.chat)
	}
	if ev.msg.Text != "the answer" {
		t.Errorf("caller chit text = %q, want the captured reply", ev.msg.Text)
	}
	if ev.msg.AgentID != "scout" || ev.msg.SessionKey != "scout/c42" {
		t.Errorf("caller chit ids = %q/%q, want scout + scout/c42", ev.msg.AgentID, ev.msg.SessionKey)
	}

	// Let the caller's turn finish, then drain both inboxes.
	f.callerCl.proceed <- struct{}{}
	probeInbox(t, f.target, "scout/c42")
	probeInbox(t, f.caller, "clutch/c1")

	// (3) #480: the target's chat never saw its own reply — no platform send
	// at all reached the target connection (the chit went through the
	// deliverer, not the connection).
	if _, texts := f.targetConn.snapshot(); len(texts) != 0 {
		t.Errorf("target chat received %q, want nothing (capture-and-relay keeps the reply out, #480)", texts)
	}
	// Exactly two chits in the whole exchange.
	if got := f.rec.snapshot(); len(got) != 2 {
		t.Errorf("chits = %d events, want exactly 2 (target + caller)", len(got))
	}
}

// TestNewAsyncNotifier_AgentMessage_EmptyReply_NoCallerChit: a target turn
// that captures nothing (empty reply) produces no caller chit and no caller
// relay turn at all — the target chit is the whole exchange.
func TestNewAsyncNotifier_AgentMessage_EmptyReply_NoCallerChit(t *testing.T) {
	f := newChitFixture(t)
	f.targetCl.setReply("")
	f.targetCl.entered = make(chan string, 4)

	notifier := newAsyncNotifier(func() *agent.Agent { return f.caller }, "clutch", f.resolver, f.ctx, f.cm, f.rec.deliver)
	notifier.InjectAgentMessage("scout/c42", "tagged injection body",
		"clutch/c1", tools.NewAgentOrigin("clutch/c1", "anyone there?"))

	waitTurnStart(t, f.targetCl)
	probeInbox(t, f.target, "scout/c42")
	probeInbox(t, f.caller, "clutch/c1")

	if got := f.rec.snapshot(); len(got) != 1 || got[0].chat != "scout/c42" {
		t.Fatalf("chits = %v, want exactly the target chit (empty reply → no caller chit)", got)
	}
	if _, calls := f.callerCl.stats(); calls != 0 {
		t.Errorf("caller backend entered %d turns, want 0 (nothing to relay)", calls)
	}
}

// ─── reply_to=session and the oneshot downgrade ─────────────────────────────

// TestNewSessionNotifyFn_AgentMessage_ChitBeforeTurn: the reply_to=session
// path delivers exactly one chit to the target session's chat, before the
// target's turn (proven with the backend held).
func TestNewSessionNotifyFn_AgentMessage_ChitBeforeTurn(t *testing.T) {
	f := newChitFixture(t)
	f.targetCl.entered = make(chan string, 4)
	f.targetCl.proceed = make(chan struct{})

	fn := newSessionNotifyFn(f.resolver, f.ctx, f.cm, "session_notify", f.rec.deliver)
	fn("scout/c42", "tagged injection body", &tools.AgentOrigin{SessionKey: "clutch/c1", AgentID: "clutch", Text: "for your human"})

	waitTurnStart(t, f.targetCl)
	got := f.rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("chits while target turn in flight = %v, want exactly one", got)
	}
	ev := got[0]
	if ev.chat != "scout/c42" || ev.msg.Text != "for your human" || ev.msg.AgentID != "clutch" || ev.msg.SessionKey != "clutch/c1" {
		t.Errorf("chit = %+v, want scout/c42 chat, raw text, clutch + clutch/c1 ids", ev)
	}

	f.targetCl.proceed <- struct{}{}
	probeInbox(t, f.target, "scout/c42")
	if got := f.rec.snapshot(); len(got) != 1 {
		t.Errorf("chits = %d, want exactly 1 for the whole reply_to=session delivery", len(got))
	}
}

// TestNewAsyncNotifier_DeliverBranch_TargetChitKeyedToSession: the downgraded
// (fire-and-forget, reply_to="") agent message still chits the target session
// — addressed by the SESSION key even though the turn's output has no session
// connection of its own and falls back to the agent's primary chat
// (route.PolicyFallback) — and never chits the caller.
func TestNewAsyncNotifier_DeliverBranch_TargetChitKeyedToSession(t *testing.T) {
	f := newChitFixtureMode(t, false)

	notifier := newAsyncNotifier(func() *agent.Agent { return f.caller }, "clutch", f.resolver, f.ctx, f.cm, f.rec.deliver)
	notifier.InjectAgentMessage("scout/c42", "tagged injection body", "", tools.NewAgentOrigin("clutch/c1", "fire and forget"))

	probeInbox(t, f.target, "scout/c42")
	probeInbox(t, f.caller, "clutch/c1")

	got := f.rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("chits = %v, want exactly one (target only, no caller chit on the downgraded delivery)", got)
	}
	if ev := got[0]; ev.chat != "scout/c42" || ev.msg.Text != "fire and forget" {
		t.Errorf("chit = %+v, want the target session's chat and the raw text", ev)
	}
	// The turn's OUTPUT still falls back to the agent's primary connection —
	// unchanged by the chit rule.
	if _, texts := f.targetConn.snapshot(); len(texts) == 0 {
		t.Error("target's primary connection saw no output, want the fallback delivery to keep working")
	}
}

// ─── relay chit details (relayCapturedResponse) ─────────────────────────────

// TestRelayCapturedResponse_ChitBeforeRelayTurn drives the relay choke point
// directly: the caller chit carries the reply with trailing silencing
// sentinels stripped and the TARGET's ids, fires before the caller's relay
// turn (proven with the backend held), while the relay text the caller's
// agent receives keeps the sentinel and the wrapper — relay unchanged.
func TestRelayCapturedResponse_ChitBeforeRelayTurn(t *testing.T) {
	f := newChitFixture(t)
	f.callerCl.entered = make(chan string, 4)
	f.callerCl.proceed = make(chan struct{})

	relayCapturedResponse(func() *agent.Agent { return f.caller }, f.ctx, f.cm, f.rec.deliver,
		"clutch", "clutch/c1", "scout/c42", "the answer\n[[NO_RESPONSE]]", "async_notify")

	relayText := waitTurnStart(t, f.callerCl)
	got := f.rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("chits while relay turn in flight = %v, want exactly the caller chit", got)
	}
	ev := got[0]
	if ev.chat != "clutch/c1" {
		t.Errorf("chit addressed to %q, want clutch/c1", ev.chat)
	}
	if ev.msg.Text != "the answer" {
		t.Errorf("chit text = %q, want the reply with the silencing sentinel stripped", ev.msg.Text)
	}
	if ev.msg.AgentID != "scout" || ev.msg.SessionKey != "scout/c42" {
		t.Errorf("chit ids = %q/%q, want scout + scout/c42 (the answering session)", ev.msg.AgentID, ev.msg.SessionKey)
	}
	// The relay itself is unchanged: verbatim reply, sentinel and all, inside
	// the wrapper.
	if !strings.Contains(relayText, "Response from session scout/c42:\nthe answer\n[[NO_RESPONSE]]") {
		t.Errorf("relay text = %q, want the verbatim captured reply in the wrapper", relayText)
	}

	f.callerCl.proceed <- struct{}{}
	probeInbox(t, f.caller, "clutch/c1")
}

// TestRelayCapturedResponse_SilentReply_NoChitButRelayRuns: a fully silent
// reply ([[NO_RESPONSE]]) produces no caller chit — there is nothing worth
// showing — but the relay to the caller's agent still runs, unchanged.
func TestRelayCapturedResponse_SilentReply_NoChitButRelayRuns(t *testing.T) {
	f := newChitFixture(t)
	f.callerCl.entered = make(chan string, 4)

	relayCapturedResponse(func() *agent.Agent { return f.caller }, f.ctx, f.cm, f.rec.deliver,
		"clutch", "clutch/c1", "scout/c42", "[[NO_RESPONSE]]", "async_notify")

	waitTurnStart(t, f.callerCl)
	probeInbox(t, f.caller, "clutch/c1")

	if got := f.rec.snapshot(); len(got) != 0 {
		t.Errorf("chits = %v, want none for a silent reply", got)
	}
	if _, calls := f.callerCl.stats(); calls != 1 {
		t.Errorf("caller backend entered %d turns, want 1 (the relay runs unchanged)", calls)
	}
}

// ─── no chit for non-agent injections (req 7) ───────────────────────────────

// TestNewAsyncNotifier_BackgroundAsyncNotify_NoChit: a plain background
// async_notify result (no origin — InjectToAgent) runs its injected turn but
// surfaces no chit.
func TestNewAsyncNotifier_BackgroundAsyncNotify_NoChit(t *testing.T) {
	f := newChitFixture(t)
	f.targetCl.entered = make(chan string, 4)

	notifier := newAsyncNotifier(func() *agent.Agent { return f.caller }, "clutch", f.resolver, f.ctx, f.cm, f.rec.deliver)
	notifier.InjectToAgent("scout/c42", "background result ready", "", "async_notify")

	waitTurnStart(t, f.targetCl)
	probeInbox(t, f.target, "scout/c42")

	if got := f.rec.snapshot(); len(got) != 0 {
		t.Errorf("chits = %v, want none (a background async result is not an agent message)", got)
	}
}

// TestNewAskDeliverFn_NoChit: the ask tool's answer/grader delivery shares
// deliverToResolvedSession but never chits — an ask answer is not an
// agent-to-agent message.
func TestNewAskDeliverFn_NoChit(t *testing.T) {
	f := newChitFixture(t)
	f.targetCl.entered = make(chan string, 4)

	deliver := newAskDeliverFn(f.resolver, f.ctx, f.cm)
	deliver("scout/c42", "req-1", "graded answer")

	waitTurnStart(t, f.targetCl)
	probeInbox(t, f.target, "scout/c42")

	if got := f.rec.snapshot(); len(got) != 0 {
		t.Errorf("chits = %v, want none (ask deliveries carry no chit)", got)
	}
}

// ─── timing: never when the inbox refuses (req 4) ───────────────────────────

// TestDeliverToSessionChatThen_ChitSkippedWhenInjectionRefused: an injection
// the inbox refuses during shutdown drain never runs its body, so its chit
// never fires — chits belong to the turn, not the enqueue.
func TestDeliverToSessionChatThen_ChitSkippedWhenInjectionRefused(t *testing.T) {
	ag := &agent.Agent{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ag.StartInbox(ctx)
	ag.BeginShutdown()

	rec := newChitRecorder()
	const sk = "test/main"
	deliverToSessionChatThen(ag, ctx, "scheduled_wake", stubConnMgr{}, "test", sk, "wake",
		newAgentChit(rec.deliver, tools.AgentOrigin{Text: "must never surface"}), "", nil)

	// The inbox is FIFO per session: once a probe behind the injection has
	// been refused, the injection has been processed too.
	pctx, pdone := context.WithTimeout(ctx, 10*time.Second)
	defer pdone()
	if err := ag.EnqueueInjectWait(pctx, sk, "probe", func() {}); !errors.Is(err, agent.ErrShuttingDown) {
		t.Fatalf("probe err = %v, want ErrShuttingDown", err)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("chits = %v, want none (the injection never ran)", got)
	}
}

// ─── nil guards ─────────────────────────────────────────────────────────────

// TestAgentChitFire_NilGuards covers the guard arms of agentChit.fire: a nil
// chit (every non-agent injection) and a chit with a nil deliverer (no app
// provider) are both silent no-ops, never panics.
func TestAgentChitFire_NilGuards(t *testing.T) {
	var chit *agentChit
	chit.fire("any/session") // nil chit must not panic

	newAgentChit(nil, tools.AgentOrigin{Text: "x"}).fire("any/session") // nil deliverer must not panic
}
