package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestIsFallbackEligible_DeadlineExceeded(t *testing.T) {
	// Proves that context.DeadlineExceeded triggers fallback, since it
	// indicates the primary model timed out.
	if !IsFallbackEligible(context.DeadlineExceeded) {
		t.Error("expected DeadlineExceeded to be fallback-eligible")
	}
}

func TestIsFallbackEligible_WrappedDeadlineExceeded(t *testing.T) {
	// Proves that a wrapped DeadlineExceeded is still detected via errors.Is.
	err := fmt.Errorf("request failed: %w", context.DeadlineExceeded)
	if !IsFallbackEligible(err) {
		t.Error("expected wrapped DeadlineExceeded to be fallback-eligible")
	}
}

func TestIsFallbackEligible_Overloaded529(t *testing.T) {
	// Proves that 529 (Anthropic overloaded) triggers fallback.
	err := &APIError{StatusCode: 529}
	if !IsFallbackEligible(err) {
		t.Error("expected 529 to be fallback-eligible")
	}
}

func TestIsFallbackEligible_ServerErrors(t *testing.T) {
	// Proves that 5xx server errors (500, 502, 503) trigger fallback.
	for _, code := range []int{500, 502, 503} {
		err := &APIError{StatusCode: code}
		if !IsFallbackEligible(err) {
			t.Errorf("expected %d to be fallback-eligible", code)
		}
	}
}

func TestIsFallbackEligible_NotEligible(t *testing.T) {
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
}

type fallbackMockResponse struct {
	resp *MessageResponse
	err  error
}

func (m *fallbackMockClient) SendMessage(_ context.Context, req *MessageRequest) (*MessageResponse, error) {
	if m.callIdx >= len(m.responses) {
		panic("fallbackMockClient: no more responses")
	}
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
	// Proves that fallback walks the chain: primary → fb1 → fb2 succeeds.
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},                         // primary fails
		{err: &APIError{StatusCode: 503}},                         // fb1 fails
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
