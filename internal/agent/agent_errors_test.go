package agent

import (
	"context"
	"strings"
	"testing"

	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestHandleMessageRateLimit(t *testing.T) {
	// Background trigger: RateLimitFunc must fire so background processes notify users.
	client := newTestClientWithError(func(_ context.Context, _ *provider.MessageRequest) (*provider.MessageResponse, error) {
		return nil, &provider.APIError{StatusCode: 429, RetryAfter: "120", Body: "rate limited"}
	})
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

	ctx := WithTrigger(context.Background(), "keepalive")
	_, err := ag.HandleMessage(ctx, "test/imain/1000000000", "Hello")
	if err == nil {
		t.Fatal("expected error for rate limit")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error = %q, want rate limited message", err.Error())
	}

	if !rateLimitCalled {
		t.Error("RateLimitFunc not called for background trigger")
	}
	if rateLimitRetry != 120 {
		t.Errorf("retryAfter = %d, want 120", rateLimitRetry)
	}
}

func TestHandleMessageRateLimitUserTrigger(t *testing.T) {
	// User trigger: RateLimitFunc must NOT fire — the error response to the user is sufficient.
	client := newTestClientWithError(func(_ context.Context, _ *provider.MessageRequest) (*provider.MessageResponse, error) {
		return nil, &provider.APIError{StatusCode: 429, RetryAfter: "120", Body: "rate limited"}
	})
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

	for _, trigger := range []string{"telegram", "user", "voice", ""} {
		rateLimitCalled = false
		ctx := WithTrigger(context.Background(), trigger)
		_, err := ag.HandleMessage(ctx, "test/iusertrigger/1000000000", "Hello")
		if err == nil {
			t.Fatalf("trigger=%q: expected error for rate limit", trigger)
		}
		if rateLimitCalled {
			t.Errorf("trigger=%q: RateLimitFunc should not be called for user trigger", trigger)
		}
	}
}

func TestHandleMessageOverloaded(t *testing.T) {
	// Client returns 529 Overloaded — should get overloaded message, not rate limit.
	client := newTestClientWithError(func(_ context.Context, _ *provider.MessageRequest) (*provider.MessageResponse, error) {
		return nil, &provider.APIError{StatusCode: 529, Body: "overloaded"}
	})
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
	client := newTestClientWithError(func(_ context.Context, _ *provider.MessageRequest) (*provider.MessageResponse, error) {
		return nil, &provider.APIError{StatusCode: 429, Body: "rate limited"}
	})
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
	// Client returns 500 Internal Server Error.
	client := newTestClientWithError(func(_ context.Context, _ *provider.MessageRequest) (*provider.MessageResponse, error) {
		return nil, &provider.APIError{StatusCode: 500, Body: "Internal server error"}
	})
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
	client := newTestClientWithError(func(_ context.Context, _ *provider.MessageRequest) (*provider.MessageResponse, error) {
		return nil, &provider.APIError{StatusCode: 500, Body: "Internal server error"}
	})
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
