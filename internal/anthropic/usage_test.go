package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// usageTestResponse is the server-side JSON shape for tests (matches the API contract).
type usageTestResponse struct {
	FiveHour   *usageAPIWindow `json:"five_hour,omitempty"`
	SevenDay   *usageAPIWindow `json:"seven_day,omitempty"`
	ExtraUsage *usageAPIExtra  `json:"extra_usage,omitempty"`
}

func TestGetUsageSuccess(t *testing.T) {
	// Proves that GetUsage sends a request to the correct path with the OAuth Bearer token and anthropic-beta header, and correctly maps the utilization from 0-100 to 0-1.
	util := 55.0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" {
			t.Errorf("path = %q, want /api/oauth/usage", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-oauth-token" {
			t.Errorf("Authorization = %q", auth)
		}
		if beta := r.Header.Get("anthropic-beta"); !strings.Contains(beta, "oauth-2025-04-20") {
			t.Errorf("anthropic-beta = %q, want oauth", beta)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(usageTestResponse{
			FiveHour: &usageAPIWindow{Utilization: &util},
		})
	}))
	defer server.Close()

	client := &UsageClient{
		tokenFunc:  func() (string, error) { return "test-oauth-token", nil },
		httpClient: http.DefaultClient,
		baseURL:    server.URL,
		cacheTTL:   defaultCacheTTL,
	}

	resp, err := client.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("GetUsage: %v", err)
	}
	if resp == nil {
		t.Fatal("response is nil")
	}
	if resp.Utilization == nil {
		t.Fatal("Utilization is nil")
	}
	// API returns 55.0 (0-100); interface should return 0.55 (0-1)
	if got := *resp.Utilization; got != 0.55 {
		t.Errorf("utilization = %f, want 0.55", got)
	}
	if resp.Period != 5*time.Hour {
		t.Errorf("period = %v, want 5h", resp.Period)
	}
}

func TestGetUsageTokenError(t *testing.T) {
	// Proves that GetUsage returns the tokenFunc error when the token source fails.
	client := &UsageClient{
		tokenFunc:  func() (string, error) { return "", fmt.Errorf("no credentials") },
		httpClient: http.DefaultClient,
		baseURL:    "http://localhost",
		cacheTTL:   defaultCacheTTL,
	}

	_, err := client.GetUsage(context.Background())
	if err == nil {
		t.Fatal("expected error for failing token func")
	}
	if !strings.Contains(err.Error(), "no credentials") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestGetUsageAPIError(t *testing.T) {
	// Proves that a 401 from the usage endpoint surfaces as a descriptive API error with the status code, rather than returning empty data.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid token"}`))
	}))
	defer server.Close()

	client := &UsageClient{
		tokenFunc:  func() (string, error) { return "bad-token", nil },
		httpClient: http.DefaultClient,
		baseURL:    server.URL,
		cacheTTL:   defaultCacheTTL,
	}

	_, err := client.GetUsage(context.Background())
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "API error (status 401)") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestGetUsageCacheHit(t *testing.T) {
	// Proves that a second call to GetUsage within the cache TTL returns the same cached response pointer without making a second HTTP request to the server.
	var calls atomic.Int32
	util := 55.0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(usageTestResponse{
			FiveHour: &usageAPIWindow{Utilization: &util},
		})
	}))
	defer server.Close()

	client := &UsageClient{
		tokenFunc:  func() (string, error) { return "tok", nil },
		httpClient: http.DefaultClient,
		baseURL:    server.URL,
		cacheTTL:   5 * time.Minute,
	}

	// First call — hits server
	resp, err := client.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if got := *resp.Utilization; got != 0.55 {
		t.Fatalf("first call util = %f, want 0.55", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("first call: server hit count = %d, want 1", calls.Load())
	}

	// Second call — cache hit, no server request
	resp2, err := client.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if resp2 != resp {
		t.Error("second call returned different pointer (expected cache hit)")
	}
	if calls.Load() != 1 {
		t.Fatalf("second call: server hit count = %d, want 1", calls.Load())
	}
}

func TestGetUsageCacheExpiry(t *testing.T) {
	// Proves that GetUsage re-fetches from the server after the cache TTL expires, by using a very short TTL and verifying two distinct server hits.
	var calls atomic.Int32
	util := 55.0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(usageTestResponse{
			FiveHour: &usageAPIWindow{Utilization: &util},
		})
	}))
	defer server.Close()

	client := &UsageClient{
		tokenFunc:  func() (string, error) { return "tok", nil },
		httpClient: http.DefaultClient,
		baseURL:    server.URL,
		cacheTTL:   1 * time.Millisecond,
	}

	// First call
	_, err := client.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("first call: server hit count = %d, want 1", calls.Load())
	}

	// Wait for cache to expire
	time.Sleep(5 * time.Millisecond)

	// Second call — cache expired, hits server again
	_, err = client.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("after expiry: server hit count = %d, want 2", calls.Load())
	}
}

func TestInvalidateForcesFetch(t *testing.T) {
	// Proves that Invalidate() clears the cache so the next GetUsage call hits the server even when the TTL has not yet expired.
	var calls atomic.Int32
	util := 55.0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(usageTestResponse{
			FiveHour: &usageAPIWindow{Utilization: &util},
		})
	}))
	defer server.Close()

	client := &UsageClient{
		tokenFunc:  func() (string, error) { return "tok", nil },
		httpClient: http.DefaultClient,
		baseURL:    server.URL,
		cacheTTL:   5 * time.Minute,
	}

	// First call — hits server
	_, err := client.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Invalidate and call again — should hit server
	client.Invalidate()
	_, err = client.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("after invalidate: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("after invalidate: server hit count = %d, want 2", calls.Load())
	}
}

func TestErrorBackoff(t *testing.T) {
	// Proves that consecutive fetch failures trigger exponential backoff: the first error sets a backoff window equal to the cache TTL, subsequent calls within the window are suppressed (no server hit), and after the window expires the client retries and doubles the backoff.
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`internal error`))
	}))
	defer server.Close()

	client := &UsageClient{
		tokenFunc:  func() (string, error) { return "tok", nil },
		httpClient: http.DefaultClient,
		baseURL:    server.URL,
		cacheTTL:   100 * time.Millisecond, // short for testing
	}

	// First call — hits server, fails, sets backoff = cacheTTL (100ms)
	_, err := client.GetUsage(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Fatalf("first call: server hits = %d, want 1", calls.Load())
	}

	// Immediate retry — should be suppressed by backoff (no server hit)
	_, err = client.GetUsage(context.Background())
	if err == nil {
		t.Fatal("expected error during backoff")
	}
	if calls.Load() != 1 {
		t.Fatalf("during backoff: server hits = %d, want 1", calls.Load())
	}

	// Wait for backoff to expire, then retry — should hit server again
	time.Sleep(150 * time.Millisecond)
	_, err = client.GetUsage(context.Background())
	if err == nil {
		t.Fatal("expected error after backoff")
	}
	if calls.Load() != 2 {
		t.Fatalf("after backoff: server hits = %d, want 2", calls.Load())
	}

	// Backoff should have doubled to 200ms — verify suppression
	_, _ = client.GetUsage(context.Background())
	if calls.Load() != 2 {
		t.Fatalf("during doubled backoff: server hits = %d, want 2", calls.Load())
	}
}

func TestErrorBackoffResetsOnSuccess(t *testing.T) {
	// Proves that a successful response after a period of failures fully clears the error backoff state, leaving errBackoff at zero and lastErr nil.
	var calls atomic.Int32
	failing := true
	util := 55.0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if failing {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`error`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(usageTestResponse{
			FiveHour: &usageAPIWindow{Utilization: &util},
		})
	}))
	defer server.Close()

	client := &UsageClient{
		tokenFunc:  func() (string, error) { return "tok", nil },
		httpClient: http.DefaultClient,
		baseURL:    server.URL,
		cacheTTL:   1 * time.Millisecond,
	}

	// Fail once to enter backoff
	_, _ = client.GetUsage(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("first call: server hits = %d, want 1", calls.Load())
	}

	// Wait for backoff to expire, switch to success
	time.Sleep(5 * time.Millisecond)
	failing = false

	resp, err := client.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	if resp == nil || resp.Utilization == nil || *resp.Utilization != 0.55 {
		t.Fatalf("unexpected response: %+v", resp)
	}

	// Verify backoff state was cleared
	client.mu.Lock()
	bo := client.errBackoff
	le := client.lastErr
	client.mu.Unlock()
	if bo != 0 || le != nil {
		t.Fatalf("backoff not reset: errBackoff=%v, lastErr=%v", bo, le)
	}
}

func TestInvalidateClearsErrorBackoff(t *testing.T) {
	// Proves that Invalidate() also clears the error backoff state, allowing an immediate retry even while still within the backoff window — this supports the /mana force-refresh user flow.
	var calls atomic.Int32
	util := 55.0
	failing := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if failing {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`error`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(usageTestResponse{
			FiveHour: &usageAPIWindow{Utilization: &util},
		})
	}))
	defer server.Close()

	client := &UsageClient{
		tokenFunc:  func() (string, error) { return "tok", nil },
		httpClient: http.DefaultClient,
		baseURL:    server.URL,
		cacheTTL:   5 * time.Minute,
	}

	// Fail to enter backoff
	_, _ = client.GetUsage(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("first call: server hits = %d, want 1", calls.Load())
	}

	// Immediate retry suppressed
	_, _ = client.GetUsage(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("during backoff: server hits = %d, want 1", calls.Load())
	}

	// Invalidate clears backoff, switch server to success
	client.Invalidate()
	failing = false

	resp, err := client.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("after invalidate: %v", err)
	}
	if resp == nil || resp.Utilization == nil || *resp.Utilization != 0.55 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if calls.Load() != 2 {
		t.Fatalf("after invalidate: server hits = %d, want 2", calls.Load())
	}
}

func TestSetCacheTTL(t *testing.T) {
	// Proves that SetCacheTTL updates the cache TTL field and that NewUsageClient initialises it to the default TTL.
	client := NewUsageClient(func() (string, error) { return "tok", nil })
	if client.cacheTTL != defaultCacheTTL {
		t.Fatalf("default cacheTTL = %v, want %v", client.cacheTTL, defaultCacheTTL)
	}
	client.SetCacheTTL(10 * time.Second)
	client.mu.Lock()
	ttl := client.cacheTTL
	client.mu.Unlock()
	if ttl != 10*time.Second {
		t.Fatalf("after SetCacheTTL: cacheTTL = %v, want 10s", ttl)
	}
}

func TestTokenErrorSkipsBackoff(t *testing.T) {
	// Proves that token resolution errors don't trigger error backoff, so a
	// refreshed token is picked up on the very next call.
	tokenErr := true
	var calls atomic.Int32
	util := 42.0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(usageTestResponse{
			FiveHour: &usageAPIWindow{Utilization: &util},
		})
	}))
	defer server.Close()

	client := &UsageClient{
		tokenFunc: func() (string, error) {
			if tokenErr {
				return "", fmt.Errorf("CC token expired at 2026-03-14T13:56:36Z, refresh triggered")
			}
			return "fresh-token", nil
		},
		httpClient: http.DefaultClient,
		baseURL:    server.URL,
		cacheTTL:   5 * time.Minute,
	}

	// First call — token expired, returns error
	_, err := client.GetUsage(context.Background())
	if err == nil {
		t.Fatal("expected token error")
	}
	if calls.Load() != 0 {
		t.Fatalf("token error should not hit server, got %d hits", calls.Load())
	}

	// Verify no error backoff was set
	client.mu.Lock()
	bo := client.errBackoff
	le := client.lastErr
	client.mu.Unlock()
	if bo != 0 || le != nil {
		t.Fatalf("token error should not set backoff: errBackoff=%v, lastErr=%v", bo, le)
	}

	// Simulate token refresh completing — next call should succeed immediately
	tokenErr = false
	resp, err := client.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("after token refresh: %v", err)
	}
	// API returns 42.0 (0-100); interface should return 0.42 (0-1)
	if resp == nil || resp.Utilization == nil || *resp.Utilization != 0.42 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if calls.Load() != 1 {
		t.Fatalf("after token refresh: server hits = %d, want 1", calls.Load())
	}
}

func TestAPIErrorStillGetsBackoff(t *testing.T) {
	// Proves that API errors (as opposed to token errors) still trigger
	// exponential backoff — the token error fix didn't break normal backoff.
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`internal error`))
	}))
	defer server.Close()

	client := &UsageClient{
		tokenFunc: func() (string, error) {
			return "valid-token", nil
		},
		httpClient: http.DefaultClient,
		baseURL:    server.URL,
		cacheTTL:   100 * time.Millisecond,
	}

	// First call — API error, should set backoff
	_, err := client.GetUsage(context.Background())
	if err == nil {
		t.Fatal("expected API error")
	}
	if calls.Load() != 1 {
		t.Fatalf("first call: server hits = %d, want 1", calls.Load())
	}

	// Immediate retry — should be suppressed by backoff
	_, _ = client.GetUsage(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("during backoff: server hits = %d, want 1 (backoff should suppress)", calls.Load())
	}
}

func TestMapUsageResponseExtraInfo(t *testing.T) {
	// Proves that mapUsageResponse formats ExtraInfo from overage and 7-day window data.
	util5h := 40.0
	reset := "2026-04-04T20:00:00Z"
	util7d := 12.0
	raw := &usageAPIResponse{
		FiveHour: &usageAPIWindow{Utilization: &util5h, ResetsAt: &reset},
		SevenDay: &usageAPIWindow{Utilization: &util7d},
		ExtraUsage: &usageAPIExtra{
			IsEnabled:    true,
			MonthlyLimit: 10.0,
			UsedCredits:  2.40,
		},
	}

	w := mapUsageResponse(raw)
	if w.Utilization == nil || *w.Utilization != 0.40 {
		t.Errorf("utilization = %v, want 0.40", w.Utilization)
	}
	if w.ResetsAt.IsZero() {
		t.Error("ResetsAt is zero")
	}
	if !strings.Contains(w.ExtraInfo, "Overage: $2.40 / $10.00") {
		t.Errorf("ExtraInfo missing overage: %q", w.ExtraInfo)
	}
	if !strings.Contains(w.ExtraInfo, "7-day: 12%") {
		t.Errorf("ExtraInfo missing 7-day: %q", w.ExtraInfo)
	}
}

func TestMapUsageResponseNilFiveHour(t *testing.T) {
	// Proves that mapUsageResponse returns a valid window even when the 5-hour data is nil.
	w := mapUsageResponse(&usageAPIResponse{})
	if w == nil {
		t.Fatal("expected non-nil window")
	}
	if w.Utilization != nil {
		t.Errorf("expected nil utilization, got %v", *w.Utilization)
	}
	if w.Period != 5*time.Hour {
		t.Errorf("period = %v, want 5h", w.Period)
	}
}

func TestUsageClientSetBaseURLAndPostFetchHook(t *testing.T) {
	// Proves SetBaseURL redirects usage fetches, and the post-fetch hook fires after a fresh fetch but not on cache hits.
	util := 10.0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(usageTestResponse{FiveHour: &usageAPIWindow{Utilization: &util}})
	}))
	defer server.Close()

	client := NewUsageClient(func() (string, error) { return "tok", nil })
	client.SetBaseURL(server.URL)

	var hookCalls atomic.Int32
	client.SetPostFetchHook(func() { hookCalls.Add(1) })

	if _, err := client.GetUsage(context.Background()); err != nil {
		t.Fatalf("GetUsage: %v", err)
	}
	if hookCalls.Load() != 1 {
		t.Errorf("hook calls after fetch = %d, want 1", hookCalls.Load())
	}

	// Cached call: no fetch, no hook.
	if _, err := client.GetUsage(context.Background()); err != nil {
		t.Fatalf("GetUsage cached: %v", err)
	}
	if hookCalls.Load() != 1 {
		t.Errorf("hook calls after cache hit = %d, want still 1", hookCalls.Load())
	}
}

func TestTokenResolutionErrorUnwrap(t *testing.T) {
	// Proves tokenResolutionError preserves the underlying error for errors.Is/As chains — GetUsage's backoff exemption depends on this.
	inner := fmt.Errorf("token gone")
	wrapped := &tokenResolutionError{inner}
	if !errors.Is(wrapped, inner) {
		t.Error("errors.Is failed to see through tokenResolutionError")
	}
	if wrapped.Error() != "token gone" {
		t.Errorf("Error() = %q", wrapped.Error())
	}
}

func TestGetUsageMalformedJSON(t *testing.T) {
	// Proves a non-JSON 200 response is reported as an unmarshal error rather than a panic or silent zero-value usage.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>gateway error</html>")
	}))
	defer server.Close()

	client := NewUsageClient(func() (string, error) { return "tok", nil })
	client.SetBaseURL(server.URL)

	_, err := client.GetUsage(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("err = %v, want unmarshal error", err)
	}
}
