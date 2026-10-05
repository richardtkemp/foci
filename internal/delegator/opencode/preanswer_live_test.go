package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
)

// liveNudgeMarker is the text the gate's follow-up carries; the fake provider
// answers liveRevised to a request whose last message contains it.
const (
	liveNudgeMarker = "PREANSWER-NUDGE-2176"
	liveFirst       = "FIRST-ANSWER"
	liveRevised     = "REVISED-ANSWER"
)

// TestLive_PreAnswerRedispatch drives the pre-answer round trip through a
// REAL `opencode serve` (#2176): Backend.Start spawns it, a user prompt runs
// to session.idle, the gate's follow-up is re-dispatched with prompt_async,
// and the turn must complete on the second idle with the revised answer.
// The model is a local fake OpenAI-compatible provider, so it costs nothing
// and the answers are deterministic.
//
// Opt-in, because it spawns opencode (seconds, and opencode may fetch
// packages on a cold HOME):
//
//	FOCI_OPENCODE_LIVE=1 make test-one PKG=./internal/delegator/opencode/ RUN=TestLive_PreAnswerRedispatch V=1
func TestLive_PreAnswerRedispatch(t *testing.T) {
	if os.Getenv("FOCI_OPENCODE_LIVE") == "" {
		t.Skip("set FOCI_OPENCODE_LIVE=1 to run against a real opencode binary")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("opencode not on PATH: %v", err)
	}

	provider := newFakeChatProvider(t)
	workDir := t.TempDir()
	cfg := fmt.Sprintf(`{"$schema":"https://opencode.ai/config.json",
 "model":"fake/fake-model","small_model":"fake/fake-model",
 "provider":{"fake":{"npm":"@ai-sdk/openai-compatible","name":"Fake",
  "options":{"baseURL":%q,"apiKey":"x"},"models":{"fake-model":{"name":"Fake"}}}}}`, provider.URL+"/v1")
	if err := os.WriteFile(filepath.Join(workDir, "opencode.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	d, err := newFromConfig(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	b := d.(*Backend)
	agentID := fmt.Sprintf("live-2176-%d", time.Now().UnixNano())
	if err := b.Start(ctx, delegator.StartOptions{AgentID: agentID, WorkDir: workDir, SystemPrompt: "You are a test fixture."}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	b.AttachSessionEvents(&delegator.SessionEvents{})

	var mu sync.Mutex
	var gateTexts []string
	done := make(chan *delegator.TurnResult, 1)
	turn := &delegator.TurnEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { done <- r },
		PreAnswerNudgeFunc: func(r *delegator.TurnResult) string {
			mu.Lock()
			defer mu.Unlock()
			gateTexts = append(gateTexts, r.Text)
			if len(gateTexts) == 1 {
				return liveNudgeMarker + ": verify your answer"
			}
			return ""
		},
	}
	if err := b.ImmediateInject(ctx, delegator.Inject{Source: delegator.SourceUser, Text: "hello", Turn: turn}); err != nil {
		t.Fatalf("inject: %v", err)
	}

	var result *delegator.TurnResult
	select {
	case result = <-done:
	case <-ctx.Done():
		t.Fatalf("turn never completed (provider saw %d request(s)); the re-dispatch wedged", provider.count())
	}

	mu.Lock()
	gates := append([]string(nil), gateTexts...)
	mu.Unlock()
	if len(gates) != 2 {
		t.Fatalf("gate called %d times (%q), want 2: once per round", len(gates), gates)
	}
	if !strings.Contains(gates[0], liveFirst) {
		t.Errorf("round-1 result seen by the gate = %q, want it to carry %s", gates[0], liveFirst)
	}
	if !provider.sawMarker() {
		t.Error("the follow-up never reached the model")
	}
	if !strings.Contains(result.Text, liveRevised) || strings.Contains(result.Text, liveFirst) {
		t.Errorf("final text = %q, want the revised answer only", result.Text)
	}
	if b.IsTurnInFlight() {
		t.Error("turn still in flight after completion")
	}
}

type fakeChatProvider struct {
	*httptest.Server
	mu     sync.Mutex
	n      int
	marker bool
}

// newFakeChatProvider serves OpenAI-compatible /v1/chat/completions, streamed
// or not, answering liveRevised when the last message carries liveNudgeMarker
// and liveFirst otherwise.
func newFakeChatProvider(t *testing.T) *fakeChatProvider {
	p := &fakeChatProvider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Stream   bool              `json:"stream"`
			Messages []json.RawMessage `json:"messages"`
		}
		_ = json.Unmarshal(raw, &req)
		text := liveFirst
		if n := len(req.Messages); n > 0 && strings.Contains(string(req.Messages[n-1]), liveNudgeMarker) {
			text = liveRevised
		}
		p.mu.Lock()
		p.n++
		if text == liveRevised {
			p.marker = true
		}
		p.mu.Unlock()

		usage := map[string]int{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "c1", "object": "chat.completion", "created": 1, "model": "fake-model",
				"choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": text}, "finish_reason": "stop"}},
				"usage":   usage,
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		emit := func(v any) {
			bs, _ := json.Marshal(v)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", bs)
			if flusher != nil {
				flusher.Flush()
			}
		}
		chunk := func(delta map[string]string, finish any) map[string]any {
			return map[string]any{"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "fake-model",
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		}
		emit(chunk(map[string]string{"role": "assistant", "content": text}, nil))
		last := chunk(map[string]string{}, "stop")
		last["usage"] = usage
		emit(last)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *fakeChatProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

func (p *fakeChatProvider) sawMarker() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.marker
}
