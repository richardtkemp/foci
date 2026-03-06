package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestHandleMessageRateLimit(t *testing.T) {
	// Server returns 429 with Retry-After header.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"rate limited"}}`))
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	var rateLimitCalled bool
	var rateLimitRetry int

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
		RateLimitFunc: func(retryAfter int) {
			rateLimitCalled = true
			rateLimitRetry = retryAfter
		},
	}

	_, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "Hello")
	if err == nil {
		t.Fatal("expected error for rate limit")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error = %q, want rate limited message", err.Error())
	}

	if !rateLimitCalled {
		t.Error("RateLimitFunc not called")
	}
	if rateLimitRetry != 120 {
		t.Errorf("retryAfter = %d, want 120", rateLimitRetry)
	}
}

func TestHandleMessageOverloaded(t *testing.T) {
	// Server returns 529 Overloaded — should get overloaded message, not rate limit.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(529)
		w.Write([]byte(`{"error":{"type":"overloaded_error","message":"overloaded"}}`))
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	var rateLimitCalled bool

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
		RateLimitFunc: func(retryAfter int) {
			rateLimitCalled = true
		},
	}

	_, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "Hello")
	if err == nil {
		t.Fatal("expected error for overloaded")
	}
	if !strings.Contains(err.Error(), "overloaded") {
		t.Errorf("error = %q, want overloaded message", err.Error())
	}
	if strings.Contains(err.Error(), "mana exhausted") {
		t.Errorf("error = %q, should not mention mana exhausted for 529", err.Error())
	}

	if rateLimitCalled {
		t.Error("RateLimitFunc should not be called for 529")
	}
}

func TestHandleMessageRateLimitNoCallback(t *testing.T) {
	// 429 without RateLimitFunc — should still return friendly error, not crash.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"rate limited"}}`))
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
		// RateLimitFunc intentionally nil
	}

	_, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "Hello")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error = %q, want rate limited message", err.Error())
	}
}

func TestHandleMessageServerError(t *testing.T) {
	// Server returns 500 Internal Server Error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`))
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	var rateLimitCalled bool

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
		RateLimitFunc: func(retryAfter int) {
			rateLimitCalled = true
			if retryAfter != 0 {
				t.Errorf("retryAfter = %d, want 0 for server error", retryAfter)
			}
		},
	}

	_, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "Hello")
	if err == nil {
		t.Fatal("expected error for server error")
	}
	if !strings.Contains(err.Error(), "temporarily unavailable") {
		t.Errorf("error = %q, want friendly server error message", err.Error())
	}
	// Should not contain raw JSON
	if strings.Contains(err.Error(), `"type":"error"`) {
		t.Errorf("error = %q, should not contain raw JSON", err.Error())
	}

	if !rateLimitCalled {
		t.Error("RateLimitFunc not called for 500")
	}
}

func TestHandleMessageServerErrorNoCallback(t *testing.T) {
	// 500 without RateLimitFunc — should still return friendly error, not crash.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`))
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
		// RateLimitFunc intentionally nil
	}

	_, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "Hello")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "temporarily unavailable") {
		t.Errorf("error = %q, want friendly server error message", err.Error())
	}
}

