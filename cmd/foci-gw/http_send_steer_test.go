package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/tools"
)

// steerEnvelopeObserver wires a turn observer that captures each driven
// batch's first envelope (under a mutex — the observer fires on the session
// worker).
func steerEnvelopeObserver(d httpHandlerDeps) (observe func() []agent.Envelope) {
	var mu sync.Mutex
	var seen []agent.Envelope
	d.agents[testAgentID].ag.SetTurnObserver(func(sk string, batch []agent.Envelope) {
		if len(batch) > 0 {
			mu.Lock()
			seen = append(seen, batch[0])
			mu.Unlock()
		}
	})
	return func() []agent.Envelope {
		mu.Lock()
		defer mu.Unlock()
		return append([]agent.Envelope(nil), seen...)
	}
}

// waitForCond polls cond until it holds or the deadline passes (a deadline is
// only a hang-guard; the condition itself is the observable).
func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within deadline: %s", what)
}

// TestSend_Steer_ReachesEnvelope proves the /send steer field reaches the
// dispatched envelope (#1318 req 2): a choice-carrying async send dispatches
// as a USER envelope through the inbox (observable as a driven batch), with
// the wire value mapped by the same platform mapping the app path uses.
// An absent or unknown value keeps today's injection dispatch — no user
// envelope exists there — and the text still reaches the backend.
func TestSend_Steer_ReachesEnvelope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		body      string
		wantSteer platform.SteerPreference
		wantUser  bool
	}{
		{"steer", `{"text":"redirect","async":true,"steer":"steer"}`, platform.SteerAlways, true},
		{"queue", `{"text":"wait for it","async":true,"steer":"queue"}`, platform.SteerNever, true},
		{"absent keeps today's injection", `{"text":"plain","async":true}`, platform.SteerDefault, false},
		{"unknown keeps today's injection", `{"text":"plain","async":true,"steer":"bogus"}`, platform.SteerDefault, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, mock := httpTestSetup(t, httpTestOpts{})
			mock.entered = make(chan string, 4)
			observe := steerEnvelopeObserver(d)
			mux := newTestMux(d)

			w := postJSON(mux, "/send", tc.body)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
			}
			select {
			case <-mock.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("send never reached the backend")
			}

			if tc.wantUser {
				waitForCond(t, "a user-envelope batch drove", func() bool { return len(observe()) > 0 })
				envs := observe()
				if envs[0].Steer != tc.wantSteer {
					t.Errorf("envelope Steer = %v, want %v", envs[0].Steer, tc.wantSteer)
				}
				if envs[0].Text == "" {
					t.Error("envelope text empty — the message never rode the envelope")
				}
				return
			}
			// Today's injection dispatch: no user envelope exists. Bounded
			// negative window (the serialisation tests' pattern) so a stray
			// envelope cannot pass vacuously.
			time.Sleep(200 * time.Millisecond)
			if envs := observe(); len(envs) != 0 {
				t.Errorf("arms without a choice must keep the injection dispatch; user envelopes drove: %+v", envs)
			}
		})
	}
}

// TestSend_Queue_MidTurn_QueuesAsUserTurn proves --queue semantics over
// HTTP: while a turn runs, a choice-carrying send returns its 202 at once and
// its turn runs only AFTER the in-flight one — never folding into it. The
// queued turn's trigger is "" (the httpDriver exposes no platform), the same
// non-interactive classification today's injected /send turns have.
func TestSend_Queue_MidTurn_QueuesAsUserTurn(t *testing.T) {
	t.Parallel()
	d, mock := httpTestSetup(t, httpTestOpts{})
	mock.entered = make(chan string)   // unbuffered: receive == a turn is inside the backend
	mock.proceed = make(chan struct{}) // each turn blocks until released
	mux := newTestMux(d)

	firstDone := make(chan struct{})
	go func() { postJSON(mux, "/send", `{"text":"first"}`); close(firstDone) }()
	select {
	case <-mock.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first turn never reached the backend")
	}
	waitForCond(t, "first turn in flight", func() bool { return d.agents[testAgentID].ag.IsTurnInFlight(testSessionKey) })

	w := postJSON(mux, "/send", `{"text":"second","async":true,"steer":"queue"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}

	// The queued choice turn must not reach the backend while the first holds.
	select {
	case text := <-mock.entered:
		t.Fatalf("choice turn (%q) entered the backend while the first was in flight", text)
	case <-time.After(200 * time.Millisecond):
	}

	mock.proceed <- struct{}{}
	<-firstDone
	select {
	case text := <-mock.entered:
		if !strings.HasSuffix(text, "second") {
			t.Errorf("second turn text = %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("choice turn never ran after the first released")
	}
	mock.proceed <- struct{}{}

	waitForCond(t, "both turns recorded", func() bool { return len(mock.snapshot()) == 2 })
	if calls := mock.snapshot(); calls[1].trigger != "" {
		t.Errorf("queued choice turn trigger = %q, want \"\" (non-interactive, like today's injected sends)", calls[1].trigger)
	}
}

// holdOpenTool registers a tool whose FIRST execution blocks until released
// — the seam that keeps a turn mid-flight AFTER its primary write
// (turn-active is what makes a SteerAlways message fold instead of queue).
// Later executions return immediately, so draining the session after the
// release is not re-parked.
func holdOpenTool(ag *agent.Agent, entered, release chan struct{}) {
	var once sync.Once
	ag.Tools.Register(&tools.Tool{
		Name:        "hold_open",
		Description: "hold the turn open mid-tool for tests",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			once.Do(func() {
				entered <- struct{}{}
				<-release
			})
			return tools.TextResult("released"), nil
		},
	})
}

// foldingClient scripts a two-round turn: round 1 answers with a tool_use
// (the primary write flips the inbox's turn-active gate once it returns),
// the tool blocks until the test releases it, and round 2 ends the turn with
// the canned reply. Delegates to the harness mock for call recording.
type foldingClient struct {
	*mockClient
}

func (f *foldingClient) SendMessage(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	round := len(f.snapshot())
	resp, err := f.mockClient.SendMessage(ctx, req)
	if err != nil {
		return nil, err
	}
	if round == 0 {
		resp.Content = []provider.ContentBlock{{Type: "tool_use", ID: "tu_hold", Name: "hold_open", Input: json.RawMessage(`{}`)}}
		resp.StopReason = "tool_use"
	}
	return resp, nil
}

// TestSend_Steer_MidTurn_FoldsIntoRunningTurn proves --steer semantics over
// HTTP: a steer choice arriving while a turn is mid-tool (past its primary
// write, so the inbox's steer gate is open) returns its 202 and the text is
// drained into the SAME turn's next backend request as a [user] block — the
// fold — with no fresh turn of its own.
func TestSend_Steer_MidTurn_FoldsIntoRunningTurn(t *testing.T) {
	t.Parallel()
	d, _ := httpTestSetup(t, httpTestOpts{})
	fc := &foldingClient{mockClient: &mockClient{}}
	d.agents[testAgentID].ag.Client = fc
	toolEntered := make(chan struct{})
	toolRelease := make(chan struct{})
	holdOpenTool(d.agents[testAgentID].ag, toolEntered, toolRelease)
	mux := newTestMux(d)

	firstDone := make(chan struct{})
	// The seeding send itself carries a queue choice so it dispatches as a
	// USER envelope: only a user batch runs under the worker ctx that
	// registers OnPrimaryWritten, which is what opens the inbox's steer gate
	// once round 1's primary is written. (An injected sync send would never
	// flip turn-active — the #777 window would stay closed for the whole
	// turn.)
	go func() { postJSON(mux, "/send", `{"text":"first","async":true,"steer":"queue"}`); close(firstDone) }()
	select {
	case <-toolEntered: // round 1 completed (primary written) and the tool is holding the turn open
	case <-time.After(5 * time.Second):
		t.Fatal("first turn never reached its tool")
	}
	waitForCond(t, "turn-active gate open", func() bool { return d.agents[testAgentID].ag.InboxTurnActive(testSessionKey) })

	w := postJSON(mux, "/send", `{"text":"redirect this","async":true,"steer":"steer"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}

	close(toolRelease)
	<-firstDone
	waitForCond(t, "turn completed", func() bool { return len(fc.snapshot()) == 2 })

	// The fold: round 2's request — the SAME turn, not a fresh one — carries
	// the steered text as a [user] block.
	calls := fc.snapshot()
	if len(calls) != 2 {
		t.Fatalf("backend calls = %d, want exactly 2 (fold, no fresh turn)", len(calls))
	}
	joined := strings.Join(calls[1].allTexts, "\n")
	if !strings.Contains(joined, "[user] redirect this") {
		t.Errorf("round-2 request = %q, want it to carry the steered text as a [user] block", joined)
	}
}

// TestSend_Steer_MidTurn_InjectedTurn_DoesNotFold pins the fold matrix the
// docs state (#1318 review F2): --steer folds only into a running USER turn.
// An injected turn (a plain modeless send — likewise keepalive, cron, webhook)
// never opens the inbox's steer window (turnActive opens only under a user
// batch), so a steer choice arriving while one runs is NOT folded: it waits
// for the turn to finish and then runs as its own fresh turn. The idle half
// of the matrix (delivers at once) is TestSend_SteerChoice_IdleAsyncDeliversOnce.
func TestSend_Steer_MidTurn_InjectedTurn_DoesNotFold(t *testing.T) {
	t.Parallel()
	d, _ := httpTestSetup(t, httpTestOpts{})
	fc := &foldingClient{mockClient: &mockClient{}}
	d.agents[testAgentID].ag.Client = fc
	toolEntered := make(chan struct{})
	toolRelease := make(chan struct{})
	holdOpenTool(d.agents[testAgentID].ag, toolEntered, toolRelease)
	observe := steerEnvelopeObserver(d)
	mux := newTestMux(d)

	// A PLAIN modeless async send: today's injected dispatch — a system
	// turn, never a user batch.
	firstDone := make(chan struct{})
	go func() { postJSON(mux, "/send", `{"text":"first","async":true}`); close(firstDone) }()
	select {
	case <-toolEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("injected turn never reached its tool")
	}
	// The crux: the turn is in flight but the inbox's steer window is NOT
	// open — only a user batch opens it, so a steer cannot fold here.
	waitForCond(t, "injected turn in flight", func() bool { return d.agents[testAgentID].ag.IsTurnInFlight(testSessionKey) })
	if d.agents[testAgentID].ag.InboxTurnActive(testSessionKey) {
		t.Fatal("an injected turn must not open the inbox steer window — --steer cannot fold into it")
	}

	w := postJSON(mux, "/send", `{"text":"redirect this","async":true,"steer":"steer"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}

	// No fold: while the injected turn is held mid-tool the steer text
	// reaches no backend request (a fold would fire the turn's next round).
	time.Sleep(200 * time.Millisecond)
	if calls := fc.snapshot(); len(calls) != 1 {
		t.Fatalf("backend calls = %d while the injected turn is held, want 1 (no fold)", len(calls))
	}

	// Release: the injected turn finishes, then the steer runs as its OWN
	// fresh turn after it — never as a [user] block inside the first turn.
	close(toolRelease)
	<-firstDone
	waitForCond(t, "steer's fresh turn ran", func() bool { return len(fc.snapshot()) == 3 })
	calls := fc.snapshot()
	if joined := strings.Join(calls[1].allTexts, "\n"); strings.Contains(joined, "redirect this") {
		t.Errorf("injected turn's finishing round carries the steer text (%q) — it must not fold", joined)
	}
	if joined := strings.Join(calls[2].allTexts, "\n"); !strings.Contains(joined, "redirect this") {
		t.Errorf("fresh turn text = %q, want it to carry the steer text as its own turn", joined)
	}
	// And the steer still dispatched as a user envelope, not an injection.
	for _, env := range observe() {
		if env.Steer == platform.SteerAlways {
			return
		}
	}
	t.Errorf("driven envelopes = %+v, want a SteerAlways user envelope", observe())
}

// TestSend_HelpOnHTTP_HidesChatOnlyPrefixes proves /help teaches the
// /steer|/queue prefixes only on surfaces that parse them (#1318 review F1):
// on HTTP the mode is the /send steer field and the bare words are unknown
// commands, so the help listing reached over HTTP must not advertise the
// chat-prefix syntax.
func TestSend_HelpOnHTTP_HidesChatOnlyPrefixes(t *testing.T) {
	t.Parallel()
	d, _ := httpTestSetup(t, httpTestOpts{})
	reg := d.agents[testAgentID].cmds
	reg.Register(command.HelpCommand(reg))
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"/help","async":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "/help") {
		t.Fatalf("body = %q, want the help listing itself", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "/queue <message>") {
		t.Errorf("/help on HTTP teaches the chat-only prefixes:\n%s", w.Body.String())
	}
}

// TestSend_SyncSteerChoice_InFlight_Returns202AndStampsUserActivity proves
// the sync matrix row (#1318 decision 2) plus the #1130 carry: --sync with a
// choice while a turn runs answers the 202 accepted receipt IMMEDIATELY (the
// reply belongs to the turns, and blocking could hang on a raced fold), and
// once the queued turn runs its own entry write stamps the durable
// last_user_activity_at via Envelope.Human — the human marker must survive
// the user-envelope dispatch.
func TestSend_SyncSteerChoice_InFlight_Returns202AndStampsUserActivity(t *testing.T) {
	t.Parallel()
	d, mock := httpTestSetup(t, httpTestOpts{})
	mock.entered = make(chan string)   // unbuffered
	mock.proceed = make(chan struct{}) // hold the first turn open
	mux := newTestMux(d)

	firstDone := make(chan struct{})
	go func() { postJSON(mux, "/send", `{"text":"first"}`); close(firstDone) }()
	select {
	case <-mock.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first turn never reached the backend")
	}
	waitForCond(t, "first turn in flight", func() bool { return d.agents[testAgentID].ag.IsTurnInFlight(testSessionKey) })

	// The choice send must answer 202 immediately — a blocking reply would
	// hang on a raced fold. Run it in a goroutine with a bound so a
	// regression to blocking fails cleanly instead of deadlocking the test.
	type reply struct{ w *httptest.ResponseRecorder }
	choiceDone := make(chan reply, 1)
	go func() {
		w := postJSON(mux, "/send", `{"text":"human choice","sync":true,"human":true,"steer":"queue"}`)
		choiceDone <- reply{w}
	}()
	var w *httptest.ResponseRecorder
	select {
	case r := <-choiceDone:
		w = r.w
	case <-time.After(5 * time.Second):
		t.Fatal("sync choice send blocked for a reply while a turn was in flight — it must answer 202 immediately")
	}
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 while a turn is in flight; body: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, `"queued"`) {
		t.Errorf("body = %q, want the queued acceptance receipt", body)
	}

	mock.proceed <- struct{}{}
	<-firstDone
	select {
	case <-mock.entered: // the queued choice turn reached the backend → its entry write ran
	case <-time.After(5 * time.Second):
		t.Fatal("queued choice turn never ran")
	}
	mock.proceed <- struct{}{}
	waitForCond(t, "durable user-activity stamp", func() bool { return userActivityRecorded(d, testSessionKey) })
}

// TestSend_SteerChoice_SyncIdle_KeepsBlockingReply pins the documented sync
// exception (#1318): with an idle session a --sync choice send keeps today's
// blocking contract — 200 with the reply body — because with nothing in
// flight the choice cannot matter.
func TestSend_SteerChoice_SyncIdle_KeepsBlockingReply(t *testing.T) {
	t.Parallel()
	d, mock := httpTestSetup(t, httpTestOpts{})
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"hi","sync":true,"steer":"queue"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), mockReply) {
		t.Errorf("body = %q, want the blocking reply %q", w.Body.String(), mockReply)
	}
	if calls := mock.snapshot(); len(calls) != 1 {
		t.Errorf("backend calls = %d, want 1", len(calls))
	}
}

// TestSend_SteerChoice_Broadcast_KeepsTodayPath pins the broadcast exception
// (#1318): with policy=broadcast the fan-out contract wins and the mode is
// inert — the request keeps today's injection dispatch (trigger "user", not
// the user envelope's "").
func TestSend_SteerChoice_Broadcast_KeepsTodayPath(t *testing.T) {
	t.Parallel()
	d, mock := httpTestSetup(t, httpTestOpts{})
	mock.entered = make(chan string, 1)
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"heads up","async":true,"policy":"broadcast","steer":"steer"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}
	select {
	case <-mock.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("broadcast send never reached the backend")
	}
	waitForCond(t, "turn recorded", func() bool { return len(mock.snapshot()) == 1 })
	if calls := mock.snapshot(); calls[0].trigger != "user" {
		t.Errorf("broadcast choice turn trigger = %q, want \"user\" (today's injection path, mode inert)", calls[0].trigger)
	}
}

// TestSend_SteerChoice_PendingAsk_OnlyHumanAnswers pins the #1318 threat-scan
// fix on the idle path (RunTurn's answer capture): with an unpaused foci_ask
// pending, a choice-carrying send WITHOUT the #1130 human declaration is not
// taken as the ask's answer — it runs as a normal turn — while a
// human-declared one answers exactly like a Telegram/Discord reply.
func TestSend_SteerChoice_PendingAsk_OnlyHumanAnswers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		body       string
		wantAnswer bool
	}{
		{"automated runs a turn", `{"text":"yes","async":true,"steer":"steer"}`, false},
		{"human answers the ask", `{"text":"yes","async":true,"human":true,"steer":"steer"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, mock := httpTestSetup(t, httpTestOpts{})
			mock.entered = make(chan string, 1)
			answered := make(chan string, 1)
			d.agents[testAgentID].ag.AskRouter = &tools.AskRouter{
				PendingForSession: func(string) string { return "ask-1" },
				IsPaused:          func(string) bool { return false },
				HandleResponse:    func(_, data string) { answered <- data },
			}
			mux := newTestMux(d)

			if w := postJSON(mux, "/send", tc.body); w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
			}
			if tc.wantAnswer {
				select {
				case got := <-answered:
					if got != "yes" {
						t.Errorf("answer = %q, want yes", got)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("human-declared send did not answer the pending ask")
				}
				return
			}
			select {
			case <-mock.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("automated send neither answered nor ran a turn")
			}
			select {
			case got := <-answered:
				t.Fatalf("automated send answered the pending ask with %q as if it were the human", got)
			default:
			}
		})
	}
}

// TestSend_SteerChoice_OrphanedAutomatedSteer_NotAnAnswer is the orphan-path
// bypass of the above: on an API-mode agent a mid-turn automated --steer is
// buffered (the steer buffer, no backend to fold into); when the turn ends
// without a tool boundary to drain it, buildFollowUp rebuilds it as a
// follow-up envelope. That envelope must stay Automated, so RunTurn's idle
// ask capture runs it as a turn instead of answering the pending ask.
func TestSend_SteerChoice_OrphanedAutomatedSteer_NotAnAnswer(t *testing.T) {
	t.Parallel()
	d, _ := httpTestSetup(t, httpTestOpts{})
	// Round 1 answers with a tool_use (its primary write opens the steer
	// window); the tool returns at once; round 2 blocks inside the backend —
	// AFTER its request drained the steer buffer — so a steer arriving now is
	// buffered and, round 2 ending text-only, orphaned.
	fc := &foldingClient{mockClient: &mockClient{entered: make(chan string), proceed: make(chan struct{})}}
	ag := d.agents[testAgentID].ag
	ag.Client = fc
	toolRelease := make(chan struct{})
	close(toolRelease)
	holdOpenTool(ag, make(chan struct{}, 1), toolRelease)
	answered := make(chan string, 1)
	ag.AskRouter = &tools.AskRouter{
		PendingForSession: func(string) string { return "ask-1" },
		IsPaused:          func(string) bool { return false },
		HandleResponse:    func(_, data string) { answered <- data },
	}
	mux := newTestMux(d)

	recvRound := func(what string) string {
		t.Helper()
		select {
		case text := <-fc.entered:
			return text
		case got := <-answered:
			t.Fatalf("%s: the orphaned automated steer answered the pending ask with %q as if it were the human", what, got)
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never reached the backend", what)
		}
		return ""
	}

	// A user-envelope turn (a queued choice: never an answer itself) — only a
	// user batch opens the inbox steer window.
	if w := postJSON(mux, "/send", `{"text":"first","async":true,"steer":"queue"}`); w.Code != http.StatusAccepted {
		t.Fatalf("first: status = %d; body: %s", w.Code, w.Body.String())
	}
	recvRound("round 1")
	fc.proceed <- struct{}{}
	recvRound("round 2")
	waitForCond(t, "steer window open", func() bool { return ag.InboxTurnActive(testSessionKey) })

	if w := postJSON(mux, "/send", `{"text":"yes","async":true,"steer":"steer"}`); w.Code != http.StatusAccepted {
		t.Fatalf("steer: status = %d; body: %s", w.Code, w.Body.String())
	}
	waitForCond(t, "steer buffered", func() bool { return ag.InboxHasPendingInput(testSessionKey) })
	fc.proceed <- struct{}{} // round 2 ends text-only → the buffered steer is orphaned

	if text := recvRound("follow-up turn"); !strings.HasSuffix(text, "yes") {
		t.Errorf("follow-up turn text = %q, want the orphaned steer", text)
	}
	fc.proceed <- struct{}{}
}

// TestSend_SteerChoice_SlashCommandStillDispatches pins the pre-existing
// short-circuit a choice cannot ride (#1318, documented): a /send whose text
// is a slash command dispatches the command — no turn exists for a mode to
// shape.
func TestSend_SteerChoice_SlashCommandStillDispatches(t *testing.T) {
	t.Parallel()
	d, mock := httpTestSetup(t, httpTestOpts{commands: []*command.Command{newPingCommand()}})
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"/ping","async":true,"steer":"steer"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pong") {
		t.Errorf("body = %q, want the command's pong", w.Body.String())
	}
	if calls := mock.snapshot(); len(calls) != 0 {
		t.Errorf("backend called %d time(s), want 0 (a command starts no turn)", len(calls))
	}
}

// TestSend_SteerChoice_SkipsImplicitColdGate proves an explicit delivery
// choice skips ONLY the implicit --wait-cold 1m default (#1318): on a warm
// session a choice-carrying send dispatches now, while the same request
// without a choice (or with an unknown value) still defers.
func TestSend_SteerChoice_SkipsImplicitColdGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		body       string
		wantStatus string
	}{
		{"queue choice dispatches now", `{"text":"now","async":true,"steer":"queue"}`, "queued"},
		{"steer choice dispatches now", `{"text":"now","async":true,"steer":"steer"}`, "queued"},
		{"no choice still defers", `{"text":"later","async":true}`, "deferred"},
		{"unknown value still defers", `{"text":"later","async":true,"steer":"bogus"}`, "deferred"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := httpTestSetup(t, httpTestOpts{})
			withDeferStore(t, &d)
			d.sessionIndex.TouchCacheTouch(testSessionKey, time.Now()) // warm → the implicit wait_cold=1m is unmet
			mux := newTestMux(d)

			w := postJSON(mux, "/send", tc.body)

			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"`+tc.wantStatus+`"`) {
				t.Errorf("body = %q, want status %q", w.Body.String(), tc.wantStatus)
			}
		})
	}
}

// TestSend_SteerChoice_ExplicitWaitStillApplies pins the other half of the
// implicit-gate ruling: an EXPLICIT --wait-* flag still defers a
// choice-carrying send (the choice survives the wait in the deferred record).
func TestSend_SteerChoice_ExplicitWaitStillApplies(t *testing.T) {
	t.Parallel()
	d, _ := httpTestSetup(t, httpTestOpts{})
	store := withDeferStore(t, &d)
	mux := newTestMux(d)

	// Cold session → the explicit wait_warm is unmet → deferred, choice kept.
	w := postJSON(mux, "/send", `{"text":"later","async":true,"steer":"queue","wait_warm":"1h"}`)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "deferred") {
		t.Fatalf("code=%d body=%s, want the deferred receipt", w.Code, w.Body.String())
	}
	all, _ := store.All()
	if len(all) != 1 || all[0].Steer != "queue" {
		t.Fatalf("deferred records = %+v, want one keeping the queue choice", all)
	}
}

// TestSend_SteerChoice_InboxFull503 pins the choice path's full-inbox
// contract: once the session inbox refuses the envelope, /send answers 503
// instead of pretending the message was accepted — and the accepted
// choice-carrying sends really are user envelopes (observable once the
// parked turn releases), not injections.
func TestSend_SteerChoice_InboxFull503(t *testing.T) {
	t.Parallel()
	d, _ := httpTestSetup(t, httpTestOpts{})
	fc := &foldingClient{mockClient: &mockClient{}}
	d.agents[testAgentID].ag.Client = fc
	toolEntered := make(chan struct{})
	toolRelease := make(chan struct{})
	holdOpenTool(d.agents[testAgentID].ag, toolEntered, toolRelease)
	observe := steerEnvelopeObserver(d)
	mux := newTestMux(d)

	// Park a user-envelope turn inside its tool so the inbox only fills.
	go func() { postJSON(mux, "/send", `{"text":"park","async":true,"steer":"queue"}`) }()
	select {
	case <-toolEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("parking turn never reached its tool")
	}
	waitForCond(t, "parking turn in flight", func() bool { return d.agents[testAgentID].ag.IsTurnInFlight(testSessionKey) })

	// Fill the session channel (capacity 64) with queued choice sends, then
	// the next one must 503.
	code := 0
	for i := 0; i < 70; i++ {
		w := postJSON(mux, "/send", `{"text":"fill","async":true,"steer":"queue"}`)
		code = w.Code
		if code == http.StatusServiceUnavailable {
			if body := w.Body.String(); !strings.Contains(body, "inbox full") {
				t.Errorf("503 body = %q, want the inbox-full reason", body)
			}
			break
		}
		if code != http.StatusAccepted {
			t.Fatalf("fill send %d: code = %d body = %s", i, code, w.Body.String())
		}
	}
	if code != http.StatusServiceUnavailable {
		t.Fatalf("inbox never filled: last code = %d, want an eventual 503", code)
	}

	// Release and drain: the accepted choice sends were user envelopes.
	close(toolRelease)
	waitForCond(t, "choice envelopes drove as user turns", func() bool {
		for _, env := range observe() {
			if env.Steer == platform.SteerNever {
				return true
			}
		}
		return false
	})
}

// TestSend_SteerChoice_IdleAsyncDeliversOnce pins the httpDriver plumbing
// (#1318): an idle choice-carrying async send runs as its own turn whose sink
// is resolved at turn time through the driver — the chat's connection gets
// the typing indicator and exactly one reply delivery (the raw-sink contract:
// RunTurn adds the conversation-logging wrapper itself, so nothing is
// delivered or logged twice).
func TestSend_SteerChoice_IdleAsyncDeliversOnce(t *testing.T) {
	t.Parallel()
	d, mock := httpTestSetup(t, httpTestOpts{})
	mock.entered = make(chan string, 1)
	conn := newRecordingConn(testSessionKey)
	d.connMgr = recordingConnMgr{sessionKey: testSessionKey, conn: conn}
	observe := steerEnvelopeObserver(d)
	mux := newTestMux(d)

	w := postJSON(mux, "/send", `{"text":"deliver me","async":true,"steer":"queue"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}
	select {
	case <-mock.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("choice turn never reached the backend")
	}

	waitForCond(t, "reply delivered", func() bool {
		_, texts := conn.snapshot()
		for _, txt := range texts {
			if strings.Contains(txt, mockReply) {
				return true
			}
		}
		return false
	})
	// Give any duplicate delivery a moment to land, then assert exactly one.
	time.Sleep(100 * time.Millisecond)
	typing, texts := conn.snapshot()
	if len(texts) == 0 || !strings.Contains(texts[len(texts)-1], mockReply) {
		t.Fatalf("delivered texts = %v, want the reply", texts)
	}
	count := 0
	for _, txt := range texts {
		if strings.Contains(txt, mockReply) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("reply delivered %d time(s), want exactly 1 (texts=%v)", count, texts)
	}
	if len(typing) < 2 || !typing[0] || typing[len(typing)-1] {
		t.Errorf("typing toggles = %v, want true then false around the turn", typing)
	}
	if envs := observe(); len(envs) != 1 || envs[0].Steer != platform.SteerNever {
		t.Errorf("driven envelopes = %+v, want exactly the one queue-choice envelope", envs)
	}
}
