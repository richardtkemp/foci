package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/provider"
)

// wireBodyServer is an httptest server that records the JSON body of every
// request it receives. block=true makes the handler hang until the request
// context ends, so a client with a per-call timeout gets a
// context.DeadlineExceeded — a fallback-eligible error that retries never
// (the SDK returns the raw ctx error, which classifySDKError passes through
// unwrapped) and sleeps never (retryWithBackoff returns immediately on a
// non-APIError). That keeps the fallback wire tests fast: no real backoff.
type fbWireServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newBlockingFBServer(t *testing.T) *fbWireServer {
	t.Helper()
	w := &fbWireServer{}
	w.srv = httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		w.record(r)
		<-r.Context().Done()
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func newAnsweringFBServer(t *testing.T) *fbWireServer {
	t.Helper()
	w := &fbWireServer{}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.record(r)
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{
			"id": "msg_fb", "type": "message", "role": "assistant",
			"model": "claude-sonnet-4-6", "stop_reason": "end_turn",
			"content": [{"type": "text", "text": "fb ok"}],
			"usage": {"input_tokens": 1, "output_tokens": 1}
		}`))
	}))
	t.Cleanup(w.srv.Close)
	return w
}

// record reads and stores the request body, returning it.
func (w *fbWireServer) record(r *http.Request) map[string]any {
	var body map[string]any
	b, err := io.ReadAll(r.Body)
	if err == nil && len(b) > 0 {
		_ = json.Unmarshal(b, &body)
	}
	w.mu.Lock()
	w.bodies = append(w.bodies, body)
	w.mu.Unlock()
	return body
}

func (w *fbWireServer) bodyCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.bodies)
}

func (w *fbWireServer) body(i int) map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bodies[i]
}

// fbWireClientProvider resolves the fallback endpoint to its client.
type fbWireClientProvider struct {
	fbClient provider.Client
}

func (p *fbWireClientProvider) GetClient(endpoint, _ string) provider.Client {
	if endpoint == "anthropic" {
		return p.fbClient
	}
	return nil
}

func (p *fbWireClientProvider) PeekClient(endpoint, format string) provider.Client {
	return p.GetClient(endpoint, format)
}

func (p *fbWireClientProvider) ResolveEndpointClient(endpoint, format string) provider.Client {
	return p.GetClient(endpoint, format)
}

// TestSendFallbackHop_WireCacheTTL proves at the wire level (real anthropic
// client, real config.FallbackResolver) that a fallback hop runs under the
// FALLBACK model's own cache settings: the hop body's top-level
// cache_control.ttl is what CacheFor returns for the fallback model ("1h"),
// not the primary's "5m".
func TestSendFallbackHop_WireCacheTTL(t *testing.T) {
	t.Parallel()
	primarySrv := newBlockingFBServer(t)
	fbSrv := newAnsweringFBServer(t)
	primary := NewClient(func() (string, error) { return "test-key", nil }, 200*time.Millisecond)
	primary.SetBaseURL(primarySrv.srv.URL)
	fb := NewClient(func() (string, error) { return "test-key", nil }, 5*time.Second)
	fb.SetBaseURL(fbSrv.srv.URL)

	// opus → sonnet through the same resolver shape the gateway builds
	// (config.NewFallbackResolver → provider.FallbackFunc).
	fr := config.NewFallbackResolver(map[string]string{
		"anthropic/claude-opus-4-6": "anthropic/claude-sonnet-4-6",
	}, nil, nil)
	fallbackFn := func(model string) (string, string, string, bool) {
		rm := fr.Resolve(model)
		if rm == nil {
			return "", "", "", false
		}
		return rm.Developer + "/" + rm.ModelID, rm.Endpoint, rm.Format, true
	}

	req := &MessageRequest{
		Model:         "anthropic/claude-opus-4-6",
		MaxTokens:     64,
		CacheStrategy: "auto",
		CacheTTL:      "5m",
		CacheFor: func(model string) (string, string) {
			if model == "anthropic/claude-sonnet-4-6" {
				return "auto", "1h"
			}
			return "auto", "5m"
		},
		Messages: []provider.Message{{Role: "user", Content: provider.TextContent("hi")}},
	}

	if _, err := provider.Send(context.Background(), primary, req, nil, fallbackFn,
		&fbWireClientProvider{fbClient: fb}, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if fbSrv.bodyCount() != 1 {
		t.Fatalf("fallback server saw %d requests, want 1", fbSrv.bodyCount())
	}
	hop := fbSrv.body(0)
	cc, ok := hop["cache_control"].(map[string]any)
	if !ok {
		t.Fatalf("hop body cache_control = %v, want the auto-strategy top-level marker", hop["cache_control"])
	}
	if got := cc["ttl"]; got != "1h" {
		t.Errorf("hop body cache_control.ttl = %v, want 1h (the fallback model's own TTL, not the primary's 5m)", got)
	}
}
