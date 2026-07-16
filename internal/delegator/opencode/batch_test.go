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

	// No pooled server → clear error, no fallback here.
	if _, err := (&Backend{}).RunBatch(context.Background(), delegator.BatchRequest{
		Prompt:  "p",
		AgentID: "no-such-agent",
	}); err == nil || !strings.Contains(err.Error(), "no running server") {
		t.Errorf("expected no-server error, got %v", err)
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
