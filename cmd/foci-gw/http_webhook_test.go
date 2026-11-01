package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/session"
)

// TestWebhook_SyncSuccess tests the happy path: known agent, configured hook ID,
// existing prompt file, body payload, sync=true → 200 with agent response.
func TestWebhook_SyncSuccess(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestWebhook_NoSession_StartsIndependent proves that a webhook with no
// ?session= no longer 412s when the agent has no default chat session. A
// webhook is an external event source, not a human conversation: absent an
// explicit session it defaults to a per-hook INDEPENDENT session
// (agent/i<hookID>, here test-agent/itest), created lazily (RungCreated) and
// dispatched to — so the request is ACCEPTED and delivered.
func TestWebhook_NoSession_StartsIndependent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.md"), []byte("prompt text"), 0644)

	d, mock := httpTestSetup(t, httpTestOpts{promptDir: dir, webhooks: map[string]string{"test": "test.md"}, noSession: true})
	mux := newTestMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/test?sync=true", nil)
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
	// The webhook prompt reached the backend (the lazily-created independent
	// session dispatched rather than erroring with "no active session").
	if !strings.Contains(mock.lastText(), "prompt text") {
		t.Errorf("agent message missing prompt text, got: %s", mock.lastText())
	}
}

// TestWebhook_IfInactive tests that the if_inactive query param skips when
// the targeted session has recent activity. "Activity" here means cache
// freshness — read from session_index.last_cache_touch (bumped by any turn-init
// path, memory turns included) rather than session_index.last_user_activity_at
// (user inbound only). The narrower user-only behaviour lives behind
// --if-user-inactive.
func TestWebhook_IfInactive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.md"), []byte("prompt"), 0644)

	d, _ := httpTestSetup(t, httpTestOpts{promptDir: dir, webhooks: map[string]string{"test": "test.md"}})

	// A no-session webhook defaults to the per-hook independent session
	// (test-agent/itest). Seed that row and mark it recently active under the
	// stable key the gate consults directly, so if_inactive skips.
	hookSessionKey := "test-agent/itest"
	d.sessionIndex.Upsert(session.SessionIndexEntry{SessionKey: hookSessionKey, FilePath: "x", SessionType: session.SessionTypeIndependent, Status: session.SessionStatusActive})
	d.sessionIndex.TouchCacheTouch(hookSessionKey, time.Now())

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
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.md"), []byte("prompt"), 0644)

	d, _ := httpTestSetup(t, httpTestOpts{promptDir: dir, webhooks: map[string]string{"test": "test.md"}})

	// Recent SESSION activity (cron-injected turn) but NO recent user
	// activity. --if-user-inactive should still allow the request through
	// (user has not been engaged), even though the session has been busy.
	d.sessionIndex.TouchCacheTouch(testSessionKey, time.Now())

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
	t.Parallel()
	d, _ := httpTestSetup(t, httpTestOpts{promptDir: t.TempDir()}) // nil webhooks
	mux := newTestMux(d)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/anything?sync=true", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

// --- #1134: per-hook session setting -------------------------------------

// postWebhookSync POSTs a sync webhook call and returns the decoded receipt.
func postWebhookSync(t *testing.T, mux http.Handler, path string) map[string]string {
	t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	req := httptest.NewRequest(http.MethodPost, path+sep+"sync=true", strings.NewReader("payload"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s: status = %d, want 200; body: %s", path, w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("POST %s: decode: %v (%s)", path, err, w.Body.String())
	}
	return resp
}

// webhookSessionSetup builds a harness with one hook "h" (prompt h.md) and
// the given session setting.
func webhookSessionSetup(t *testing.T, sessionSetting string) (httpHandlerDeps, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "h.md"), []byte("hook prompt"), 0644)
	d, _ := httpTestSetup(t, httpTestOpts{promptDir: dir, hooks: map[string]config.WebhookConfig{
		"h": {Prompt: "h.md", Session: sessionSetting},
	}})
	return d, newTestMux(d)
}

func TestWebhookSession_OldFormIsPersistent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "h.md"), []byte("hook prompt"), 0644)
	d, _ := httpTestSetup(t, httpTestOpts{promptDir: dir, webhooks: map[string]string{"h": "h.md"}})
	mux := newTestMux(d)
	for i := range 2 {
		if got := postWebhookSync(t, mux, "/webhook/test-agent/h")["session"]; got != "test-agent/ih" {
			t.Errorf("call %d: session = %q, want test-agent/ih", i, got)
		}
	}
}

func TestWebhookSession_Persistent(t *testing.T) {
	t.Parallel()
	_, mux := webhookSessionSetup(t, config.WebhookSessionPersistent)
	for i := range 2 {
		if got := postWebhookSync(t, mux, "/webhook/test-agent/h")["session"]; got != "test-agent/ih" {
			t.Errorf("call %d: session = %q, want test-agent/ih", i, got)
		}
	}
}

func TestWebhookSession_FixedName(t *testing.T) {
	t.Parallel()
	_, mux := webhookSessionSetup(t, "ops")
	if got := postWebhookSync(t, mux, "/webhook/test-agent/h")["session"]; got != "test-agent/iops" {
		t.Errorf("fixed: session = %q, want test-agent/iops", got)
	}
	// ?session= overrides the configured name.
	if got := postWebhookSync(t, mux, "/webhook/test-agent/h?session=other")["session"]; got != "test-agent/iother" {
		t.Errorf("override: session = %q, want test-agent/iother", got)
	}
}

// A fixed name resolves like ?session=: an existing chat key is reached exactly.
func TestWebhookSession_FixedNameResolvesLikeQuery(t *testing.T) {
	t.Parallel()
	_, mux := webhookSessionSetup(t, testSessionKey)
	if got := postWebhookSync(t, mux, "/webhook/test-agent/h")["session"]; got != testSessionKey {
		t.Errorf("session = %q, want %q", got, testSessionKey)
	}
}

func assertFreshDistinct(t *testing.T, a, b string) {
	t.Helper()
	if a == b {
		t.Errorf("two calls shared session %q, want distinct fresh sessions", a)
	}
	for _, k := range []string{a, b} {
		if k == "test-agent/ih" {
			t.Errorf("session = %q: the persistent per-hook session, want a fresh one", k)
		}
		if !strings.HasPrefix(k, "test-agent/i") {
			t.Errorf("session = %q, want an independent root of test-agent", k)
		}
		if typ := session.ClassifySessionKey(k); typ != session.SessionTypeIndependent {
			t.Errorf("session %q type = %q, want independent", k, typ)
		}
	}
}

func TestWebhookSession_NewPerCall(t *testing.T) {
	t.Parallel()
	_, mux := webhookSessionSetup(t, config.WebhookSessionNewPerCall)
	r1 := postWebhookSync(t, mux, "/webhook/test-agent/h")
	r2 := postWebhookSync(t, mux, "/webhook/test-agent/h")
	assertFreshDistinct(t, r1["session"], r2["session"])
	if r1["resolved_via"] != "created" {
		t.Errorf("resolved_via = %q, want created", r1["resolved_via"])
	}
	// ?session= still overrides new-per-call.
	if got := postWebhookSync(t, mux, "/webhook/test-agent/h?session=pinned")["session"]; got != "test-agent/ipinned" {
		t.Errorf("override: session = %q, want test-agent/ipinned", got)
	}
}

// ?new_session=true asks for a fresh session for one call on any hook.
func TestWebhookSession_PerCallQueryOnPersistentHook(t *testing.T) {
	t.Parallel()
	_, mux := webhookSessionSetup(t, "")
	r1 := postWebhookSync(t, mux, "/webhook/test-agent/h?new_session=true")
	r2 := postWebhookSync(t, mux, "/webhook/test-agent/h?new_session=true")
	assertFreshDistinct(t, r1["session"], r2["session"])
	// Without it the hook is back on its persistent session.
	if got := postWebhookSync(t, mux, "/webhook/test-agent/h")["session"]; got != "test-agent/ih" {
		t.Errorf("session = %q, want test-agent/ih", got)
	}
}

// A ?session= value that looks like a config keyword still names a session.
func TestWebhookSession_QueryKeywordIsAName(t *testing.T) {
	t.Parallel()
	_, mux := webhookSessionSetup(t, "")
	for _, name := range []string{"new", config.WebhookSessionNewPerCall, config.WebhookSessionPersistent} {
		if got := postWebhookSync(t, mux, "/webhook/test-agent/h?session="+name)["session"]; got != "test-agent/i"+name {
			t.Errorf("?session=%s: session = %q, want test-agent/i%s", name, got, name)
		}
	}
}

func TestWebhookSession_BadQueryCombos(t *testing.T) {
	t.Parallel()
	_, mux := webhookSessionSetup(t, "")
	for _, q := range []string{"session=x&new_session=true", "new_session=maybe"} {
		req := httptest.NewRequest(http.MethodPost, "/webhook/test-agent/h?sync=true&"+q, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("?%s: status = %d, want 400; body: %s", q, w.Code, w.Body.String())
		}
	}
}

// A fresh session has no history: if_active skips, if_inactive runs.
func TestWebhookSession_NewPerCallGates(t *testing.T) {
	t.Parallel()
	d, mux := webhookSessionSetup(t, config.WebhookSessionNewPerCall)
	// Make the persistent per-hook session and the default chat busy, so a
	// gate that looked at either would answer the other way.
	for _, k := range []string{"test-agent/ih", testSessionKey} {
		d.sessionIndex.Upsert(session.SessionIndexEntry{SessionKey: k, FilePath: "x", SessionType: session.SessionTypeIndependent, Status: session.SessionStatusActive})
		d.sessionIndex.TouchCacheTouch(k, time.Now())
	}
	if got := postWebhookSync(t, mux, "/webhook/test-agent/h?if_active=1h")["response"]; got != "skipped: no recent activity" {
		t.Errorf("if_active=1h: response = %q, want skipped: no recent activity", got)
	}
	if got := postWebhookSync(t, mux, "/webhook/test-agent/h?if_inactive=1h")["response"]; got != mockReply {
		t.Errorf("if_inactive=1h: response = %q, want the turn to run (%q)", got, mockReply)
	}
}
