package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/secrets"
	"foci/internal/tools"
)

// newToolsRegistryForTest builds a fresh tool registry for wiring tests.
func newToolsRegistryForTest() *tools.Registry { return tools.NewRegistry() }

// TestAsUserInput_SetsTextAndMarker unit-pins the injectOption the ask
// delivery uses: the message becomes the envelope's Text (so the steer path
// can fold it) and the injection is marked as the user's own input.
func TestAsUserInput_SetsTextAndMarker(t *testing.T) {
	env := agent.Envelope{SessionKey: "scout/c42", Inject: &agent.InjectMeta{Trigger: "ask_grader"}}
	asUserInput("the graded answer")(&env)

	if !env.Inject.UserInput {
		t.Error("InjectMeta.UserInput = false, want true")
	}
	if env.Text != "the graded answer" {
		t.Errorf("Text = %q, want the answer message", env.Text)
	}
	if env.Inject.Trigger != "ask_grader" {
		t.Errorf("Trigger = %q, want ask_grader unchanged (via=ask-grader)", env.Inject.Trigger)
	}
}

// TestOnInjectRefused_FiresOnRefusedEnvelope unit-pins the refusal option:
// the hook rides on InjectMeta.Refused, the handle the inbox fires when it
// declines to run the injection.
func TestOnInjectRefused_FiresOnRefusedEnvelope(t *testing.T) {
	fired := make(chan struct{}, 1)
	env := agent.Envelope{SessionKey: "scout/c42", Inject: &agent.InjectMeta{Trigger: "scheduled_wake"}}
	onInjectRefused(func() { fired <- struct{}{} })(&env)

	if env.Inject.Refused == nil {
		t.Fatal("Refused not set")
	}
	env.Inject.Refused()
	select {
	case <-fired:
	default:
		t.Fatal("the hook did not fire")
	}
}

// TestDeliverToSessionChatThen_UserInputCtxMarksEnvelope pins the wiring the
// ask tool's user route (newAskUserDeliverFn) rides on: a ctx carrying the
// user-input marker makes deliverToSessionChatThen enqueue the injection as
// the user's own input, with the message as Text and the request id tagged —
// while a plain ctx (the system route, newAskDeliverFn) enqueues neither.
func TestDeliverToSessionChatThen_UserInputCtxMarksEnvelope(t *testing.T) {
	newCase := func(user bool) (captured agent.Envelope) {
		ag := &agent.Agent{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ag.StartInbox(ctx)

		capture := func(env *agent.Envelope) { captured = *env }
		callCtx := agent.WithTrigger(context.Background(), "ask_grader")
		if user {
			callCtx = agent.WithUserInput(callCtx)
		}
		deliverToSessionChatThen(ag, callCtx, "ask_grader", stubConnMgr{}, "scout", "scout/c42",
			"the answer", nil, "req-1", nil, capture)
		return captured
	}

	user := newCase(true)
	if user.Inject == nil {
		t.Fatal("no envelope captured")
	}
	if !user.Inject.UserInput {
		t.Error("user-input ctx: InjectMeta.UserInput = false, want true (the answer takes the user path)")
	}
	if user.Text != "the answer" {
		t.Errorf("user-input ctx: Text = %q, want the answer message", user.Text)
	}
	if user.Inject.AskReqID != "req-1" {
		t.Errorf("AskReqID = %q, want req-1 (#1712 tagging kept)", user.Inject.AskReqID)
	}
	if user.Inject.Trigger != "ask_grader" {
		t.Errorf("Trigger = %q, want ask_grader (via=ask-grader kept)", user.Inject.Trigger)
	}

	system := newCase(false)
	if system.Inject == nil {
		t.Fatal("no envelope captured")
	}
	if system.Inject.UserInput {
		t.Error("plain ctx: InjectMeta.UserInput = true, want false (expiry/restore notices stay system input)")
	}
	if system.Text != "" {
		t.Errorf("plain ctx: Text = %q, want empty (system injects carry no text)", system.Text)
	}
	if system.Inject.AskReqID != "req-1" {
		t.Errorf("plain ctx: AskReqID = %q, want req-1 (the system route keeps the tag too)", system.Inject.AskReqID)
	}
}

// deliverRecorder records AskDeliverFn calls, for wiring assertions.
type deliverRecorder struct {
	calls chan string
}

func newDeliverRecorder() *deliverRecorder {
	return &deliverRecorder{calls: make(chan string, 4)}
}

func (d *deliverRecorder) fn(sessionKey, requestID, message string) {
	d.calls <- message
}

// TestAskToolTable_WiresUserInputDeliver pins the production wiring (#2233):
// the tool table's ask row passes WithUserInputDeliver, so an answer batch
// built through the REAL registration dispatches through the user-input
// route — the risk of a deployment wiring only the system route is what this
// guards (a nil user route silently degrades answers to system input).
func TestAskToolTable_WiresUserInputDeliver(t *testing.T) {
	p := minimalSetupParams(t, "test")
	registry := newToolsRegistryForTest()
	sys, user := newDeliverRecorder(), newDeliverRecorder()
	out := &toolOutputs{}
	registerTools(&toolDeps{
		p:              p,
		path:           pathAPI,
		registry:       registry,
		connMgr:        stubConnMgr{},
		agLazy:         func() *agent.Agent { return nil },
		wakeFn:         stubWakeFn,
		askDeliver:     sys.fn,
		askUserDeliver: user.fn,
		out:            out,
	})

	tool := registry.Get("ask")
	if tool == nil {
		t.Fatal("ask tool not registered")
	}
	res, err := tool.Execute(tools.WithSessionKey(context.Background(), "test/main"), json.RawMessage(`{"questions":[{"question":"pick","options":[{"label":"A"}]}]}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var started struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &started); err != nil {
		t.Fatalf("parse result %q: %v", res.Text, err)
	}

	// The user answers: the batch must arrive through the user-input route.
	out.askRouter.HandleResponse(started.RequestID, "qa:0")
	select {
	case msg := <-user.calls:
		if !strings.Contains(msg, "A") {
			t.Errorf("user route message = %q, want the answer batch", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("answer batch never reached the user-input route — WithUserInputDeliver is unwired")
	}
	select {
	case msg := <-sys.calls:
		t.Fatalf("answer batch also hit the system route: %q", msg)
	default:
	}
}

// askAnswerTarget builds the agent an ask answer's delivery resolves to,
// capturing the envelope its Enqueue receives — the observable difference
// between the user-input route (InjectMeta.UserInput set, the answer as Text)
// and the system route (neither).
func askAnswerTarget() (*agent.Agent, <-chan agent.Envelope) {
	target := &agent.Agent{}
	envs := make(chan agent.Envelope, 1)
	target.SetEnqueueObserver(func(env agent.Envelope) {
		select {
		case envs <- env:
		default:
		}
	})
	return target, envs
}

// startAskAndAnswer drives one ask through a registry built by a real setup
// function and answers it via the router that setup mounted on askAgent (the
// configured agent for configureAPI, the lazy target for buildExecRegistry),
// so the delivery exercises the production deliver fns end to end.
func startAskAndAnswer(t *testing.T, registry *tools.Registry, askAgent *agent.Agent) {
	t.Helper()
	tool := registry.Get("ask")
	if tool == nil {
		t.Fatal("ask tool not registered")
	}
	// A chat-shaped session key: the delivery path parses it back to the
	// owning agent (test/c42 → "test") through the production resolver.
	res, err := tool.Execute(tools.WithSessionKey(context.Background(), "test/c42"), json.RawMessage(`{"questions":[{"question":"pick","options":[{"label":"A"}]}]}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var started struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &started); err != nil {
		t.Fatalf("parse result %q: %v", res.Text, err)
	}
	if askAgent.AskRouter == nil {
		t.Fatal("the setup function did not mount the ask router on the agent")
	}
	askAgent.AskRouter.HandleResponse(started.RequestID, "qa:0")
}

// wantUserInputEnvelope asserts the captured envelope is an ask answer on the
// user-input route (#2233): marker set, answer as Text — never the bare
// system inject a missing askUserDeliver wiring degrades it to.
func wantUserInputEnvelope(t *testing.T, envs <-chan agent.Envelope) {
	t.Helper()
	select {
	case env := <-envs:
		if env.Inject == nil {
			t.Fatal("enqueued envelope is not an injection")
		}
		if !env.Inject.UserInput {
			t.Fatal("ask answer took the system route — the setup's askUserDeliver wiring is missing (#2233)")
		}
		if !strings.Contains(env.Text, "A") {
			t.Errorf("Text = %q, want the answer batch text", env.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ask answer never reached the target session")
	}
}

// TestAskExecRegistry_WiresUserInputDeliver pins the delegated-path production
// wiring (#2233): the ask tool built by buildExecRegistry delivers answer
// batches through the user-input route (newAskUserDeliverFn). A dropped
// askUserDeliver field compiles and silently degrades every exec-path answer
// to system input — held behind background work, never steering.
func TestAskExecRegistry_WiresUserInputDeliver(t *testing.T) {
	target, envs := askAnswerTarget()
	p := minimalSetupParams(t, "test")
	p.ctx = context.Background()
	p.agentResolverFn = func(string) *agentInstance { return &agentInstance{ag: target} }

	registry := buildExecRegistry(p, stubWakeFn, nil, func() *agent.Agent { return target })
	startAskAndAnswer(t, registry, target)
	wantUserInputEnvelope(t, envs)
}

// TestConfigureAPI_WiresUserInputAskDeliver pins the API-path production
// wiring (#2233): the ask tool built by configureAPI's registerTools delivers
// answer batches through the user-input route (newAskUserDeliverFn). A
// dropped askUserDeliver field compiles and silently degrades every API-agent
// answer to system input — held behind background work, never steering.
func TestConfigureAPI_WiresUserInputAskDeliver(t *testing.T) {
	target, envs := askAnswerTarget()

	p := minimalSetupParams(t, "test")
	p.acfg.Workspace = t.TempDir()
	p.ctx = context.Background()
	p.cfg.Agents = []config.AgentConfig{{ID: "test"}} // an API-backed agent: groups may resolve
	p.cfg.Groups.Groups = map[string]string{"powerful": "anthropic/claude-opus-4-6"}
	p.clientProvider = &summariserWireClientProvider{client: &summariserWireClient{}}
	store, err := secrets.Load(filepath.Join(t.TempDir(), "secrets.toml"))
	if err != nil {
		t.Fatalf("secrets.Load: %v", err)
	}
	p.store = store
	p.agentResolverFn = func(string) *agentInstance { return &agentInstance{ag: target} }

	// The same preamble setupAgent runs before configureAPI (see
	// TestConfigureAPI_SummariserCarriesRouting).
	shared := resolveSharedSetup(p)
	ag := shared.newAgent()
	configureUniversal(ag, shared.p, nil)
	fp, ok := configureAPI(ag, shared.p, shared, nil)
	if !ok {
		t.Fatal("configureAPI failed")
	}

	startAskAndAnswer(t, fp.registry, ag)
	wantUserInputEnvelope(t, envs)
}

// sourceProbeBackend is gateProbeBackend recording the Source of every begin
// it receives, so a test can see which path a delivered message took into
// the backend.
type sourceProbeBackend struct {
	gateProbeBackend
	sources chan delegator.InjectSource
}

func (b *sourceProbeBackend) ImmediateInject(ctx context.Context, inj delegator.Inject) error {
	select {
	case b.sources <- inj.Source:
	default:
	}
	return b.gateProbeBackend.ImmediateInject(ctx, inj)
}

// TestAskUserDeliver_ReachesBackendAsSourceUser is the end-to-end pin for the
// ask-answer route (#2233 review F2): an answer delivered through
// newAskUserDeliverFn into a DELEGATED session with background work pending
// is neither held by the pending-work gate nor begun as system input — the
// backend receives it as SourceUser. It guards the whole chain
// (newAskUserDeliverFn → deliverToResolvedSession → deliverToSessionChatThen →
// HandleMessage's ctx → RunInference), including the ctx marker reaching
// HandleMessage, which the envelope-only test above cannot see.
func TestAskUserDeliver_ReachesBackendAsSourceUser(t *testing.T) {
	t.Parallel()
	be := &sourceProbeBackend{sources: make(chan delegator.InjectSource, 4)}
	d := delegatedBranchHarnessWith(t, httpTestOpts{}, be, delegator.Spec{})
	inst := d.agents[testAgentID]
	sk := testAgentID + "/i" + runToken("ask")
	if _, err := inst.ag.DelegatedManager.Get(d.ctx, sk); err != nil {
		t.Fatalf("start backend: %v", err)
	}
	be.awaiting.Store(true) // background work pending: system input is held
	if !inst.ag.SystemInputHeld(sk) {
		t.Fatal("fixture: system input must read held")
	}

	deliver := newAskUserDeliverFn(func(string) *agentInstance { return inst }, d.ctx, stubConnMgr{})
	deliver(sk, "req-1", "[SYSTEM: the user answered your `ask` request.] A")

	select {
	case src := <-be.sources:
		if src != delegator.SourceUser {
			t.Fatalf("ask answer began as %v, want SourceUser — the user-input ctx marker was lost before HandleMessage", src)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ask answer never reached the backend — held behind background work like system input")
	}
}
