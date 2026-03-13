package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetUsageSuccess(t *testing.T) {
	// Proves that GetUsage sends a request to the correct path with the OAuth Bearer token and anthropic-beta header, and correctly deserializes the utilization value from the response.
	util := 55.0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify path
		if r.URL.Path != "/api/oauth/usage" {
			t.Errorf("path = %q, want /api/oauth/usage", r.URL.Path)
		}
		// Verify headers
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-oauth-token" {
			t.Errorf("Authorization = %q", auth)
		}
		if beta := r.Header.Get("anthropic-beta"); !strings.Contains(beta, "oauth-2025-04-20") {
			t.Errorf("anthropic-beta = %q, want oauth", beta)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(UsageResponse{
			FiveHour: &UsageWindow{Utilization: &util},
		})
	}))
	defer server.Close()

	client := &UsageClient{
		oauthToken: "test-oauth-token",
		httpClient: http.DefaultClient,
		baseURL:    server.URL,
		cacheTTL:   defaultCacheTTL,
	}

	resp, err := client.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("GetUsage: %v", err)
	}
	if resp.FiveHour == nil {
		t.Fatal("FiveHour is nil")
	}
	if *resp.FiveHour.Utilization != 55.0 {
		t.Errorf("utilization = %f, want 55.0", *resp.FiveHour.Utilization)
	}
}

func TestGetUsageEmptyToken(t *testing.T) {
	// Proves that GetUsage returns a descriptive error when no OAuth token is configured, rather than sending an unauthenticated request.
	client := &UsageClient{
		oauthToken: "",
		httpClient: http.DefaultClient,
		baseURL:    "http://localhost",
		cacheTTL:   defaultCacheTTL,
	}

	_, err := client.GetUsage(context.Background())
	if err == nil {
		t.Fatal("expected error for empty token")
	}
	if !strings.Contains(err.Error(), "OAuth token not configured") {
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
		oauthToken: "bad-token",
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
		json.NewEncoder(w).Encode(UsageResponse{
			FiveHour: &UsageWindow{Utilization: &util},
		})
	}))
	defer server.Close()

	client := &UsageClient{
		oauthToken: "tok",
		httpClient: http.DefaultClient,
		baseURL:    server.URL,
		cacheTTL:   5 * time.Minute,
	}

	// First call — hits server
	resp, err := client.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if *resp.FiveHour.Utilization != 55.0 {
		t.Fatalf("first call util = %f", *resp.FiveHour.Utilization)
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
		json.NewEncoder(w).Encode(UsageResponse{
			FiveHour: &UsageWindow{Utilization: &util},
		})
	}))
	defer server.Close()

	client := &UsageClient{
		oauthToken: "tok",
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
		json.NewEncoder(w).Encode(UsageResponse{
			FiveHour: &UsageWindow{Utilization: &util},
		})
	}))
	defer server.Close()

	client := &UsageClient{
		oauthToken: "tok",
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
		oauthToken: "tok",
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
		json.NewEncoder(w).Encode(UsageResponse{
			FiveHour: &UsageWindow{Utilization: &util},
		})
	}))
	defer server.Close()

	client := &UsageClient{
		oauthToken: "tok",
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
	if resp.FiveHour == nil || *resp.FiveHour.Utilization != 55.0 {
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
		json.NewEncoder(w).Encode(UsageResponse{
			FiveHour: &UsageWindow{Utilization: &util},
		})
	}))
	defer server.Close()

	client := &UsageClient{
		oauthToken: "tok",
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
	if resp.FiveHour == nil || *resp.FiveHour.Utilization != 55.0 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if calls.Load() != 2 {
		t.Fatalf("after invalidate: server hits = %d, want 2", calls.Load())
	}
}

func TestSetCacheTTL(t *testing.T) {
	// Proves that SetCacheTTL updates the cache TTL field and that NewUsageClient initialises it to the default TTL.
	client := NewUsageClient("tok")
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
