package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestIsFallbackEligible_DeadlineExceeded(t *testing.T) {
	t.Parallel()
	// Proves that context.DeadlineExceeded triggers fallback, since it
	// indicates the primary model timed out.
	if !IsFallbackEligible(context.DeadlineExceeded) {
		t.Error("expected DeadlineExceeded to be fallback-eligible")
	}
}

func TestIsFallbackEligible_WrappedDeadlineExceeded(t *testing.T) {
	t.Parallel()
	// Proves that a wrapped DeadlineExceeded is still detected via errors.Is.
	err := fmt.Errorf("request failed: %w", context.DeadlineExceeded)
	if !IsFallbackEligible(err) {
		t.Error("expected wrapped DeadlineExceeded to be fallback-eligible")
	}
}

func TestIsFallbackEligible_Overloaded529(t *testing.T) {
	t.Parallel()
	// Proves that 529 (Anthropic overloaded) triggers fallback.
	err := &APIError{StatusCode: 529}
	if !IsFallbackEligible(err) {
		t.Error("expected 529 to be fallback-eligible")
	}
}

func TestIsFallbackEligible_ServerErrors(t *testing.T) {
	t.Parallel()
	// Proves that 5xx server errors (500, 502, 503) trigger fallback.
	for _, code := range []int{500, 502, 503} {
		err := &APIError{StatusCode: code}
		if !IsFallbackEligible(err) {
			t.Errorf("expected %d to be fallback-eligible", code)
		}
	}
}

func TestIsFallbackEligible_NotEligible(t *testing.T) {
	t.Parallel()
	// Proves that client errors (400, 401, 429) and non-API errors
	// do NOT trigger fallback.
	cases := []struct {
		name string
		err  error
	}{
		{"400 bad request", &APIError{StatusCode: http.StatusBadRequest}},
		{"401 unauthorized", &APIError{StatusCode: http.StatusUnauthorized}},
		{"429 rate limit", &APIError{StatusCode: http.StatusTooManyRequests}},
		{"generic error", fmt.Errorf("connection refused")},
		{"context cancelled", context.Canceled},
	}
	for _, tc := range cases {
		if IsFallbackEligible(tc.err) {
			t.Errorf("%s: expected NOT fallback-eligible", tc.name)
		}
	}
}

// fallbackMockClient is a minimal Client for testing Send's fallback behaviour.
type fallbackMockClient struct {
	responses []fallbackMockResponse // consumed in order; panics if exhausted
	callIdx   int
	calls     []capturedRequest // what each call carried, snapshotted at call time
}

type fallbackMockResponse struct {
	resp *MessageResponse
	err  error
}

// capturedRequest snapshots what a request carried AT CALL TIME. Send and
// walkFallback reuse and mutate the same *MessageRequest across the whole
// chain (req.Model, req.ProviderRouting), so recording the pointer would
// alias every entry to the last hop's state.
type capturedRequest struct {
	model   string
	routing *ProviderRouting
}

func (m *fallbackMockClient) SendMessage(_ context.Context, req *MessageRequest) (*MessageResponse, error) {
	if m.callIdx >= len(m.responses) {
		panic("fallbackMockClient: no more responses")
	}
	m.calls = append(m.calls, capturedRequest{model: req.Model, routing: req.ProviderRouting})
	r := m.responses[m.callIdx]
	m.callIdx++
	if r.resp != nil && r.resp.Model == "" {
		r.resp.Model = req.Model
	}
	return r.resp, r.err
}

func (m *fallbackMockClient) CountTokens(_ context.Context, _ *MessageRequest) (int, error) {
	return 0, nil
}

func (m *fallbackMockClient) IsCachingAvailable() bool { return false }

// HandlesOwnRetries makes sendWithRetry skip its retry loop, so tests control
// exactly how many times SendMessage is called.
func (m *fallbackMockClient) HandlesOwnRetries() bool { return true }

// fallbackMockClientProvider returns distinct mock clients keyed by endpoint:format.
type fallbackMockClientProvider struct {
	clients map[string]Client
}

func (p *fallbackMockClientProvider) GetClient(endpoint, format string) Client {
	return p.clients[endpoint+":"+format]
}

func (p *fallbackMockClientProvider) PeekClient(endpoint, format string) Client {
	return p.clients[endpoint+":"+format]
}

func (p *fallbackMockClientProvider) ResolveEndpointClient(endpoint, format string) Client {
	return p.clients[endpoint+":"+format]
}

func TestSend_NilFallbackFn(t *testing.T) {
	t.Parallel()
	// Proves that a nil fallbackFn degrades to plain send-with-retry.
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{resp: &MessageResponse{Content: TextContent("ok")}},
	}}
	req := &MessageRequest{Model: "primary"}
	resp, err := Send(context.Background(), mc, req, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if TextOf(resp.Content) != "ok" {
		t.Errorf("expected 'ok', got %q", TextOf(resp.Content))
	}
}

func TestSend_PrimarySucceeds(t *testing.T) {
	t.Parallel()
	// Proves that when the primary call succeeds, fallback is never tried.
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{resp: &MessageResponse{Content: TextContent("primary ok")}},
	}}
	called := false
	fallbackFn := func(model string) (string, string, string, bool) {
		called = true
		return "fb-model", "ep", "fmt", true
	}
	req := &MessageRequest{Model: "primary"}
	resp, err := Send(context.Background(), mc, req, nil, fallbackFn, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if TextOf(resp.Content) != "primary ok" {
		t.Errorf("expected 'primary ok', got %q", TextOf(resp.Content))
	}
	if called {
		t.Error("fallbackFn should not be called when primary succeeds")
	}
}

func TestSend_FallbackOnTransientError(t *testing.T) {
	t.Parallel()
	// Proves that a 529 on primary triggers the fallback, and the
	// fallback model's client is resolved via clientProvider.
	primaryClient := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},
	}}
	fbClient := &fallbackMockClient{responses: []fallbackMockResponse{
		{resp: &MessageResponse{Content: TextContent("fb ok")}},
	}}
	cp := &fallbackMockClientProvider{clients: map[string]Client{
		"fb-ep:fb-fmt": fbClient,
	}}
	fallbackFn := func(model string) (string, string, string, bool) {
		if model == "primary-model" {
			return "fb-model", "fb-ep", "fb-fmt", true
		}
		return "", "", "", false
	}
	var logs []string
	logf := func(f string, args ...any) { logs = append(logs, fmt.Sprintf(f, args...)) }

	req := &MessageRequest{Model: "primary-model"}
	resp, err := Send(context.Background(), primaryClient, req, nil, fallbackFn, cp, logf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if TextOf(resp.Content) != "fb ok" {
		t.Errorf("expected 'fb ok', got %q", TextOf(resp.Content))
	}
	if req.Model != "fb-model" {
		t.Errorf("expected req.Model to be 'fb-model', got %q", req.Model)
	}
	if len(logs) != 2 {
		t.Errorf("expected 2 log messages, got %d: %v", len(logs), logs)
	}
}

func TestSend_ChainWalk(t *testing.T) {
	t.Parallel()
	// Proves that fallback walks the chain: primary → fb1 → fb2 succeeds.
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},                        // primary fails
		{err: &APIError{StatusCode: 503}},                        // fb1 fails
		{resp: &MessageResponse{Content: TextContent("fb2 ok")}}, // fb2 succeeds
	}}
	fallbackFn := func(model string) (string, string, string, bool) {
		switch model {
		case "primary":
			return "fb1", "", "", true
		case "fb1":
			return "fb2", "", "", true
		default:
			return "", "", "", false
		}
	}
	req := &MessageRequest{Model: "primary"}
	resp, err := Send(context.Background(), mc, req, nil, fallbackFn, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if TextOf(resp.Content) != "fb2 ok" {
		t.Errorf("expected 'fb2 ok', got %q", TextOf(resp.Content))
	}
}

func TestSend_AllFail(t *testing.T) {
	t.Parallel()
	// Proves that when all models in the chain fail, the last error is returned.
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},
		{err: &APIError{StatusCode: 529}},
	}}
	fallbackFn := func(model string) (string, string, string, bool) {
		if model == "primary" {
			return "fb1", "", "", true
		}
		return "", "", "", false
	}
	req := &MessageRequest{Model: "primary"}
	_, err := Send(context.Background(), mc, req, nil, fallbackFn, nil, nil)
	if err == nil {
		t.Fatal("expected error when all fallbacks fail")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.IsRetryable() {
		t.Error("expected retryable API error")
	}
}

func TestSend_NonTransientSkipsFallback(t *testing.T) {
	t.Parallel()
	// Proves that a non-transient error (401) does NOT trigger fallback.
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 401}},
	}}
	called := false
	fallbackFn := func(model string) (string, string, string, bool) {
		called = true
		return "fb", "", "", true
	}
	req := &MessageRequest{Model: "primary"}
	_, err := Send(context.Background(), mc, req, nil, fallbackFn, nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if called {
		t.Error("fallbackFn should not be called for non-transient errors")
	}
}

func TestSend_StripUnsupportedParams(t *testing.T) {
	t.Parallel()
	// Proves that a 400 mentioning "thinking" strips the param and retries
	// successfully on the same model.
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 400, Body: `{"error":"thinking is not supported"}`}},
		{resp: &MessageResponse{Content: TextContent("ok after strip")}},
	}}
	req := &MessageRequest{
		Model:    "primary",
		Thinking: &ThinkingConfig{Type: "enabled", BudgetTokens: 1024},
	}
	resp, err := Send(context.Background(), mc, req, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if TextOf(resp.Content) != "ok after strip" {
		t.Errorf("expected 'ok after strip', got %q", TextOf(resp.Content))
	}
	if req.Thinking != nil {
		t.Error("expected Thinking to be stripped")
	}
}

func TestSend_NoFallbackConfigured(t *testing.T) {
	t.Parallel()
	// Proves that when a transient error triggers fallback but no fallback model
	// is configured, the original error is returned (not nil). This was a crash:
	// walkFallback returned (nil, nil) when fallbackFn returned ok=false on
	// the first iteration, causing a nil response dereference.
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: context.DeadlineExceeded},
	}}
	fallbackFn := func(model string) (string, string, string, bool) {
		return "", "", "", false // no fallback for any model
	}
	req := &MessageRequest{Model: "primary"}
	_, err := Send(context.Background(), mc, req, nil, fallbackFn, nil, nil)
	if err == nil {
		t.Fatal("expected error when no fallback is configured, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}

func TestSend_StripThenFallback(t *testing.T) {
	t.Parallel()
	// Proves that strip-and-retry happens before fallback: a 400 strips params,
	// retry still fails (529), then fallback kicks in.
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 400, Body: `{"error":"effort not supported"}`}},
		{err: &APIError{StatusCode: 529}}, // strip retry → transient
	}}
	fbClient := &fallbackMockClient{responses: []fallbackMockResponse{
		{resp: &MessageResponse{Content: TextContent("fb ok")}},
	}}
	cp := &fallbackMockClientProvider{clients: map[string]Client{
		"fb-ep:fb-fmt": fbClient,
	}}
	fallbackFn := func(model string) (string, string, string, bool) {
		if model == "primary" {
			return "fb-model", "fb-ep", "fb-fmt", true
		}
		return "", "", "", false
	}
	req := &MessageRequest{
		Model:  "primary",
		Output: &OutputConfig{Effort: "high"},
	}
	resp, err := Send(context.Background(), mc, req, nil, fallbackFn, cp, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if TextOf(resp.Content) != "fb ok" {
		t.Errorf("expected 'fb ok', got %q", TextOf(resp.Content))
	}
	if req.Output != nil {
		t.Error("expected Output to be stripped")
	}
}

func TestSend_FallbackUsesFallbackModelRouting(t *testing.T) {
	t.Parallel()
	// Proves that a fallback hop runs under the FALLBACK model's own
	// [models.*.provider] routing, not the primary's: the primary's
	// order/allow_fallbacks may name providers that don't serve the
	// fallback model at all.
	primaryClient := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},
	}}
	fbClient := &fallbackMockClient{responses: []fallbackMockResponse{
		{resp: &MessageResponse{Content: TextContent("fb ok")}},
	}}
	cp := &fallbackMockClientProvider{clients: map[string]Client{
		"fb-ep:fb-fmt": fbClient,
	}}
	fallbackFn := func(model string) (string, string, string, bool) {
		if model == "primary-model" {
			return "fb-model", "fb-ep", "fb-fmt", true
		}
		return "", "", "", false
	}
	routingA := &ProviderRouting{Order: []string{"prov-a"}}
	routingB := &ProviderRouting{Order: []string{"prov-b"}}
	req := &MessageRequest{
		Model:           "primary-model",
		ProviderRouting: routingA,
		RoutingFor: func(model string) *ProviderRouting {
			if model == "fb-model" {
				return routingB
			}
			return nil
		},
	}
	if _, err := Send(context.Background(), primaryClient, req, nil, fallbackFn, cp, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(primaryClient.calls) != 1 || primaryClient.calls[0].routing != routingA {
		t.Errorf("primary snapshot = %+v, want routing A", primaryClient.calls)
	}
	if len(fbClient.calls) != 1 {
		t.Fatalf("fallback client calls = %d, want 1", len(fbClient.calls))
	}
	got := fbClient.calls[0]
	if got.model != "fb-model" {
		t.Errorf("fallback snapshot model = %q, want fb-model", got.model)
	}
	if got.routing != routingB {
		t.Errorf("fallback snapshot routing = %+v, want routing B", got.routing)
	}
}

func TestSend_FallbackModelWithoutRoutingSendsNil(t *testing.T) {
	t.Parallel()
	// Proves that a fallback model with no [models.*.provider] table sends
	// no routing at all — the primary's routing must not ride along.
	primaryClient := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},
	}}
	fbClient := &fallbackMockClient{responses: []fallbackMockResponse{
		{resp: &MessageResponse{Content: TextContent("fb ok")}},
	}}
	cp := &fallbackMockClientProvider{clients: map[string]Client{
		"fb-ep:fb-fmt": fbClient,
	}}
	fallbackFn := func(model string) (string, string, string, bool) {
		if model == "primary-model" {
			return "fb-model", "fb-ep", "fb-fmt", true
		}
		return "", "", "", false
	}
	req := &MessageRequest{
		Model:           "primary-model",
		ProviderRouting: &ProviderRouting{Order: []string{"prov-a"}},
		RoutingFor: func(model string) *ProviderRouting {
			return nil // fallback model has no routing configured
		},
	}
	if _, err := Send(context.Background(), primaryClient, req, nil, fallbackFn, cp, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fbClient.calls) != 1 {
		t.Fatalf("fallback client calls = %d, want 1", len(fbClient.calls))
	}
	if fbClient.calls[0].routing != nil {
		t.Errorf("fallback snapshot routing = %+v, want nil", fbClient.calls[0].routing)
	}
}

func TestSend_FallbackNilRoutingForSendsNil(t *testing.T) {
	t.Parallel()
	// Proves that a request without a routing lookup (no [models.*]
	// configured, or a caller that never set one) sends no routing on a
	// fallback hop — today's behaviour for unconfigured models.
	primaryClient := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},
	}}
	fbClient := &fallbackMockClient{responses: []fallbackMockResponse{
		{resp: &MessageResponse{Content: TextContent("fb ok")}},
	}}
	cp := &fallbackMockClientProvider{clients: map[string]Client{
		"fb-ep:fb-fmt": fbClient,
	}}
	fallbackFn := func(model string) (string, string, string, bool) {
		if model == "primary-model" {
			return "fb-model", "fb-ep", "fb-fmt", true
		}
		return "", "", "", false
	}
	req := &MessageRequest{
		Model:           "primary-model",
		ProviderRouting: &ProviderRouting{Order: []string{"prov-a"}},
	}
	if _, err := Send(context.Background(), primaryClient, req, nil, fallbackFn, cp, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fbClient.calls) != 1 {
		t.Fatalf("fallback client calls = %d, want 1", len(fbClient.calls))
	}
	if fbClient.calls[0].routing != nil {
		t.Errorf("fallback snapshot routing = %+v, want nil", fbClient.calls[0].routing)
	}
}

func TestSend_ChainEachHopOwnRouting(t *testing.T) {
	t.Parallel()
	// Proves that every hop of a multi-hop chain resolves its OWN routing:
	// primary→fb1→fb2 with routing A/B/C sends A, then B, then C — the
	// primary's lock never rides onto a later model. Call-time snapshots
	// make this assertable at all: the same *MessageRequest is mutated per
	// hop, so a pointer record would show C three times.
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},                        // primary fails
		{err: &APIError{StatusCode: 503}},                        // fb1 fails
		{resp: &MessageResponse{Content: TextContent("fb2 ok")}}, // fb2 succeeds
	}}
	fallbackFn := func(model string) (string, string, string, bool) {
		switch model {
		case "primary":
			return "fb1", "", "", true
		case "fb1":
			return "fb2", "", "", true
		default:
			return "", "", "", false
		}
	}
	routingA := &ProviderRouting{Order: []string{"prov-a"}}
	routingB := &ProviderRouting{Order: []string{"prov-b"}}
	routingC := &ProviderRouting{Order: []string{"prov-c"}}
	req := &MessageRequest{
		Model:           "primary",
		ProviderRouting: routingA,
		RoutingFor: func(model string) *ProviderRouting {
			switch model {
			case "fb1":
				return routingB
			case "fb2":
				return routingC
			default:
				return nil
			}
		},
	}
	if _, err := Send(context.Background(), mc, req, nil, fallbackFn, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []capturedRequest{
		{model: "primary", routing: routingA},
		{model: "fb1", routing: routingB},
		{model: "fb2", routing: routingC},
	}
	if len(mc.calls) != len(want) {
		t.Fatalf("calls = %+v, want %+v", mc.calls, want)
	}
	for i, w := range want {
		if mc.calls[i] != w {
			t.Errorf("call %d = %+v, want %+v", i, mc.calls[i], w)
		}
	}
}

func TestSend_PrimarySuccessKeepsRouting(t *testing.T) {
	t.Parallel()
	// Proves the unchanged case: a successful primary request keeps its
	// own routing — walkFallback is never entered.
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{resp: &MessageResponse{Content: TextContent("primary ok")}},
	}}
	routingA := &ProviderRouting{Order: []string{"prov-a"}}
	req := &MessageRequest{
		Model:           "primary",
		ProviderRouting: routingA,
	}
	if _, err := Send(context.Background(), mc, req, nil, nil, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(mc.calls))
	}
	if mc.calls[0].model != "primary" || mc.calls[0].routing != routingA {
		t.Errorf("snapshot = %+v, want {primary routing A}", mc.calls[0])
	}
}

func TestSend_StripRetryKeepsRouting(t *testing.T) {
	t.Parallel()
	// Proves the unchanged case: the 400 strip-and-retry in Send stays on
	// the same model with its routing untouched — both attempts carry it.
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 400, Body: `{"error":"thinking is not supported"}`}},
		{resp: &MessageResponse{Content: TextContent("ok after strip")}},
	}}
	routingA := &ProviderRouting{Order: []string{"prov-a"}}
	req := &MessageRequest{
		Model:           "primary",
		Thinking:        &ThinkingConfig{Type: "enabled", BudgetTokens: 1024},
		ProviderRouting: routingA,
	}
	if _, err := Send(context.Background(), mc, req, nil, nil, nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(mc.calls))
	}
	for i, c := range mc.calls {
		if c.model != "primary" || c.routing != routingA {
			t.Errorf("call %d = %+v, want {primary routing A}", i, c)
		}
	}
}
