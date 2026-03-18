package provider

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// retryMockClient is a test client that allows controlling success/failure behavior for retry tests.
type retryMockClient struct {
	attempts      *atomic.Int32
	failUntil     int // fail until this attempt number
	failWith      error
	successResp   *MessageResponse
	streamHandler *StreamHandler
}

func (m *retryMockClient) SendMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error) {
	n := int(m.attempts.Add(1))
	if n <= m.failUntil {
		return nil, m.failWith
	}
	return m.successResp, nil
}

func (m *retryMockClient) CountTokens(ctx context.Context, req *MessageRequest) (int, error) {
	return 0, errors.New("not implemented")
}

func (m *retryMockClient) IsCachingAvailable() bool {
	return true
}

func (m *retryMockClient) StreamMessage(ctx context.Context, req *MessageRequest, handler *StreamHandler) (*MessageResponse, error) {
	m.streamHandler = handler
	return m.SendMessage(ctx, req)
}

// retryMockRetryableClient extends retryMockClient with overload recovery support.
type retryMockRetryableClient struct {
	retryMockClient
	recoverCh       chan struct{}
	recoverySignals int
}

func (m *retryMockRetryableClient) OnRetrySuccess() {
	m.recoverySignals++
	if m.recoverCh != nil {
		close(m.recoverCh)
	}
}

func (m *retryMockRetryableClient) WaitForRecovery() <-chan struct{} {
	if m.recoverCh == nil {
		m.recoverCh = make(chan struct{})
	}
	return m.recoverCh
}

func (m *retryMockRetryableClient) RetryBaseDelay() time.Duration {
	return time.Millisecond // fast retries for tests
}

func (m *retryMockRetryableClient) OverloadBaseDelay() time.Duration {
	return 5 * time.Millisecond // fast overload retries for tests
}

func (m *retryMockRetryableClient) OverloadMaxDuration() time.Duration {
	return 500 * time.Millisecond // short max duration for tests
}

func (m *retryMockRetryableClient) ServerErrorMaxDuration() time.Duration {
	return 100 * time.Millisecond // short max duration for tests
}

func TestRetryCallbacks(t *testing.T) {
	// Verifies that retry callbacks are called exactly once per
	// retry sequence, and success callback is called when retry succeeds.
	attempts := &atomic.Int32{}
	client := &retryMockClient{
		attempts:  attempts,
		failUntil: 2, // fail first 2 attempts, succeed on 3rd
		failWith: &APIError{
			StatusCode: 500,
			Body:       "internal server error",
		},
		successResp: &MessageResponse{
			ID:      "msg_ok",
			Content: []ContentBlock{{Type: "text", Text: "hello"}},
		},
	}

	var firstRetryCalled, successCalled bool
	var firstRetryEndpoint string
	ctx := WithRetryCallbacks(context.Background(), &RetryCallbacks{
		OnFirstRetry: func(endpoint string) {
			if firstRetryCalled {
				t.Error("OnFirstRetry called more than once")
			}
			firstRetryCalled = true
			firstRetryEndpoint = endpoint
		},
		OnSuccess: func() {
			successCalled = true
		},
	})

	resp, err := sendWithRetry(ctx, client, &MessageRequest{
		Model:     "test-model",
		MaxTokens: 256,
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if int(attempts.Load()) != 3 {
		t.Errorf("attempts = %d, want 3", attempts.Load())
	}
	if resp.ID != "msg_ok" {
		t.Errorf("resp.ID = %q, want msg_ok", resp.ID)
	}
	if !firstRetryCalled {
		t.Error("OnFirstRetry was not called")
	}
	if firstRetryEndpoint != "API" {
		t.Errorf("OnFirstRetry endpoint = %q, want API", firstRetryEndpoint)
	}
	if !successCalled {
		t.Error("OnSuccess was not called")
	}
}

func TestRetryCallbacksNoRetry(t *testing.T) {
	// Verifies that when request succeeds on first attempt,
	// callbacks should not fire.
	attempts := &atomic.Int32{}
	client := &retryMockClient{
		attempts:  attempts,
		failUntil: 0, // succeed immediately
		successResp: &MessageResponse{
			ID:      "msg_ok",
			Content: []ContentBlock{{Type: "text", Text: "hello"}},
		},
	}

	var firstRetryCalled, successCalled bool
	ctx := WithRetryCallbacks(context.Background(), &RetryCallbacks{
		OnFirstRetry: func(endpoint string) {
			firstRetryCalled = true
		},
		OnSuccess: func() {
			successCalled = true
		},
	})

	_, err := sendWithRetry(ctx, client, &MessageRequest{
		Model:     "test-model",
		MaxTokens: 256,
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if firstRetryCalled {
		t.Error("OnFirstRetry should not be called on first-attempt success")
	}
	if successCalled {
		t.Error("OnSuccess should not be called on first-attempt success")
	}
}

func TestRetryCallbacks529Overload(t *testing.T) {
	// Verifies callbacks work with 529 overload retries
	// (phase 2) when using a retryableClient.
	attempts := &atomic.Int32{}
	client := &retryMockRetryableClient{
		retryMockClient: retryMockClient{
			attempts:  attempts,
			failUntil: 6, // fail first 6 attempts (4 in phase 1, 2 in phase 2)
			failWith: &APIError{
				StatusCode: 529,
				Body:       "overloaded",
			},
			successResp: &MessageResponse{
				ID:      "msg_ok",
				Content: []ContentBlock{{Type: "text", Text: "hello"}},
			},
		},
	}

	var firstRetryCalled, successCalled bool
	ctx := WithRetryCallbacks(context.Background(), &RetryCallbacks{
		OnFirstRetry: func(endpoint string) {
			if firstRetryCalled {
				t.Error("OnFirstRetry called more than once")
			}
			firstRetryCalled = true
		},
		OnSuccess: func() {
			successCalled = true
		},
	})

	resp, err := sendWithRetry(ctx, client, &MessageRequest{
		Model:     "test-model",
		MaxTokens: 256,
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ID != "msg_ok" {
		t.Errorf("resp.ID = %q, want msg_ok", resp.ID)
	}
	if !firstRetryCalled {
		t.Error("OnFirstRetry was not called")
	}
	if !successCalled {
		t.Error("OnSuccess was not called")
	}
	if client.recoverySignals != 1 {
		t.Errorf("recoverySignals = %d, want 1", client.recoverySignals)
	}
}

func TestRetryNonRetryableError(t *testing.T) {
	// Verifies that non-retryable errors stop immediately.
	attempts := &atomic.Int32{}
	client := &retryMockClient{
		attempts:  attempts,
		failUntil: 10, // would fail many times if retried
		failWith: &APIError{
			StatusCode: 400, // non-retryable
			Body:       "bad request",
		},
	}

	_, err := sendWithRetry(context.Background(), client, &MessageRequest{
		Model:     "test-model",
		MaxTokens: 256,
	}, nil)

	if err == nil {
		t.Fatal("expected error")
	}
	if int(attempts.Load()) != 1 {
		t.Errorf("attempts = %d, want 1 (should not retry 400)", attempts.Load())
	}
}

func TestRetryStreamingClient(t *testing.T) {
	// Verifies retry works with streaming clients.
	attempts := &atomic.Int32{}
	client := &retryMockClient{
		attempts:  attempts,
		failUntil: 1, // fail once, then succeed
		failWith: &APIError{
			StatusCode: 503,
			Body:       "service unavailable",
		},
		successResp: &MessageResponse{
			ID:      "msg_ok",
			Content: []ContentBlock{{Type: "text", Text: "hello"}},
		},
	}

	handler := &StreamHandler{
		OnTextDelta: func(delta string) {
			// no-op for test
		},
	}

	resp, err := sendWithRetry(context.Background(), client, &MessageRequest{
		Model:     "test-model",
		MaxTokens: 256,
	}, handler)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if int(attempts.Load()) != 2 {
		t.Errorf("attempts = %d, want 2", attempts.Load())
	}
	if resp.ID != "msg_ok" {
		t.Errorf("resp.ID = %q, want msg_ok", resp.ID)
	}
	if client.streamHandler == nil {
		t.Error("stream handler was not passed to client")
	}
}

func TestEndpointNameFromURL(t *testing.T) {
	// Verifies that EndpointNameFromURL extracts a readable name from
	// various API base URL formats.
	tests := []struct {
		url  string
		want string
	}{
		{"https://api.openrouter.ai/v1", "Openrouter"},
		{"https://api.together.xyz/v1", "Together"},
		{"https://api.groq.com/v1", "Groq"},
		{"https://generativelanguage.googleapis.com", "Googleapis"},
		{"https://localhost:8080", "localhost:8080"},
		{"not-a-url", "not-a-url"},
	}
	for _, tt := range tests {
		got := EndpointNameFromURL(tt.url)
		if got != tt.want {
			t.Errorf("EndpointNameFromURL(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestEndpointFromClientFallback(t *testing.T) {
	// Verifies that endpointFromClient falls back to "API" for clients
	// that don't implement endpointDescriber.
	client := &retryMockClient{
		attempts: &atomic.Int32{},
	}
	got := endpointFromClient(client)
	if got != "API" {
		t.Errorf("endpointFromClient = %q, want %q", got, "API")
	}
}

func TestRetry500ExtendsWithShorterDuration(t *testing.T) {
	// Verifies that 500 errors trigger phase 2 extended retry
	// (with shorter ServerErrorMaxDuration, not the longer OverloadMaxDuration).
	attempts := &atomic.Int32{}
	client := &retryMockRetryableClient{
		retryMockClient: retryMockClient{
			attempts:  attempts,
			failUntil: 100, // always fail
			failWith: &APIError{
				StatusCode: 500,
				Body:       "internal server error",
			},
		},
	}

	start := time.Now()
	_, err := sendWithRetry(context.Background(), client, &MessageRequest{
		Model:     "test-model",
		MaxTokens: 256,
	}, nil)

	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error")
	}
	// Should do phase 1 (4 attempts) + phase 2 extended retry (with 100ms max)
	if int(attempts.Load()) <= 4 {
		t.Errorf("attempts = %d, want > 4 (phase 2 should fire for 500s)", attempts.Load())
	}
	// Should NOT run as long as overload retry (500ms), should be bounded by ServerErrorMaxDuration (100ms)
	// Allow generous headroom for CI but ensure we didn't use the overload duration.
	if elapsed > 400*time.Millisecond {
		t.Errorf("elapsed = %s, want < 400ms (should use ServerErrorMaxDuration, not OverloadMaxDuration)", elapsed)
	}
}

func TestRetry500Recovery(t *testing.T) {
	// Verifies that a 500 error recovers after retries in phase 2.
	attempts := &atomic.Int32{}
	client := &retryMockRetryableClient{
		retryMockClient: retryMockClient{
			attempts:  attempts,
			failUntil: 6, // fail first 6 (4 phase 1 + 2 phase 2), succeed on 7th
			failWith: &APIError{
				StatusCode: 500,
				Body:       "internal server error",
			},
			successResp: &MessageResponse{
				ID:      "msg_recovered",
				Content: []ContentBlock{{Type: "text", Text: "recovered"}},
			},
		},
	}

	resp, err := sendWithRetry(context.Background(), client, &MessageRequest{
		Model:     "test-model",
		MaxTokens: 256,
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ID != "msg_recovered" {
		t.Errorf("resp.ID = %q, want msg_recovered", resp.ID)
	}
	if int(attempts.Load()) != 7 {
		t.Errorf("attempts = %d, want 7", attempts.Load())
	}
}
