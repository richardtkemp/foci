package opencode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
)

var _ delegator.RunningBackendCleaner = (*Backend)(nil)

// deleteRecorder is a stand-in opencode server that records DELETE /session/{id}.
func deleteRecorder(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		gone []string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/session/") {
			mu.Lock()
			gone = append(gone, strings.TrimPrefix(r.URL.Path, "/session/"))
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)
	return ts, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), gone...)
	}
}

// The #1707 state itself: an idle agent has no pooled server, so CleanupSession
// cannot delete anything. This is the control — if it ever stops failing, the
// scope test below is proving nothing.
func TestCleanupSession_FailsWithNoPooledServer(t *testing.T) {
	const agentID = "idle-agent"
	resetTestPool(t)

	b := &Backend{cfg: map[string]any{}}
	err := b.CleanupSession(context.Background(), delegator.CleanupRequest{
		SessionID: "ses_abc", AgentID: agentID,
	})
	if err == nil {
		t.Fatal("CleanupSession with an empty pool succeeded, want 'no running server'")
	}
	if !strings.Contains(err.Error(), "no running server") {
		t.Errorf("err = %v, want 'no running server'", err)
	}
}

// The fix: one scope spawns the server for an idle agent, every CleanupSession
// inside it succeeds against that server, and release reaps it.
func TestOpenCleanupScope_SpawnsOnceAndServesTheWholeSweep(t *testing.T) {
	const agentID = "idle-agent"
	resetTestPool(t)

	ts, deleted := deleteRecorder(t)

	var (
		calls  int
		gotCfg serverConfig
		gotEnv map[string]string
	)
	orig := acquireServerFn
	acquireServerFn = func(id string, cfg serverConfig, env map[string]string) (*Server, error) {
		calls++
		gotCfg = cfg
		gotEnv = env
		// Mimic a real spawn: pool a live Server with refCount=1 so the
		// scope's real releaseServer decrements a truthful count.
		srv := &Server{agentID: id, baseURL: ts.URL, http: ts.Client(), running: true, refCount: 1}
		serverPoolMu.Lock()
		serverPool[id] = srv
		serverPoolMu.Unlock()
		return srv, nil
	}
	t.Cleanup(func() { acquireServerFn = orig })

	b := &Backend{cfg: map[string]any{"binary": "custom-opencode", "hostname": "10.0.0.9"}}
	release, err := b.OpenCleanupScope(context.Background(), delegator.CleanupRequest{
		AgentID: agentID, WorkDir: "/tmp/agent-workdir",
	})
	if err != nil {
		t.Fatalf("OpenCleanupScope: %v", err)
	}

	// The sweep: several deletes, all inside the one scope.
	for _, id := range []string{"ses_a", "ses_b", "ses_c"} {
		if err := b.CleanupSession(context.Background(), delegator.CleanupRequest{
			SessionID: id, AgentID: agentID,
		}); err != nil {
			t.Errorf("CleanupSession(%s) inside scope: %v", id, err)
		}
	}
	release()

	if calls != 1 {
		t.Errorf("acquireServerFn calls = %d across 3 deletes, want 1", calls)
	}
	if got := deleted(); len(got) != 3 {
		t.Errorf("server saw DELETEs %v, want 3", got)
	}
	// Config must come from b.cfg — a cleanup spawn is not a divergent
	// configuration from an interactive one.
	if gotCfg.binaryPath != "custom-opencode" || gotCfg.hostname != "10.0.0.9" {
		t.Errorf("cfg = %+v, want binary/hostname from b.cfg", gotCfg)
	}
	if gotEnv != nil {
		t.Errorf("env = %v, want nil (no interactive session to route)", gotEnv)
	}
	// Sole holder released → server reaped, not pinned open forever.
	serverPoolMu.Lock()
	_, stillPooled := serverPool[agentID]
	serverPoolMu.Unlock()
	if stillPooled {
		t.Error("server still pooled after release — refcount leaked")
	}
}

func TestOpenCleanupScope_RejectsEmptyAgentID(t *testing.T) {
	b := &Backend{cfg: map[string]any{}}
	if _, err := b.OpenCleanupScope(context.Background(), delegator.CleanupRequest{}); err == nil {
		t.Error("empty agent id accepted, want error")
	}
}
