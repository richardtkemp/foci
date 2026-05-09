package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/session"
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
// search dirs point at promptDir and webhooks map is configured.
// Returns deps, the mock client (for assertions).
func webhookTestSetup(t *testing.T, promptDir string, sessionKey string, webhooks map[string]string) (httpHandlerDeps, *mockWebhookClient) {
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

	// Provide a stub connMgr so mostRecentSessionKey can resolve the session.
	// "EMPTY" is a sentinel: pass it to get no session (tests the error path).
	var cm platform.ConnectionManager = stubConnMgr{}
	if sk != "EMPTY" {
		cm = stubConnMgr{agentID: "test-agent", sessionKey: sk}
	}

	inst := &agentInstance{
		id:               "test-agent",
		ag:               ag,
		promptSearchDirs: []string{promptDir},
		webhooks:         webhooks,
	}

	d := httpHandlerDeps{
		agents:     map[string]*agentInstance{"test-agent": inst},
		agentOrder: []string{"test-agent"},
		cfg:        &config.Config{},
		sessions:   sessions,
		connMgr:    cm,
		ctx:        context.Background(),
	}
	return d, mock
}

func newWebhookMux(d httpHandlerDeps) *http.ServeMux {
	mux := http.NewServeMux()
	registerHTTPHandlers(mux, d)
	return mux
}

// TestWebhook_SyncSuccess tests the happy path: known agent, configured hook ID,
// existing prompt file, body payload, sync=true → 200 with agent response.
func TestWebhook_SyncSuccess(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "deploy.md"), []byte("Handle this deploy event."), 0644)

	webhooks := map[string]string{"deploy": "deploy.md"}
	d, mock := webhookTestSetup(t, dir, "", webhooks)
	mux := newWebhookMux(d)

	body := `{"action":"completed","repo":"foci"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/deploy?sync=true", strings.NewReader(body))
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

	webhooks := map[string]string{"alert": "alert.md"}
	d, _ := webhookTestSetup(t, dir, "", webhooks)
	mux := newWebhookMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/alert", strings.NewReader("payload"))
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

	webhooks := map[string]string{"ping": "ping.md"}
	d, mock := webhookTestSetup(t, dir, "", webhooks)
	mux := newWebhookMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/ping?sync=true", nil)
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
	d, _ := webhookTestSetup(t, t.TempDir(), "", nil)
	mux := newWebhookMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/nonexistent/deploy", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestWebhook_UnknownHookID returns 404 when the hook ID is not in the webhooks map.
func TestWebhook_UnknownHookID(t *testing.T) {
	webhooks := map[string]string{"deploy": "deploy.md"}
	d, _ := webhookTestSetup(t, t.TempDir(), "", webhooks)
	mux := newWebhookMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/nonexistent?sync=true", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

// TestWebhook_PathTraversal tests that path-traversal-style hook IDs return 404.
// Since hookid is a map key lookup, traversal strings simply won't match any key.
func TestWebhook_PathTraversal(t *testing.T) {
	webhooks := map[string]string{"deploy": "deploy.md"}
	d, _ := webhookTestSetup(t, t.TempDir(), "", webhooks)
	mux := newWebhookMux(d)

	// These hookIDs look like traversal attempts but are just map keys that don't exist.
	hookIDs := []string{
		"..%2F..%2Fetc%2Fpasswd",
		"..%2Fdeploy",
		"deploy.md%00",
	}
	for _, hookID := range hookIDs {
		req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/"+hookID, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("hookID %q: status = %d, want 404", hookID, w.Code)
		}
	}
}

// TestWebhook_PromptFileNotFound returns 404 when the hook is configured but the prompt file doesn't exist.
func TestWebhook_PromptFileNotFound(t *testing.T) {
	webhooks := map[string]string{"missing": "nonexistent.md"}
	d, _ := webhookTestSetup(t, t.TempDir(), "", webhooks) // empty dir, no prompts
	mux := newWebhookMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/missing?sync=true", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

// TestWebhook_BadPath tests various malformed paths.
func TestWebhook_BadPath(t *testing.T) {
	d, _ := webhookTestSetup(t, t.TempDir(), "", nil)
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
	d, _ := webhookTestSetup(t, t.TempDir(), "", nil)
	mux := newWebhookMux(d)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/webhook/test-agent/deploy", nil)
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

	webhooks := map[string]string{"test": "test.md"}
	d, _ := webhookTestSetup(t, dir, "EMPTY", webhooks)

	mux := newWebhookMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/test?sync=true", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want 412; body: %s", w.Code, w.Body.String())
	}
}

// TestWebhook_IfInactive tests that the if_inactive query param skips when
// the targeted session has recent activity. Under TODO #753 semantics,
// "activity" is read from session_metadata.last_activity (any turn-init
// path) rather than agent_metadata.last_user_activity (user inbound only).
// The narrower user-only behaviour now lives behind --if-user-inactive.
func TestWebhook_IfInactive(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.md"), []byte("prompt"), 0644)

	webhooks := map[string]string{"test": "test.md"}
	d, _ := webhookTestSetup(t, dir, "", webhooks)

	// Simulate recent session activity. The webhook test setup wires a
	// stubConnMgr with sessionKey="test-agent/i0/0", so SessionKeyBase is
	// "test-agent/i0".
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	idx.SetSessionMetadata("test-agent/i0", "last_activity", fmt.Sprintf("%d", time.Now().Unix()))
	d.sessionIndex = idx

	mux := newWebhookMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/test?if_inactive=1h", strings.NewReader("payload"))
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

// TestWebhook_IfUserInactive_LegacyUserActivityOnly verifies the new
// --if-user-inactive flag preserves the OLD pre-TODO-#753 narrow semantics:
// only `last_user_activity` (real user inbound) counts, not session-level
// activity from cron/CLI/webhook turns. Confirms the split is honoured.
func TestWebhook_IfUserInactive_LegacyUserActivityOnly(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.md"), []byte("prompt"), 0644)

	webhooks := map[string]string{"test": "test.md"}
	d, _ := webhookTestSetup(t, dir, "", webhooks)

	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	// Recent SESSION activity (cron-injected turn) but NO recent user
	// activity. --if-user-inactive should still allow the request through
	// (user has not been engaged), even though the session has been busy.
	idx.SetSessionMetadata("test-agent/i0", "last_activity", fmt.Sprintf("%d", time.Now().Unix()))
	d.sessionIndex = idx

	mux := newWebhookMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/test?if_user_inactive=1h&sync=true", strings.NewReader("payload"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	// Should NOT have been skipped — webhook delivered to the agent.
	if strings.HasPrefix(resp["response"], "skipped:") {
		t.Errorf("response = %q, want webhook to deliver (user-inactive should not be tripped by session activity)", resp["response"])
	}
}

// TestWebhook_NoWebhooksConfigured returns 404 when agent has no webhooks configured.
func TestWebhook_NoWebhooksConfigured(t *testing.T) {
	d, _ := webhookTestSetup(t, t.TempDir(), "", nil) // nil webhooks
	mux := newWebhookMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/anything?sync=true", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}
