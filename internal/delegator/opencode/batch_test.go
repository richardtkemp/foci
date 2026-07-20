package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
)

// batchServer fakes the opencode routes RunBatch touches. The message list
// reports an incomplete assistant message on the first poll and a completed
// one afterwards, exercising the poll loop.
func batchServer(t *testing.T) (*httptest.Server, *struct {
	promptBody atomic.Value // string
	deleted    atomic.Bool
	polls      atomic.Int32
}) {
	t.Helper()
	state := &struct {
		promptBody atomic.Value
		deleted    atomic.Bool
		polls      atomic.Int32
	}{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "ses_batch1"})
		case r.Method == http.MethodPost && r.URL.Path == "/session/ses_batch1/prompt_async":
			body, _ := io.ReadAll(r.Body)
			state.promptBody.Store(string(body))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/session/ses_batch1/message":
			n := state.polls.Add(1)
			if n == 1 {
				fmt.Fprint(w, `[{"info":{"role":"user","time":{}},"parts":[{"type":"text","text":"q"}]},
					{"info":{"role":"assistant","time":{"completed":null}},"parts":[{"type":"text","text":"partial"}]}]`)
			} else {
				fmt.Fprint(w, `[{"info":{"role":"user","time":{}},"parts":[{"type":"text","text":"q"}]},
					{"info":{"role":"assistant","time":{"completed":1721160000000}},"parts":[{"type":"step-start","text":""},{"type":"text","text":"  the extracted rules  "}]}]`)
			}
		case r.Method == http.MethodDelete && r.URL.Path == "/session/ses_batch1":
			state.deleted.Store(true)
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, state
}

func TestRunBatch(t *testing.T) {
	old := batchPollInterval
	batchPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { batchPollInterval = old })

	ts, state := batchServer(t)
	injectPooledServer(t, "batch-agent", ts)

	b := &Backend{}
	got, err := b.RunBatch(context.Background(), delegator.BatchRequest{
		Prompt:       "extract the rules",
		SystemPrompt: "CHARACTER FILES HERE",
		Model:        "zai-coding-plan/glm-5.2",
		AgentID:      "batch-agent",
	})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if got != "the extracted rules" {
		t.Errorf("result = %q", got)
	}
	if state.polls.Load() < 2 {
		t.Errorf("expected >=2 polls (incomplete then complete), got %d", state.polls.Load())
	}

	body, _ := state.promptBody.Load().(string)
	for _, want := range []string{
		`"text":"extract the rules"`,
		`"system":"CHARACTER FILES HERE"`,
		`"providerID":"zai-coding-plan"`,
		`"modelID":"glm-5.2"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prompt body missing %s:\n%s", want, body)
		}
	}

	// The ephemeral session must be reclaimed (poll deletion async-safely).
	deadline := time.Now().Add(2 * time.Second)
	for !state.deleted.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !state.deleted.Load() {
		t.Error("ephemeral session was not deleted")
	}
}

func TestRunBatch_DefaultsAndErrors(t *testing.T) {
	old := batchPollInterval
	batchPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { batchPollInterval = old })

	ts, state := batchServer(t)
	injectPooledServer(t, "batch-agent-2", ts)
	b := &Backend{}

	// Empty Model/SystemPrompt → omitted from the request body.
	if _, err := b.RunBatch(context.Background(), delegator.BatchRequest{
		Prompt:  "p",
		AgentID: "batch-agent-2",
	}); err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	body, _ := state.promptBody.Load().(string)
	if strings.Contains(body, `"model"`) || strings.Contains(body, `"system"`) {
		t.Errorf("defaults must omit model/system: %s", body)
	}

	// No pooled server → RunBatch now spawns one (see
	// TestRunBatch_SpawnsAndPersistsWhenNoServerPooled) rather than erroring.
	// A spawn failure still surfaces as a clear wrapped error — stub
	// acquireServerFn to fail deterministically instead of depending on
	// whether a real "opencode" binary happens to be on the test host's
	// $PATH. Restored immediately (not via t.Cleanup) so it doesn't leak
	// into the "bad model format" case below, which needs the real
	// pooled-reuse path against batch-agent-2.
	orig := acquireServerFn
	acquireServerFn = func(string, serverConfig, map[string]string) (*Server, error) {
		return nil, fmt.Errorf("boom")
	}
	_, err := (&Backend{}).RunBatch(context.Background(), delegator.BatchRequest{
		Prompt:  "p",
		AgentID: "no-such-agent",
	})
	acquireServerFn = orig
	if err == nil || !strings.Contains(err.Error(), "acquire server") {
		t.Errorf("expected acquire-server error, got %v", err)
	}

	// Bad model format → rejected before any prompt.
	if _, err := b.RunBatch(context.Background(), delegator.BatchRequest{
		Prompt:  "p",
		Model:   "not-slash-separated",
		AgentID: "batch-agent-2",
	}); err == nil || !strings.Contains(err.Error(), "providerID/modelID") {
		t.Errorf("expected model format error, got %v", err)
	}
}

// TestRunBatch_SpawnsThenReleasesWhenSoleHolder is the core regression test for
// the batch server lifecycle: with no live interactive session (nothing
// pooled), RunBatch spawns the shared server, builds its config from the SAME
// per-agent b.cfg an interactive Backend would use (not a divergent batch-only
// config), runs the one-shot, and RELEASES on completion. Because the batch was
// the sole holder, that release drops refCount to 0 and the server is reaped —
// it is NOT pinned open forever (the refcount-leak bug, corrected 2026-07-20).
//
// Stubs acquireServerFn (no real "opencode serve") but mimics a real spawn by
// pooling the fake Server with refCount=1, so the defer's real releaseServer
// decrements a truthful count.
func TestRunBatch_SpawnsThenReleasesWhenSoleHolder(t *testing.T) {
	old := batchPollInterval
	batchPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { batchPollInterval = old })

	const agentID = "batch-spawn-agent"
	resetTestPool(t)

	ts, _ := batchServer(t)

	var (
		gotAgentID string
		gotCfg     serverConfig
		calls      int
	)
	orig := acquireServerFn
	acquireServerFn = func(agentID string, cfg serverConfig, env map[string]string) (*Server, error) {
		calls++
		gotAgentID = agentID
		gotCfg = cfg
		if env != nil {
			t.Errorf("acquireServerFn env = %v, want nil (batch has no exec-bridge env)", env)
		}
		// Mimic a real acquireServer spawn: pool a live Server with refCount=1
		// so the defer's real releaseServer sees a truthful refcount.
		srv := &Server{agentID: agentID, baseURL: ts.URL, http: ts.Client(), running: true, refCount: 1}
		serverPoolMu.Lock()
		serverPool[agentID] = srv
		serverPoolMu.Unlock()
		return srv, nil
	}
	t.Cleanup(func() { acquireServerFn = orig })

	b := &Backend{cfg: map[string]any{"binary": "custom-opencode", "hostname": "10.0.0.9"}}
	got, err := b.RunBatch(context.Background(), delegator.BatchRequest{
		Prompt:  "extract",
		AgentID: agentID,
		WorkDir: "/tmp/agent-workdir-for-batch",
	})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if got != "the extracted rules" {
		t.Errorf("result = %q", got)
	}
	if calls != 1 {
		t.Fatalf("acquireServerFn called %d times, want 1 (no pooled server, one spawn)", calls)
	}
	if gotAgentID != agentID {
		t.Errorf("acquireServerFn agentID = %q, want %q", gotAgentID, agentID)
	}
	// Config correctness: workDir comes from req.WorkDir (the agent workspace),
	// binary/hostname from b.cfg — the same per-agent backend_config an
	// interactive Start would resolve, so the batch-spawned server IS the server
	// an interactive session would attach to.
	if gotCfg.workDir != "/tmp/agent-workdir-for-batch" {
		t.Errorf("cfg.workDir = %q, want /tmp/agent-workdir-for-batch", gotCfg.workDir)
	}
	if gotCfg.binaryPath != "custom-opencode" {
		t.Errorf("cfg.binaryPath = %q, want custom-opencode (sourced from b.cfg)", gotCfg.binaryPath)
	}
	if gotCfg.hostname != "10.0.0.9" {
		t.Errorf("cfg.hostname = %q, want 10.0.0.9 (sourced from b.cfg)", gotCfg.hostname)
	}

	// Sole-holder release: refCount hit 0, so the server must be evicted from
	// the pool — NOT left pinned open. This is the whole point of the fix.
	if _, ok := lookupTestPool(agentID); ok {
		t.Error("server still pooled after RunBatch — a sole-holder batch must release and reap it, not pin it open")
	}
}

// TestRunBatch_KeepsServerWhenOtherHolderRemains is the counterpart: if an
// interactive session is also attached (refCount 2), the batch's release just
// decrements to 1 and the shared server stays pooled for that session. Proves
// the release is refcount-correct, not an unconditional teardown.
func TestRunBatch_KeepsServerWhenOtherHolderRemains(t *testing.T) {
	old := batchPollInterval
	batchPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { batchPollInterval = old })

	const agentID = "batch-coexist-agent"
	resetTestPool(t)

	ts, _ := batchServer(t)

	// An interactive holder already owns the pooled server (refCount=1).
	live := &Server{agentID: agentID, baseURL: ts.URL, http: ts.Client(), running: true, refCount: 1}
	serverPoolMu.Lock()
	serverPool[agentID] = live
	serverPoolMu.Unlock()

	orig := acquireServerFn
	acquireServerFn = func(agentID string, cfg serverConfig, env map[string]string) (*Server, error) {
		live.refCount++ // reuse path: bump the existing holder, like real acquireServer
		return live, nil
	}
	t.Cleanup(func() { acquireServerFn = orig })

	b := &Backend{cfg: map[string]any{}}
	if _, err := b.RunBatch(context.Background(), delegator.BatchRequest{
		Prompt: "extract", AgentID: agentID, WorkDir: "/tmp/wd",
	}); err != nil {
		t.Fatalf("RunBatch: %v", err)
	}

	cur, ok := lookupTestPool(agentID)
	if !ok || cur != live {
		t.Fatal("server evicted though an interactive holder still needed it")
	}
	if live.refCount != 1 {
		t.Errorf("refCount = %d, want 1 (batch released its +1; interactive holder remains)", live.refCount)
	}
}
