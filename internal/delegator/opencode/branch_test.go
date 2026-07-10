package opencode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"foci/internal/delegator"
)

// injectPooledServer registers a fake live Server (pointing at ts) under
// agentID and returns a cleanup func. White-box: Server fields are unexported.
func injectPooledServer(t *testing.T, agentID string, ts *httptest.Server) {
	t.Helper()
	srv := &Server{agentID: agentID, baseURL: ts.URL, http: ts.Client(), running: true}
	serverPoolMu.Lock()
	serverPool[agentID] = srv
	serverPoolMu.Unlock()
	t.Cleanup(func() {
		serverPoolMu.Lock()
		delete(serverPool, agentID)
		serverPoolMu.Unlock()
	})
}

func TestForkSession(t *testing.T) {
	const agentID = "brtest-fork"
	var gotForkPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/session/ses_parent/fork" {
			gotForkPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"ses_fork"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()
	injectPooledServer(t, agentID, ts)

	b := &Backend{}
	res, err := b.ForkSession(context.Background(), delegator.ForkRequest{ParentSessionID: "ses_parent", AgentID: agentID})
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if res.SessionID != "ses_fork" {
		t.Errorf("forked id = %q, want ses_fork", res.SessionID)
	}
	if gotForkPath != "/session/ses_parent/fork" {
		t.Errorf("fork endpoint not hit (got %q)", gotForkPath)
	}
}

func TestForkSessionErrors(t *testing.T) {
	b := &Backend{}
	cases := []struct {
		name string
		req  delegator.ForkRequest
	}{
		{"empty parent", delegator.ForkRequest{AgentID: "x"}},
		{"truncate unsupported", delegator.ForkRequest{ParentSessionID: "ses_x", TruncateAfter: 5, AgentID: "x"}},
		{"no pooled server", delegator.ForkRequest{ParentSessionID: "ses_x", AgentID: "brtest-absent"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := b.ForkSession(context.Background(), tc.req); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestCleanupSession(t *testing.T) {
	const agentID = "brtest-cleanup"
	var deleted string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/session/ses_fork" {
			deleted = "ses_fork"
			_, _ = w.Write([]byte(`true`))
			return
		}
		w.WriteHeader(http.StatusNotFound) // anything else → 404
	}))
	defer ts.Close()
	injectPooledServer(t, agentID, ts)

	b := &Backend{}
	if err := b.CleanupSession(context.Background(), delegator.CleanupRequest{SessionID: "ses_fork", AgentID: agentID}); err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}
	if deleted != "ses_fork" {
		t.Error("delete endpoint not hit")
	}
	// 404 (already gone) is treated as success.
	if err := b.CleanupSession(context.Background(), delegator.CleanupRequest{SessionID: "ses_missing", AgentID: agentID}); err != nil {
		t.Errorf("404 should be success, got %v", err)
	}
	// Empty id and absent server both error.
	if err := b.CleanupSession(context.Background(), delegator.CleanupRequest{AgentID: agentID}); err == nil {
		t.Error("expected error for empty session id")
	}
	if err := b.CleanupSession(context.Background(), delegator.CleanupRequest{SessionID: "x", AgentID: "brtest-absent"}); err == nil {
		t.Error("expected error when no server pooled")
	}
}
