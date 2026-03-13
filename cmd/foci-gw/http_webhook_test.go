package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/state"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// mockWebhookClient is a minimal provider.Client that returns a canned response.
type mockWebhookClient struct {
	lastReqText string // last user message text sent to the client
}

func (m *mockWebhookClient) SendMessage(_ context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	// Capture the last user message for assertions.
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			m.lastReqText = provider.TextOf(req.Messages[i].Content)
			break
		}
	}
	return &provider.MessageResponse{
		ID:         "msg_test",
		Type:       "message",
		Role:       "assistant",
		Content:    provider.TextContent("webhook reply"),
		StopReason: "end_turn",
		Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
	}, nil
}

func (m *mockWebhookClient) CountTokens(_ context.Context, _ *provider.MessageRequest) (int, error) {
	return 100, nil
}

func (m *mockWebhookClient) IsCachingAvailable() bool { return false }

// RetryBaseDelay satisfies the provider.retryableClient interface (structural typing)
// so that provider.Send uses fast retries in tests.
func (m *mockWebhookClient) RetryBaseDelay() time.Duration { return time.Millisecond }

// webhookTestSetup builds httpHandlerDeps with a single agent whose prompt
// search dirs point at promptDir. Returns deps, the mock client (for assertions),
// and a cleanup function.
func webhookTestSetup(t *testing.T, promptDir string, sessionKey string) (httpHandlerDeps, *mockWebhookClient) {
	t.Helper()
	mock := &mockWebhookClient{}
	sessDir := filepath.Join(t.TempDir(), "sessions")
	os.MkdirAll(sessDir, 0755)
	sessions := session.NewStore(sessDir)

	ag := &agent.Agent{
		Client:    mock,
		Sessions:  sessions,
		Tools:     tools.NewRegistry(),
		Bootstrap: workspace.NewBootstrap(t.TempDir(), nil),
		Model:     "test-model",
	}

	sk := sessionKey
	if sk == "" {
		sk = "test-agent/i0/0"
	}

	inst := &agentInstance{
		id:                "test-agent",
		ag:                ag,
		defaultSessionKey: func() string { return sk },
		promptSearchDirs:  []string{promptDir},
	}

	d := httpHandlerDeps{
		agents:     map[string]*agentInstance{"test-agent": inst},
		agentOrder: []string{"test-agent"},
		cfg:        &config.Config{},
		sessions:   sessions,
		ctx:        context.Background(),
	}
	return d, mock
}

func newWebhookMux(d httpHandlerDeps) *http.ServeMux {
	mux := http.NewServeMux()
	registerHTTPHandlers(mux, d)
	return mux
}

// TestWebhook_SyncSuccess tests the happy path: known agent, existing prompt
// file, body payload, sync=true → 200 with agent response.
func TestWebhook_SyncSuccess(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "deploy.md"), []byte("Handle this deploy event."), 0644)

	d, mock := webhookTestSetup(t, dir, "")
	mux := newWebhookMux(d)

	body := `{"action":"completed","repo":"foci"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/deploy.md?sync=true", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != "webhook reply" {
		t.Errorf("response = %q, want %q", resp["response"], "webhook reply")
	}

	// Verify the combined message includes prompt + payload.
	if !strings.Contains(mock.lastReqText, "Handle this deploy event.") {
		t.Errorf("agent message missing prompt text, got: %s", mock.lastReqText)
	}
	if !strings.Contains(mock.lastReqText, "## Webhook Payload") {
		t.Errorf("agent message missing payload heading, got: %s", mock.lastReqText)
	}
	if !strings.Contains(mock.lastReqText, body) {
		t.Errorf("agent message missing payload body, got: %s", mock.lastReqText)
	}
}

// TestWebhook_AsyncDefault tests that requests without ?sync=true return 202.
func TestWebhook_AsyncDefault(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "alert.md"), []byte("Process this alert."), 0644)

	d, _ := webhookTestSetup(t, dir, "")
	mux := newWebhookMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/alert.md", strings.NewReader("payload"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "queued" {
		t.Errorf("status = %q, want %q", resp["status"], "queued")
	}

	// Allow async goroutine to complete before temp dir cleanup.
	time.Sleep(50 * time.Millisecond)
}

// TestWebhook_EmptyBody tests that a webhook with no body still works
// (sends just the prompt text, no payload heading).
func TestWebhook_EmptyBody(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ping.md"), []byte("Ping check."), 0644)

	d, mock := webhookTestSetup(t, dir, "")
	mux := newWebhookMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/ping.md?sync=true", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if strings.Contains(mock.lastReqText, "## Webhook Payload") {
		t.Error("empty body should not produce payload heading")
	}
	if !strings.Contains(mock.lastReqText, "Ping check.") {
		t.Errorf("message should contain prompt text, got: %s", mock.lastReqText)
	}
}

// TestWebhook_UnknownAgent returns 400 for an unknown agent ID.
func TestWebhook_UnknownAgent(t *testing.T) {
	d, _ := webhookTestSetup(t, t.TempDir(), "")
	mux := newWebhookMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/nonexistent/deploy.md", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestWebhook_PromptNotFound returns 404 when the prompt file doesn't exist.
func TestWebhook_PromptNotFound(t *testing.T) {
	d, _ := webhookTestSetup(t, t.TempDir(), "") // empty dir, no prompts
	mux := newWebhookMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/missing.md?sync=true", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

// TestWebhook_BadPath tests various malformed paths.
func TestWebhook_BadPath(t *testing.T) {
	d, _ := webhookTestSetup(t, t.TempDir(), "")
	mux := newWebhookMux(d)

	paths := []string{
		"/webhook/",
		"/webhook/agent-only",
		"/webhook/agent/",
	}
	for _, path := range paths {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("path %q: status = %d, want 400", path, w.Code)
		}
	}
}

// TestWebhook_MethodNotAllowed verifies only POST is accepted.
func TestWebhook_MethodNotAllowed(t *testing.T) {
	d, _ := webhookTestSetup(t, t.TempDir(), "")
	mux := newWebhookMux(d)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/webhook/test-agent/deploy.md", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, w.Code)
		}
	}
}

// TestWebhook_NoSession returns 412 when the agent has no default session.
func TestWebhook_NoSession(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.md"), []byte("prompt text"), 0644)

	d, _ := webhookTestSetup(t, dir, "EMPTY")
	// Override to return empty session key.
	d.agents["test-agent"].defaultSessionKey = func() string { return "" }

	mux := newWebhookMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/test.md?sync=true", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want 412; body: %s", w.Code, w.Body.String())
	}
}

// TestWebhook_IfInactive tests the if_inactive query param skips when agent is active.
func TestWebhook_IfInactive(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.md"), []byte("prompt"), 0644)

	d, _ := webhookTestSetup(t, dir, "")

	// Simulate recent activity via state store.
	ss := state.New(filepath.Join(t.TempDir(), "state.json"))
	ss.Set("agent/test-agent/last_user_activity", time.Now().Unix())
	d.stateStore = ss

	mux := newWebhookMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/test.md?if_inactive=1h", strings.NewReader("payload"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != "skipped: session recently active" {
		t.Errorf("response = %q, want skipped message", resp["response"])
	}
}
