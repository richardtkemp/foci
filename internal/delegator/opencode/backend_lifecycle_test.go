package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
)

// ---------------------------------------------------------------------------
// Test infrastructure
// ---------------------------------------------------------------------------

// newTestBackendServer returns an httptest.Server + a Backend whose .server
// field points at it. The httptest server handles the endpoint(s) the
// test wants to exercise; other endpoints return 404. The Backend is
// ready to be passed to Start (which sees b.server already set and
// skips acquireServer).
func newTestBackendServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Backend) {
	t.Helper()
	hs := httptest.NewServer(handler)
	t.Cleanup(hs.Close)
	srv := &Server{
		baseURL:  hs.URL,
		http:     hs.Client(),
		agentID:  "test-agent",
		sessions: map[string]*Backend{},
	}
	b := &Backend{
		server:      srv,
		agentID:     "test-agent",
		readyCh:     make(chan struct{}),
		outstanding: delegator.NewOutstandingRegistry(),
	}
	return hs, b
}

// ---------------------------------------------------------------------------
// Start
// ---------------------------------------------------------------------------

func TestBackend_Start_AcquiresServerAndCreatesSession(t *testing.T) {
	// Verifies Start: POST /session fires, the response's ID becomes
	// b.sessionID, the Backend registers under that ID, onSessionReady
	// fires with the ID, and readyCh closes.
	var (
		mu              sync.Mutex
		gotSessionPost  bool
		gotReadyID      string
		readyFired      atomic.Bool
		registeredUnder string
	)
	_, b := newTestBackendServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			mu.Lock()
			gotSessionPost = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"sess-created","title":"test"}`))
			return
		}
		http.NotFound(w, r)
	})

	b.SetOnSessionReady(func(id string) {
		readyFired.Store(true)
		mu.Lock()
		gotReadyID = id
		mu.Unlock()
	})

	if err := b.Start(context.Background(), delegator.StartOptions{AgentID: "test-agent"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !gotSessionPost {
		t.Error("POST /session was not invoked")
	}
	if b.sessionID != "sess-created" {
		t.Errorf("b.sessionID = %q, want sess-created", b.sessionID)
	}
	if !readyFired.Load() {
		t.Error("onSessionReady was not invoked")
	}
	if gotReadyID != "sess-created" {
		t.Errorf("onSessionReady id = %q, want sess-created", gotReadyID)
	}

	// Verify registration under the new sessionID.
	b.server.sessionsMu.RLock()
	registered := b.server.sessions[b.sessionID]
	b.server.sessionsMu.RUnlock()
	if registered == nil || registered != b {
		t.Errorf("Backend not registered under sessionID %q", b.sessionID)
	}
	registeredUnder = b.sessionID

	// readyCh should be closed.
	select {
	case <-b.readyCh:
	default:
		t.Error("readyCh not closed after Start")
	}
	_ = registeredUnder

	// Cleanup — Close the Backend to stop the dispatcher goroutine.
	_ = b.Close()
}

func TestBackend_Start_InjectsSystemPromptAsNoReply(t *testing.T) {
	// Verifies that when StartOptions.SystemPrompt is set, Start POSTs
	// it to /session/:id/message with noReply:true so opencode treats it
	// as context-only (no AI response). Mirrors ccstream's
	// --append-system-prompt flag.
	const prompt = "You are a helpful test assistant."
	var (
		mu         sync.Mutex
		gotMessage bool
		bodyBytes  []byte
		gotPath    string
	)
	_, b := newTestBackendServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"sess-prompt","title":""}`))
			return
		}
		if r.URL.Path == "/session/sess-prompt/message" && r.Method == http.MethodPost {
			mu.Lock()
			gotMessage = true
			gotPath = r.URL.Path
			bodyBytes, _ = io.ReadAll(r.Body)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	})

	err := b.Start(context.Background(), delegator.StartOptions{
		AgentID:      "test-agent",
		SystemPrompt: prompt,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !gotMessage {
		t.Fatal("system prompt was not POSTed to /session/:id/message")
	}
	if gotPath != "/session/sess-prompt/message" {
		t.Errorf("message POST path = %q, want /session/sess-prompt/message", gotPath)
	}

	// Verify noReply:true + parts[0].text == prompt.
	var body struct {
		NoReply bool `json:"noReply"`
		Parts   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !body.NoReply {
		t.Error("noReply = false, want true (system prompt must not trigger AI response)")
	}
	if len(body.Parts) != 1 || body.Parts[0].Type != "text" || body.Parts[0].Text != prompt {
		t.Errorf("parts = %+v, want [{type:text text:%q}]", body.Parts, prompt)
	}
	_ = b.Close()
}

func TestBackend_Start_PromptFuncOverridesStatic(t *testing.T) {
	// Verifies SystemPromptFunc (when non-nil and returns non-empty)
	// takes precedence over the static SystemPrompt field — matches
	// ccstream's behaviour where the per-session rebuild wins over the
	// frozen-at-setup snapshot (#828).
	const staticPrompt = "static"
	const dynamicPrompt = "dynamic-from-func"
	var gotBody []byte
	_, b := newTestBackendServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"sess-func"}`))
			return
		}
		if r.URL.Path == "/session/sess-func/message" {
			gotBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	})

	err := b.Start(context.Background(), delegator.StartOptions{
		AgentID:      "test-agent",
		SystemPrompt: staticPrompt,
		SystemPromptFunc: func() string {
			return dynamicPrompt
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !strings.Contains(string(gotBody), dynamicPrompt) {
		t.Errorf("body did not contain dynamic prompt %q: %s", dynamicPrompt, string(gotBody))
	}
	if strings.Contains(string(gotBody), staticPrompt) {
		t.Errorf("body should not contain static prompt when func returned non-empty: %s", string(gotBody))
	}
	_ = b.Close()
}

func TestBackend_Start_ReadyFires(t *testing.T) {
	// Verifies readyCh closes after Start completes. The plan called
	// this TestBackend_Start_ReadyFires; it's a tighter check than the
	// "registered" assertion in TestBackend_Start_AcquiresServerAndCreatesSession.
	_, b := newTestBackendServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"sess-ready"}`))
			return
		}
		http.NotFound(w, r)
	})

	done := make(chan error, 1)
	go func() {
		done <- b.Start(context.Background(), delegator.StartOptions{AgentID: "test-agent"})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return")
	}

	select {
	case <-b.readyCh:
		// expected
	case <-time.After(time.Second):
		t.Error("readyCh not closed after Start returned")
	}
	_ = b.Close()

}

func TestBackend_Start_SessionCreateFailure(t *testing.T) {
	// Verifies Start surfaces a /session creation failure cleanly. The
	// Backend should not be marked running and the session should not
	// be registered.
	_, b := newTestBackendServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	})

	err := b.Start(context.Background(), delegator.StartOptions{AgentID: "test-agent"})
	if err == nil {
		t.Fatal("Start should fail when /session returns 500")
	}
	if !strings.Contains(err.Error(), "create session") {
		t.Errorf("err = %v, want it to mention 'create session'", err)
	}
	if b.IsRunning() {
		t.Error("Backend marked running after Start failure")
	}
	if b.sessionID != "" {
		t.Errorf("b.sessionID = %q, want empty after Start failure", b.sessionID)
	}
}

func TestBackend_Start_ResumeExistingSession(t *testing.T) {
	// Verifies: when ResumeSessionID is set and GET /session/:id returns
	// 200, the Backend reuses that session ID. No POST /session fires,
	// and the system prompt is NOT reinjected (the resumed session
	// already has it from original creation).
	var (
		mu              sync.Mutex
		gotSessionPost  bool
		gotResumeGet    bool
		gotPromptInject bool
	)
	_, b := newTestBackendServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/ses-saved" && r.Method == http.MethodGet {
			mu.Lock()
			gotResumeGet = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"ses-saved"}`))
			return
		}
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			mu.Lock()
			gotSessionPost = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"should-not-happen"}`))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/session/ses-saved/message") {
			mu.Lock()
			gotPromptInject = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	})

	err := b.Start(context.Background(), delegator.StartOptions{
		AgentID:         "test-agent",
		ResumeSessionID: "ses-saved",
		SystemPrompt:    "should-not-be-injected",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !gotResumeGet {
		t.Error("GET /session/ses-saved was not invoked")
	}
	if gotSessionPost {
		t.Error("POST /session should not fire when resume succeeds")
	}
	if gotPromptInject {
		t.Error("system prompt should not be reinjected on resume")
	}
	if b.sessionID != "ses-saved" {
		t.Errorf("b.sessionID = %q, want ses-saved", b.sessionID)
	}
	_ = b.Close()
}

func TestBackend_Start_ResumeErrorsOn404(t *testing.T) {
	// Verifies: when ResumeSessionID is set but GET /session/:id returns 404
	// (session evicted, db wiped), Start FAILS rather than silently creating a
	// new session inline. DelegatedManager's retry-without-resume path owns the
	// fallback (creates the fresh session AND alerts the user), so Start must
	// NOT POST /session itself here.
	var (
		mu            sync.Mutex
		gotResumeGet  bool
		gotCreatePost bool
	)
	_, b := newTestBackendServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/ses-stale" && r.Method == http.MethodGet {
			mu.Lock()
			gotResumeGet = true
			mu.Unlock()
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			mu.Lock()
			gotCreatePost = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"sess-fresh"}`))
			return
		}
		http.NotFound(w, r)
	})

	err := b.Start(context.Background(), delegator.StartOptions{
		AgentID:         "test-agent",
		ResumeSessionID: "ses-stale",
		SystemPrompt:    "fresh-prompt",
	})
	if err == nil {
		t.Fatal("Start should fail when the resume session returns 404")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !gotResumeGet {
		t.Error("GET /session/ses-stale was not invoked")
	}
	if gotCreatePost {
		t.Error("POST /session must NOT fire on 404 — the manager owns the fallback")
	}
	_ = b.Close()
}

func TestBackend_Start_ResumeFailsOnServerError(t *testing.T) {
	// Verifies: when GET /session/:id returns a non-404 error (e.g. 500),
	// Start fails instead of silently falling through to create. This
	// prevents a transient server hiccup from discarding the resume ID —
	// the manager's retry-without-resume path handles the fallback.
	_, b := newTestBackendServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/ses-probe" && r.Method == http.MethodGet {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"should-not-happen"}`))
			return
		}
		http.NotFound(w, r)
	})

	err := b.Start(context.Background(), delegator.StartOptions{
		AgentID:         "test-agent",
		ResumeSessionID: "ses-probe",
	})
	if err == nil {
		t.Fatal("Start should fail when resume probe returns 500")
	}
	if !strings.Contains(err.Error(), "probe resume session") {
		t.Errorf("err = %v, want it to mention 'probe resume session'", err)
	}
	if b.IsRunning() {
		t.Error("Backend marked running after resume probe failure")
	}
}

func TestBackend_Start_Idempotent(t *testing.T) {
	// Verifies calling Start twice on the same Backend is a no-op:
	// the second call returns nil without re-POSTing /session,
	// re-closing readyCh (which would panic), or re-firing
	// onSessionReady. DelegatedManager creates a fresh Backend per
	// session so this doesn't occur in production, but the guard
	// prevents a footgun for tests and future callers.
	var (
		sessionPosts atomic.Int32
		readyFires   atomic.Int32
	)
	_, b := newTestBackendServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			sessionPosts.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"sess-once"}`))
			return
		}
		http.NotFound(w, r)
	})
	b.SetOnSessionReady(func(string) { readyFires.Add(1) })

	if err := b.Start(context.Background(), delegator.StartOptions{AgentID: "test-agent"}); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := b.Start(context.Background(), delegator.StartOptions{AgentID: "test-agent"}); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	if got := sessionPosts.Load(); got != 1 {
		t.Errorf("POST /session fired %d times, want 1 (second Start should be no-op)", got)
	}
	if got := readyFires.Load(); got != 1 {
		t.Errorf("onSessionReady fired %d times, want 1", got)
	}
	_ = b.Close()
}

// ---------------------------------------------------------------------------
// Close
// ---------------------------------------------------------------------------

func TestBackend_Close_DeregistersAndReleases(t *testing.T) {
	// Verifies Close deregisters the Backend from the Server's session
	// map and the dispatcher goroutine exits. Also verifies a second
	// Close is a no-op (idempotent).
	var deleteCalled atomic.Bool
	_, b := newTestBackendServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"sess-close"}`))
			return
		}
		if r.URL.Path == "/session/sess-close" && r.Method == http.MethodDelete {
			deleteCalled.Store(true)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	})

	if err := b.Start(context.Background(), delegator.StartOptions{AgentID: "test-agent"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Confirm registration.
	b.server.sessionsMu.RLock()
	_, ok := b.server.sessions["sess-close"]
	b.server.sessionsMu.RUnlock()
	if !ok {
		t.Fatal("Backend not registered before Close")
	}

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify deregister.
	b.server.sessionsMu.RLock()
	_, ok = b.server.sessions["sess-close"]
	b.server.sessionsMu.RUnlock()
	if ok {
		t.Error("Backend still registered after Close")
	}

	// Verify DELETE /session/sess-close fired.
	if !deleteCalled.Load() {
		t.Error("DELETE /session/:id was not invoked")
	}

	// Verify dispatcher stopped — be.stopDispatcher should be nil.
	if b.stopDispatcher != nil {
		t.Error("be.stopDispatcher should be nil after Close")
	}

	// Second Close is a no-op.
	if err := b.Close(); err != nil {
		t.Errorf("second Close returned error: %v", err)
	}

}

func TestBackend_Close_LastReleaseShutsDownServer(t *testing.T) {
	// Verifies the full chain: Backend.Close → releaseServer →
	// (refcount→0) → Server.Close (in goroutine) → subprocess killed.
	// Uses a real opc-stub subprocess so the kill is observable, not
	// just the pool-entry removal.
	//
	// Step 3's TestServer_Pool_RefcountShutdown covers pool semantics
	// without a subprocess; this test pins the Backend → release → kill
	// composition end-to-end.
	prevGrace, prevKill, prevTerm := closeGracefulWait, closeSigkillWait, closeSigtermWait
	closeGracefulWait = 200 * time.Millisecond
	closeSigkillWait = 200 * time.Millisecond
	closeSigtermWait = 200 * time.Millisecond
	defer func() {
		closeGracefulWait = prevGrace
		closeSigkillWait = prevKill
		closeSigtermWait = prevTerm
	}()

	resetTestPool(t)

	// Spawn a real stub subprocess via the same helper Step 3 uses.
	srv := newServer("agent-shutdown", serverConfig{
		workDir:    t.TempDir(),
		binaryPath: stubBinary(t),
		hostname:   "127.0.0.1",
	})
	if err := launchStubDirectly(t, srv); err != nil {
		t.Fatalf("launchStubDirectly: %v", err)
	}

	// Insert into the pool with refcount=1 so releaseServer will
	// trigger shutdown when this Backend closes.
	serverPoolMu.Lock()
	serverPool["agent-shutdown"] = srv
	srv.refCount = 1
	serverPoolMu.Unlock()

	// Construct a Backend that "owns" this Server reference. Manually
	// mark running so Close's no-op-on-not-running guard passes, and
	// register so the dispatcher is started (Close will stop it).
	b := &Backend{
		server:      srv,
		agentID:     "agent-shutdown",
		sessionID:   "sess-shutdown",
		readyCh:     make(chan struct{}),
		outstanding: delegator.NewOutstandingRegistry(),
	}
	b.mu.Lock()
	b.running = true
	b.mu.Unlock()
	srv.registerSession(b)

	// Pre-condition: subprocess is alive.
	if srv.cmd.Process == nil {
		t.Fatal("subprocess has no Process handle")
	}

	// Pre-condition: agent in pool.
	if _, ok := lookupTestPool("agent-shutdown"); !ok {
		t.Fatal("agent not in pool before Close")
	}

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Post-condition 1: pool entry removed synchronously (releaseServer
	// removes under the mutex before spawning the Close goroutine).
	if _, ok := lookupTestPool("agent-shutdown"); ok {
		t.Error("agent still in pool after Close — refcount wasn't decremented")
	}

	// Post-condition 2: subprocess killed. releaseServer spawns Close
	// in a goroutine; with shrunk timeouts, the kill ladder completes
	// in <1s. srv.done is the authoritative "subprocess reaped" signal
	// (launchStubDirectly closes it after cmd.Wait() returns).
	select {
	case <-srv.done:
		// subprocess reaped
	case <-time.After(2 * time.Second):
		t.Fatal("subprocess not killed within 2s of Backend.Close")
	}
}

// ---------------------------------------------------------------------------
// WaitReady
// ---------------------------------------------------------------------------

func TestBackend_WaitReady_AfterStart(t *testing.T) {
	// Verifies WaitReady returns nil once Start completes (readyCh closed).
	_, b := newTestBackendServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"sess-wait"}`))
			return
		}
		http.NotFound(w, r)
	})

	if err := b.Start(context.Background(), delegator.StartOptions{AgentID: "test-agent"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// readyCh should be closed; WaitReady returns immediately.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.WaitReady(ctx); err != nil {
		t.Errorf("WaitReady: %v", err)
	}
	_ = b.Close()

}

func TestBackend_WaitReady_ServerDeath(t *testing.T) {
	// Verifies WaitReady returns an error mentioning the exit when the
	// Server dies before readyCh closes. Plan §5.1 called out this
	// early-exit path so callers can retry without burning the full
	// ready-timeout budget.
	_, b := newTestBackendServer(t, func(http.ResponseWriter, *http.Request) {})
	// Simulate subprocess death by closing server.done. The test helper
	// doesn't init done (no real subprocess), so allocate a channel
	// first to model the death signal.
	b.server.done = make(chan struct{})
	close(b.server.done)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := b.WaitReady(ctx)
	if err == nil {
		t.Fatal("WaitReady should error when server dies before readyCh")
	}
	if !strings.Contains(err.Error(), "server died") {
		t.Errorf("err = %v, want it to mention 'server died'", err)
	}
}

func TestBackend_WaitReady_ContextExpiry(t *testing.T) {
	// Verifies WaitReady honours ctx — if Start hasn't completed (and
	// server hasn't died), it returns ctx.Err() on deadline.
	_, b := newTestBackendServer(t, func(http.ResponseWriter, *http.Request) {})
	// Don't call Start. readyCh stays open; server.done stays open.

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := b.WaitReady(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

// ---------------------------------------------------------------------------
// CheckReady
// ---------------------------------------------------------------------------

func TestBackend_CheckReady_Healthy(t *testing.T) {
	// Verifies CheckReady returns (true, nil) when /global/health
	// reports {healthy: true}.
	_, b := newTestBackendServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/global/health" {
			_, _ = w.Write([]byte(`{"healthy": true, "version": "test"}`))
			return
		}
		http.NotFound(w, r)
	})

	ok, err := b.CheckReady(context.Background())
	if err != nil {
		t.Fatalf("CheckReady: %v", err)
	}
	if !ok {
		t.Error("ok = false, want true")
	}

}

func TestBackend_CheckReady_Unhealthy(t *testing.T) {
	// Verifies CheckReady returns (false, err) when /global/health
	// reports {healthy: false} — the server is up but not ready.
	_, b := newTestBackendServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/global/health" {
			_, _ = w.Write([]byte(`{"healthy": false, "version": ""}`))
			return
		}
		http.NotFound(w, r)
	})

	ok, err := b.CheckReady(context.Background())
	if ok {
		t.Error("ok = true, want false (server unhealthy)")
	}
	if err == nil {
		t.Error("err = nil, want error mentioning unhealthy")
	}

}

func TestBackend_CheckReady_TransportError(t *testing.T) {
	// Verifies CheckReady returns (false, err) when the server is
	// unreachable (transport error). Uses a closed httptest.Server so
	// the connection fails fast.
	hs := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	hs.Close()
	b := &Backend{
		server: &Server{baseURL: hs.URL, http: hs.Client()},
	}

	ok, err := b.CheckReady(context.Background())
	if ok {
		t.Error("ok = true, want false on transport error")
	}
	if err == nil {
		t.Error("err = nil on transport error")
	}
}

func TestBackend_CheckReady_NilServer(t *testing.T) {
	// Verifies CheckReady on a Backend that hasn't had Start called
	// (b.server == nil) returns an explicit error rather than panicking.
	b := &Backend{}
	ok, err := b.CheckReady(context.Background())
	if ok {
		t.Error("ok = true, want false")
	}
	if err == nil {
		t.Error("err = nil, want error about missing server")
	}
}
