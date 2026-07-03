package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWebhook_SyncSuccess tests the happy path: known agent, configured hook ID,
// existing prompt file, body payload, sync=true → 200 with agent response.
func TestWebhook_SyncSuccess(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "deploy.md"), []byte("Handle this deploy event."), 0644)

	d, mock := httpTestSetup(t, httpTestOpts{promptDir: dir, webhooks: map[string]string{"deploy": "deploy.md"}})
	mux := newTestMux(d)

	body := `{"action":"completed","repo":"foci"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/deploy?sync=true", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != mockReply {
		t.Errorf("response = %q, want %q", resp["response"], mockReply)
	}

	// Verify the combined message includes prompt + payload.
	if !strings.Contains(mock.lastText(), "Handle this deploy event.") {
		t.Errorf("agent message missing prompt text, got: %s", mock.lastText())
	}
	if !strings.Contains(mock.lastText(), "## Webhook Payload") {
		t.Errorf("agent message missing payload heading, got: %s", mock.lastText())
	}
	if !strings.Contains(mock.lastText(), body) {
		t.Errorf("agent message missing payload body, got: %s", mock.lastText())
	}
}

// TestWebhook_AsyncDefault tests that requests without ?sync=true return 202
// immediately AND that the queued turn actually executes afterwards: the 202
// only means "accepted", so the test then waits for the mock backend to see
// the combined prompt+payload message.
func TestWebhook_AsyncDefault(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "alert.md"), []byte("Process this alert."), 0644)

	d, mock := httpTestSetup(t, httpTestOpts{promptDir: dir, webhooks: map[string]string{"alert": "alert.md"}})
	mock.entered = make(chan string, 1)
	mux := newTestMux(d)

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

	// The queued turn must reach the backend with the combined message.
	select {
	case text := <-mock.entered:
		if !strings.Contains(text, "Process this alert.") || !strings.Contains(text, "payload") {
			t.Errorf("async turn message = %q, want prompt + payload", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("202 returned but the queued turn never reached the backend")
	}
}

// TestWebhook_EmptyBody tests that a webhook with no body still works
// (sends just the prompt text, no payload heading).
func TestWebhook_EmptyBody(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ping.md"), []byte("Ping check."), 0644)

	d, mock := httpTestSetup(t, httpTestOpts{promptDir: dir, webhooks: map[string]string{"ping": "ping.md"}})
	mux := newTestMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/ping?sync=true", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if strings.Contains(mock.lastText(), "## Webhook Payload") {
		t.Error("empty body should not produce payload heading")
	}
	if !strings.Contains(mock.lastText(), "Ping check.") {
		t.Errorf("message should contain prompt text, got: %s", mock.lastText())
	}
}

// TestWebhook_UnknownAgent returns 400 for an unknown agent ID.
func TestWebhook_UnknownAgent(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{promptDir: t.TempDir()})
	mux := newTestMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/nonexistent/deploy", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestWebhook_UnknownHookID returns 404 when the hook ID is not in the webhooks map.
func TestWebhook_UnknownHookID(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{promptDir: t.TempDir(), webhooks: map[string]string{"deploy": "deploy.md"}})
	mux := newTestMux(d)

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
	d, _ := httpTestSetup(t, httpTestOpts{promptDir: t.TempDir(), webhooks: map[string]string{"deploy": "deploy.md"}})
	mux := newTestMux(d)

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
	// empty dir, no prompts
	d, _ := httpTestSetup(t, httpTestOpts{promptDir: t.TempDir(), webhooks: map[string]string{"missing": "nonexistent.md"}})
	mux := newTestMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/missing?sync=true", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

// TestWebhook_BadPath tests various malformed paths.
func TestWebhook_BadPath(t *testing.T) {
	d, _ := httpTestSetup(t, httpTestOpts{promptDir: t.TempDir()})
	mux := newTestMux(d)

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
	d, _ := httpTestSetup(t, httpTestOpts{promptDir: t.TempDir()})
	mux := newTestMux(d)

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

	d, _ := httpTestSetup(t, httpTestOpts{promptDir: dir, webhooks: map[string]string{"test": "test.md"}, noSession: true})
	mux := newTestMux(d)

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

	d, _ := httpTestSetup(t, httpTestOpts{promptDir: dir, webhooks: map[string]string{"test": "test.md"}})

	// Simulate recent session activity under the stable key the gate
	// consults directly.
	d.sessionIndex.SetSessionMetadata(testSessionKey, "last_activity", fmt.Sprintf("%d", time.Now().Unix()))

	mux := newTestMux(d)

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

	d, _ := httpTestSetup(t, httpTestOpts{promptDir: dir, webhooks: map[string]string{"test": "test.md"}})

	// Recent SESSION activity (cron-injected turn) but NO recent user
	// activity. --if-user-inactive should still allow the request through
	// (user has not been engaged), even though the session has been busy.
	d.sessionIndex.SetSessionMetadata(testSessionKey, "last_activity", fmt.Sprintf("%d", time.Now().Unix()))

	mux := newTestMux(d)

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
	d, _ := httpTestSetup(t, httpTestOpts{promptDir: t.TempDir()}) // nil webhooks
	mux := newTestMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/anything?sync=true", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}
