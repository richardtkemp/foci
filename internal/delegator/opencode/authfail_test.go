package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"foci/internal/delegator"
)

// ---------------------------------------------------------------------------
// fireAuthFailure — CAS gating
// ---------------------------------------------------------------------------

func TestFireAuthFailure_FiresOncePerServer(t *testing.T) {
	// Verifies fireAuthFailure is gated by an atomic CAS — calling it
	// twice fires the onAuthFailure callback exactly once. Prevents a
	// flaky 401 loop from spamming repeated notifications.
	var fired atomic.Int32
	b := &Backend{
		sessionID:   "sess-test",
		outstanding: delegator.NewOutstandingRegistry(),
	}
	b.mu.Lock()
	b.onAuthFailure = func(d string) { fired.Add(1) }
	b.mu.Unlock()

	b.fireAuthFailure("first")
	b.fireAuthFailure("second")
	b.fireAuthFailure("third")

	if got := fired.Load(); got != 1 {
		t.Errorf("onAuthFailure fired %d times, want 1 (CAS gate)", got)
	}
}

// ---------------------------------------------------------------------------
// HTTP 401 detection
// ---------------------------------------------------------------------------

func TestSendPrompt_On401FiresAuthFailure(t *testing.T) {
	// Verifies postMessage detects HTTP 401 and fires auth failure via
	// checkHTTP401. The fan-out path is tested separately; here we
	// verify the detection point.
	var authFired atomic.Bool
	var authDetail string

	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			json.NewEncoder(w).Encode(map[string]string{"id": "sess-401"})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer hs.Close()

	srv := &Server{
		baseURL:  hs.URL,
		http:     hs.Client(),
		agentID:  "test-401",
		sessions: map[string]*Backend{},
	}
	srv.wrapAuthCheckingTransport()
	b := &Backend{
		server:      srv,
		agentID:     "test-401",
		sessionID:   "sess-401",
		outstanding: delegator.NewOutstandingRegistry(),
	}
	b.mu.Lock()
	b.onAuthFailure = func(d string) {
		authFired.Store(true)
		authDetail = d
	}
	b.mu.Unlock()
	// Register so fanOutAuthFailure reaches this Backend.
	srv.sessions["sess-401"] = b

	// Call postMessage directly — it hits a 401 and should trigger
	// checkHTTP401 → fanOutAuthFailure → fireAuthFailure.
	err := b.postMessage(context.Background(), "/prompt_async", []byte(`{"parts":[]}`))
	_ = err // we expect an error; the important assertion is authFired

	if !authFired.Load() {
		t.Fatal("onAuthFailure was not fired after HTTP 401")
	}
	if authDetail == "" {
		t.Error("authDetail is empty")
	}
}

// ---------------------------------------------------------------------------
// Server-level fan-out
// ---------------------------------------------------------------------------

func TestServer_FansOutAuthFailureToAllBackends(t *testing.T) {
	// Verifies Server.fanOutAuthFailure fires fireAuthFailure on every
	// registered Backend. Because auth failures are account-wide, all
	// sessions on the Server must know.
	var fired1, fired2 atomic.Bool

	srv := &Server{
		agentID:  "test-fanout",
		sessions: map[string]*Backend{},
	}

	b1 := &Backend{sessionID: "sess-1", outstanding: delegator.NewOutstandingRegistry()}
	b1.mu.Lock()
	b1.onAuthFailure = func(d string) { fired1.Store(true) }
	b1.mu.Unlock()

	b2 := &Backend{sessionID: "sess-2", outstanding: delegator.NewOutstandingRegistry()}
	b2.mu.Lock()
	b2.onAuthFailure = func(d string) { fired2.Store(true) }
	b2.mu.Unlock()

	srv.sessions["sess-1"] = b1
	srv.sessions["sess-2"] = b2

	srv.fanOutAuthFailure("account-wide auth failure")

	if !fired1.Load() {
		t.Error("Backend 1 did not fire onAuthFailure")
	}
	if !fired2.Load() {
		t.Error("Backend 2 did not fire onAuthFailure")
	}
}

func TestFireAuthFailure_NilCallback(t *testing.T) {
	// Verifies fireAuthFailure with nil onAuthFailure doesn't panic —
	// it just logs and returns. Important for robustness: the callback
	// might not be wired in test setups.
	b := &Backend{
		sessionID:   "sess-test",
		outstanding: delegator.NewOutstandingRegistry(),
		// onAuthFailure is nil
	}
	b.fireAuthFailure("test detail")
	// No assertion — if we got here without panicking, the test passes.
}

// ---------------------------------------------------------------------------
// checkHTTP401 — only fires on 401, not on other statuses
// ---------------------------------------------------------------------------

func TestCheckHTTP401_Non401DoesNotFire(t *testing.T) {
	// Verifies checkHTTP401 only triggers on 401, not on 200/500/etc.
	var authFired atomic.Bool
	b := &Backend{
		outstanding: delegator.NewOutstandingRegistry(),
	}
	b.mu.Lock()
	b.onAuthFailure = func(d string) { authFired.Store(true) }
	b.mu.Unlock()

	b.checkHTTP401(http.StatusOK, "/test")
	b.checkHTTP401(http.StatusInternalServerError, "/test")
	b.checkHTTP401(http.StatusForbidden, "/test")

	if authFired.Load() {
		t.Error("onAuthFailure fired for non-401 status")
	}
}

// Suppress unused import (io may not be referenced after test trimming).
var _ = io.EOF
