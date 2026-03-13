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

// TestRetryCallbacks verifies that retry callbacks are called exactly once per
// retry sequence, and success callback is called when retry succeeds.
func TestRetryCallbacks(t *testing.T) {
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

	resp, err := Send(ctx, client, &MessageRequest{
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

// TestRetryCallbacksNoRetry verifies that when request succeeds on first attempt,
// callbacks should not fire.
func TestRetryCallbacksNoRetry(t *testing.T) {
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

	_, err := Send(ctx, client, &MessageRequest{
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

// TestRetryCallbacks529Overload verifies callbacks work with 529 overload retries
// (phase 2) when using a retryableClient.
func TestRetryCallbacks529Overload(t *testing.T) {
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

	resp, err := Send(ctx, client, &MessageRequest{
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

// TestRetryNonRetryableError verifies that non-retryable errors stop immediately.
func TestRetryNonRetryableError(t *testing.T) {
	attempts := &atomic.Int32{}
	client := &retryMockClient{
		attempts:  attempts,
		failUntil: 10, // would fail many times if retried
		failWith: &APIError{
			StatusCode: 400, // non-retryable
			Body:       "bad request",
		},
	}

	_, err := Send(context.Background(), client, &MessageRequest{
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

// TestRetryStreamingClient verifies retry works with streaming clients.
func TestRetryStreamingClient(t *testing.T) {
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

	resp, err := Send(context.Background(), client, &MessageRequest{
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

// TestRetry500DoesNotExtendTo529 verifies that 500 errors don't trigger phase 2.
func TestRetry500DoesNotExtendTo529(t *testing.T) {
	attempts := &atomic.Int32{}
	client := &retryMockRetryableClient{
		retryMockClient: retryMockClient{
			attempts:  attempts,
			failUntil: 10, // always fail
			failWith: &APIError{
				StatusCode: 500, // retryable but not overloaded
				Body:       "internal server error",
			},
		},
	}

	_, err := Send(context.Background(), client, &MessageRequest{
		Model:     "test-model",
		MaxTokens: 256,
	}, nil)

	if err == nil {
		t.Fatal("expected error")
	}
	// Should only do phase 1 (4 attempts), not phase 2
	if int(attempts.Load()) != 4 {
		t.Errorf("attempts = %d, want 4 (phase 1 only)", attempts.Load())
	}
}
