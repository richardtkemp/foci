package openai

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
// (the SDK returns the raw ctx error, which classifyError passes through
// unwrapped) and sleeps never (retryWithBackoff returns immediately on a
// non-APIError). That keeps the fallback wire tests fast: no real backoff.
type wireBodyServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newBlockingBodyServer(t *testing.T) *wireBodyServer {
	t.Helper()
	w := &wireBodyServer{}
	w.srv = httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		w.record(r)
		<-r.Context().Done()
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func newAnsweringBodyServer(t *testing.T) *wireBodyServer {
	t.Helper()
	w := &wireBodyServer{}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body := w.record(r)
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"id":      "chatcmpl-fb",
			"object":  "chat.completion",
			"model":   body["model"],
			"created": 1700000000,
			"choices": []map[string]any{
				{
					"index":         0,
					"finish_reason": "stop",
					"message": map[string]any{
						"role":    "assistant",
						"content": "fb ok",
					},
				},
			},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	t.Cleanup(w.srv.Close)
	return w
}

// record reads and stores the request body, returning it.
func (w *wireBodyServer) record(r *http.Request) map[string]any {
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

func (w *wireBodyServer) bodyCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.bodies)
}

func (w *wireBodyServer) body(i int) map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bodies[i]
}

// wireClientProvider resolves the fallback endpoint to its client, standing
// in for the gateway's lazy client registry.
type wireClientProvider struct {
	fbEndpoint string
	fbClient   provider.Client
}

func (p *wireClientProvider) GetClient(endpoint, _ string) provider.Client {
	if endpoint == p.fbEndpoint {
		return p.fbClient
	}
	return nil
}

func (p *wireClientProvider) PeekClient(endpoint, format string) provider.Client {
	return p.GetClient(endpoint, format)
}

func (p *wireClientProvider) ResolveEndpointClient(endpoint, format string) provider.Client {
	return p.GetClient(endpoint, format)
}

// sendFallbackHop drives provider.Send with a blocking primary server
// (deadline-exceeded after ~200ms via the client's per-call timeout) and an
// immediately-answering fallback server, through a REAL config resolver —
// the same FallbackFunc shape the gateway builds from
// config.NewFallbackResolver (agents.go). Returns both servers so the test
// can assert on the recorded wire bodies.
func sendFallbackHop(t *testing.T, primaryModel string, req *provider.MessageRequest, fallbacks map[string]string) (*wireBodyServer, *wireBodyServer, error) {
	t.Helper()
	primarySrv := newBlockingBodyServer(t)
	fbSrv := newAnsweringBodyServer(t)
	primary := NewClient("test-key", WithBaseURL(primarySrv.srv.URL), WithHTTPTimeout(200*time.Millisecond))
	fb := NewClient("test-key", WithBaseURL(fbSrv.srv.URL), WithHTTPTimeout(5*time.Second))

	fr := config.NewFallbackResolver(fallbacks, nil, nil)
	if fr == nil {
		t.Fatal("fallback resolver is nil")
	}
	fallbackFn := func(model string) (string, string, string, bool) {
		rm := fr.Resolve(model)
		if rm == nil {
			return "", "", "", false
		}
		return rm.Developer + "/" + rm.ModelID, rm.Endpoint, rm.Format, true
	}

	req.Model = primaryModel
	_, err := provider.Send(context.Background(), primary, req, nil, fallbackFn,
		&wireClientProvider{fbEndpoint: "openrouter", fbClient: fb}, nil)
	return primarySrv, fbSrv, err
}

func TestSendFallbackHop_WireCasePreserved(t *testing.T) {
	// Proves at the wire level, with a real openai client and a real
	// config.FallbackResolver: a [fallbacks] key written in lowercase fires
	// for a primary spelled in mixed case (matching folds case), while the
	// model ids the provider receives keep the case written in config —
	// primary body "Qwen/Qwen3-X", hop body "DeepSeek/V4-Pro" — and the
	// hop still carries its own provider-routing table.
	t.Parallel()
	routingFb := &provider.ProviderRouting{Order: []string{"prov-fb"}}
	req := &provider.MessageRequest{
		MaxTokens: 64,
		Messages:  []provider.Message{{Role: "user", Content: provider.TextContent("hi")}},
		RoutingFor: config.RoutingFor(func(model string) config.ModelDefaults {
			if model == "openrouter/DeepSeek/V4-Pro" {
				return config.ModelDefaults{ProviderRouting: routingFb}
			}
			return config.ModelDefaults{}
		}),
	}

	primarySrv, fbSrv, err := sendFallbackHop(t, "openrouter/Qwen/Qwen3-X", req, map[string]string{
		"openrouter/qwen/qwen3-x": "openrouter/DeepSeek/V4-Pro",
	})
	if err != nil {
		t.Fatalf("Send: %v (fallback never fired for the mixed-case primary?)", err)
	}
	if got := primarySrv.body(0)["model"]; got != "Qwen/Qwen3-X" {
		t.Errorf("primary body model = %v, want the configured spelling %q", got, "Qwen/Qwen3-X")
	}
	if fbSrv.bodyCount() != 1 {
		t.Fatalf("fallback server saw %d requests, want 1", fbSrv.bodyCount())
	}
	hop := fbSrv.body(0)
	if got := hop["model"]; got != "DeepSeek/V4-Pro" {
		t.Errorf("hop body model = %v, want the fallback value's configured spelling %q", got, "DeepSeek/V4-Pro")
	}
	if got, ok := hop["provider"]; !ok || got == nil {
		t.Errorf("hop body provider = %v (present=%v), want the fallback model's routing table", got, ok)
	} else if prov, ok := got.(map[string]any); !ok || prov["order"] == nil {
		t.Errorf("hop body provider = %v, want an object with the configured order", got)
	}
}

func TestSendFallbackHop_WireProviderRouting(t *testing.T) {
	// Proves at the wire level (real openai client, real resolver, same-case
	// spellings): the fallback hop's request body carries the FALLBACK
	// model's "provider" object from its [models.*.provider] table — the
	// #2219 per-hop routing re-resolution, seen on the actual JSON.
	t.Parallel()
	routingFb := &provider.ProviderRouting{Order: []string{"prov-fb"}}
	req := &provider.MessageRequest{
		MaxTokens:       64,
		Messages:        []provider.Message{{Role: "user", Content: provider.TextContent("hi")}},
		ProviderRouting: &provider.ProviderRouting{Order: []string{"prov-primary"}},
		RoutingFor: config.RoutingFor(func(model string) config.ModelDefaults {
			if model == "openrouter/fb-model" {
				return config.ModelDefaults{ProviderRouting: routingFb}
			}
			return config.ModelDefaults{}
		}),
	}

	primarySrv, fbSrv, err := sendFallbackHop(t, "openrouter/primary-model", req, map[string]string{
		"openrouter/primary-model": "openrouter/fb-model",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got, ok := primarySrv.body(0)["provider"].(map[string]any); !ok {
		t.Errorf("primary body provider = %v, want the primary's table", primarySrv.body(0)["provider"])
	} else if order, _ := orderSlice(got["order"]); len(order) != 1 || order[0] != "prov-primary" {
		t.Errorf("primary body provider.order = %v, want [prov-primary]", got["order"])
	}
	if fbSrv.bodyCount() != 1 {
		t.Fatalf("fallback server saw %d requests, want 1", fbSrv.bodyCount())
	}
	if got, ok := fbSrv.body(0)["provider"].(map[string]any); !ok {
		t.Errorf("hop body provider = %v, want the fallback's table", fbSrv.body(0)["provider"])
	} else if order, _ := orderSlice(got["order"]); len(order) != 1 || order[0] != "prov-fb" {
		t.Errorf("hop body provider.order = %v, want [prov-fb] (the fallback model's own table)", got["order"])
	}
}

func TestSendFallbackHop_WireNoProviderKey(t *testing.T) {
	// Proves the unset case at the wire level: a fallback model with no
	// [models.*.provider] table sends NO "provider" key on the hop — the
	// primary's table must not ride along.
	t.Parallel()
	req := &provider.MessageRequest{
		MaxTokens:       64,
		Messages:        []provider.Message{{Role: "user", Content: provider.TextContent("hi")}},
		ProviderRouting: &provider.ProviderRouting{Order: []string{"prov-primary"}},
	}

	_, fbSrv, err := sendFallbackHop(t, "openrouter/primary-model", req, map[string]string{
		"openrouter/primary-model": "openrouter/fb-model",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if fbSrv.bodyCount() != 1 {
		t.Fatalf("fallback server saw %d requests, want 1", fbSrv.bodyCount())
	}
	if _, ok := fbSrv.body(0)["provider"]; ok {
		t.Errorf("hop body has a provider key (%v), want none", fbSrv.body(0)["provider"])
	}
}

// orderSlice normalises a decoded JSON array of strings.
func orderSlice(v any) ([]string, bool) {
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}
