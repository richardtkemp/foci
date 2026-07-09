package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"foci/internal/delegator"
)

// ---------------------------------------------------------------------------
// Test infrastructure
// ---------------------------------------------------------------------------

func newPlanTestServer(t *testing.T) (*httptest.Server, *planRecorder) {
	t.Helper()
	rec := &planRecorder{}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.requests = append(rec.requests, planRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		rec.mu.Unlock()
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			json.NewEncoder(w).Encode(map[string]string{"id": "sess-plan"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hs.Close)
	return hs, rec
}

type planRequest struct {
	Method string
	Path   string
	Body   []byte
}

type planRecorder struct {
	requests []planRequest
	mu       planMu
}

type planMu struct{}

func (m *planMu) Lock()   {}
func (m *planMu) Unlock() {}

func (r *planRecorder) lastPromptAsync() (planRequest, bool) {
	for i := len(r.requests) - 1; i >= 0; i-- {
		if r.requests[i].Path == "/session/sess-plan/prompt_async" {
			return r.requests[i], true
		}
	}
	return planRequest{}, false
}

func (r *planRecorder) anyConfigPatch() bool {
	for _, req := range r.requests {
		if req.Method == http.MethodPatch && req.Path == "/config" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestPlanDelivery_SendsPromptWithPlanAgent(t *testing.T) {
	// Verifies planDelivery sends the plan request via POST /prompt_async
	// with agent:"plan" in the body — so opencode uses the plan agent
	// for this turn without changing server-wide config.
	hs, rec := newPlanTestServer(t)
	srv := &Server{baseURL: hs.URL, http: hs.Client(), agentID: "plan-test", sessions: map[string]*Backend{}}
	b := &Backend{
		server:      srv,
		agentID:     "plan-test",
		sessionID:   "sess-plan",
		readyCh:     make(chan struct{}),
		outstanding: delegator.NewOutstandingRegistry(),
	}

	deps := delegator.PlanDeps{
		SessionKey: "test/session",
		Backend:    func() (delegator.Delegator, error) { return b, nil },
	}
	msg, err := planDelivery(context.Background(), deps, "implement feature X")
	if err != nil {
		t.Fatalf("planDelivery: %v", err)
	}
	if msg == "" {
		t.Error("planDelivery returned empty message")
	}

	// Verify POST /prompt_async fired with agent:"plan".
	prompt, ok := rec.lastPromptAsync()
	if !ok {
		t.Fatal("POST /prompt_async was not sent")
	}
	var body struct {
		Agent string `json:"agent"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(prompt.Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Agent != "plan" {
		t.Errorf("agent = %q, want plan", body.Agent)
	}
	if len(body.Parts) != 1 || body.Parts[0].Text != "implement feature X" {
		t.Errorf("parts = %+v, want text 'implement feature X'", body.Parts)
	}
}

func TestPlanDelivery_NoConfigChangeNeeded(t *testing.T) {
	// Verifies the per-request agent approach doesn't need a config
	// swap-back. The plan's original sketch called for PATCH /config
	// then swap back via OnTurnComplete — we use the prompt body's
	// agent field instead, which is per-request and auto-reverts.
	// Assert no PATCH /config was sent.
	hs, rec := newPlanTestServer(t)
	srv := &Server{baseURL: hs.URL, http: hs.Client(), agentID: "plan-test2", sessions: map[string]*Backend{}}
	b := &Backend{
		server:      srv,
		agentID:     "plan-test2",
		sessionID:   "sess-plan",
		readyCh:     make(chan struct{}),
		outstanding: delegator.NewOutstandingRegistry(),
	}

	deps := delegator.PlanDeps{
		SessionKey: "test/session",
		Backend:    func() (delegator.Delegator, error) { return b, nil },
	}
	_, _ = planDelivery(context.Background(), deps, "test plan")

	if rec.anyConfigPatch() {
		t.Error("PATCH /config was sent — per-request agent field should make config change unnecessary")
	}
}

func TestPlanDelivery_BackendError(t *testing.T) {
	// Verifies planDelivery surfaces a Backend() error cleanly — port
	// of ccstream's TestPlanDeliveryNoNotifier.
	deps := delegator.PlanDeps{
		SessionKey: "test/session",
		Backend:    func() (delegator.Delegator, error) { return nil, planErr },
	}
	_, err := planDelivery(context.Background(), deps, "test")
	if err == nil {
		t.Fatal("planDelivery should error when Backend() fails")
	}
}

func TestPlanDelivery_WrongBackendType(t *testing.T) {
	// Verifies planDelivery errors if Backend() returns a non-*Backend
	// (defensive — shouldn't happen in production but catches wiring
	// bugs).
	deps := delegator.PlanDeps{
		SessionKey: "test/session",
		Backend:    func() (delegator.Delegator, error) { return fakeDelegator{}, nil },
	}
	_, err := planDelivery(context.Background(), deps, "test")
	if err == nil {
		t.Fatal("planDelivery should error when backend type is wrong")
	}
}

// Test helpers
type planError struct{}

func (planError) Error() string { return "fake backend error" }

var planErr = planError{}

type fakeDelegator struct{}

func (fakeDelegator) Start(context.Context, delegator.StartOptions) error { return nil }
func (fakeDelegator) ImmediateInject(context.Context, delegator.Inject) error      { return nil }
func (fakeDelegator) WaitForTurn(context.Context) error                   { return nil }
func (fakeDelegator) IsTurnInFlight() bool                                { return false }
func (fakeDelegator) IsRunning() bool                                     { return false }
func (fakeDelegator) SetPermissionPromptFunc(delegator.PermissionPromptFunc) {}
func (fakeDelegator) SetOnPromptsCleared(func())                          {}
func (fakeDelegator) RegisterPromptCancelListener(string, func(string))   {}
func (fakeDelegator) SetOnSessionReady(func(string))                      {}
func (fakeDelegator) SetTypingFunc(func(bool))                            {}
func (fakeDelegator) AttachSessionEvents(*delegator.SessionEvents)        {}
func (fakeDelegator) SendKeystroke(context.Context, string) error         { return nil }
func (fakeDelegator) SendSpecialKey(context.Context, string) error        { return nil }
func (fakeDelegator) Interrupt(context.Context) error                     { return nil }
func (fakeDelegator) SessionID() string                                   { return "" }
func (fakeDelegator) SessionFilePath() string                             { return "" }
func (fakeDelegator) WaitReady(context.Context) error                     { return nil }
func (fakeDelegator) CheckReady(context.Context) (bool, error)            { return true, nil }
func (fakeDelegator) StatusDetail() string                                  { return "" }
func (fakeDelegator) Close() error                                        { return nil }
