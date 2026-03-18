package provider

import (
	"context"
	"testing"
)

// mockClient implements Client but not StreamingClient
type mockClient struct {
	sendCalled bool
	response   *MessageResponse
	err        error
}

func (m *mockClient) SendMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error) {
	m.sendCalled = true
	return m.response, m.err
}

func (m *mockClient) CountTokens(ctx context.Context, req *MessageRequest) (int, error) {
	return 0, nil
}

func (m *mockClient) IsCachingAvailable() bool {
	return true
}

// mockStreamingClient implements both Client and StreamingClient
type mockStreamingClient struct {
	sendCalled   bool
	streamCalled bool
	response     *MessageResponse
	err          error
}

func (m *mockStreamingClient) SendMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error) {
	m.sendCalled = true
	return m.response, m.err
}

func (m *mockStreamingClient) StreamMessage(ctx context.Context, req *MessageRequest, handler *StreamHandler) (*MessageResponse, error) {
	m.streamCalled = true
	return m.response, m.err
}

func (m *mockStreamingClient) CountTokens(ctx context.Context, req *MessageRequest) (int, error) {
	return 0, nil
}

func (m *mockStreamingClient) IsCachingAvailable() bool {
	return true
}

func TestSendWithNilHandlerUsesNonStreaming(t *testing.T) {
	// Proves that Send dispatches to SendMessage (not StreamMessage) when handler is nil,
	// even when the client supports streaming.
	mock := &mockStreamingClient{
		response: &MessageResponse{
			ID:   "msg_test",
			Type: "message",
			Role: "assistant",
			Content: []ContentBlock{
				{Type: "text", Text: "Hello"},
			},
		},
	}

	req := &MessageRequest{
		Model:     "test-model",
		MaxTokens: 100,
		Messages: []Message{
			{Role: "user", Content: TextContent("Hi")},
		},
	}

	// Call with nil handler - should use SendMessage, not StreamMessage
	resp, err := sendWithRetry(context.Background(), mock, req, nil)
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if resp == nil {
		t.Fatal("expected response, got nil")
	}

	if mock.streamCalled {
		t.Error("StreamMessage was called with nil handler - should use SendMessage instead")
	}

	if !mock.sendCalled {
		t.Error("SendMessage was not called with nil handler")
	}
}

func TestSendWithHandlerUsesStreaming(t *testing.T) {
	// Proves that Send dispatches to StreamMessage when a handler is provided and the
	// client supports streaming.
	mock := &mockStreamingClient{
		response: &MessageResponse{
			ID:   "msg_test",
			Type: "message",
			Role: "assistant",
			Content: []ContentBlock{
				{Type: "text", Text: "Hello"},
			},
		},
	}

	req := &MessageRequest{
		Model:     "test-model",
		MaxTokens: 100,
		Messages: []Message{
			{Role: "user", Content: TextContent("Hi")},
		},
	}

	handler := &StreamHandler{}

	// Call with handler - should use StreamMessage
	resp, err := sendWithRetry(context.Background(), mock, req, handler)
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if resp == nil {
		t.Fatal("expected response, got nil")
	}

	if !mock.streamCalled {
		t.Error("StreamMessage was not called when handler provided")
	}

	if mock.sendCalled {
		t.Error("SendMessage was called when handler provided - should use StreamMessage")
	}
}

func TestSendWithNonStreamingClientAlwaysUsesSendMessage(t *testing.T) {
	// Proves that Send always falls back to SendMessage when the client does not
	// implement StreamingClient, regardless of whether a handler is provided.
	mock := &mockClient{
		response: &MessageResponse{
			ID:   "msg_test",
			Type: "message",
			Role: "assistant",
			Content: []ContentBlock{
				{Type: "text", Text: "Hello"},
			},
		},
	}

	req := &MessageRequest{
		Model:     "test-model",
		MaxTokens: 100,
		Messages: []Message{
			{Role: "user", Content: TextContent("Hi")},
		},
	}

	// Even with a handler, non-streaming client should use SendMessage
	handler := &StreamHandler{}
	resp, err := sendWithRetry(context.Background(), mock, req, handler)
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if resp == nil {
		t.Fatal("expected response, got nil")
	}

	if !mock.sendCalled {
		t.Error("SendMessage was not called for non-streaming client")
	}
}

func TestSendWithEmptyHandlerUsesStreaming(t *testing.T) {
	// Proves that an empty StreamHandler (non-nil but with no callbacks set) still
	// triggers the streaming path rather than falling back to SendMessage.
	mock := &mockStreamingClient{
		response: &MessageResponse{
			ID:   "msg_test",
			Type: "message",
			Role: "assistant",
			Content: []ContentBlock{
				{Type: "text", Text: "Hello"},
			},
		},
	}

	req := &MessageRequest{
		Model:     "test-model",
		MaxTokens: 100,
		Messages: []Message{
			{Role: "user", Content: TextContent("Hi")},
		},
	}

	// Empty handler (no callbacks) should still use streaming
	handler := &StreamHandler{}

	resp, err := sendWithRetry(context.Background(), mock, req, handler)
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if resp == nil {
		t.Fatal("expected response, got nil")
	}

	if !mock.streamCalled {
		t.Error("StreamMessage was not called with empty handler")
	}
}

// mockSelfRetryingClient implements a client that handles its own retries.
type mockSelfRetryingClient struct {
	mockClient
	sendMessageCalls int
}

func (m *mockSelfRetryingClient) HandlesOwnRetries() bool {
	return true
}

func (m *mockSelfRetryingClient) SendMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error) {
	m.sendMessageCalls++
	return m.mockClient.SendMessage(ctx, req)
}

func TestSendWithSelfRetryingClientSkipsProviderRetry(t *testing.T) {
	// Proves that Send invokes SendMessage exactly once for clients that declare they
	// handle their own retries, bypassing the provider-level retry wrapper entirely.
	mock := &mockSelfRetryingClient{
		mockClient: mockClient{
			response: &MessageResponse{
				ID:   "msg_test",
				Type: "message",
				Role: "assistant",
				Content: []ContentBlock{
					{Type: "text", Text: "Hello"},
				},
			},
		},
	}

	req := &MessageRequest{
		Model:     "test-model",
		MaxTokens: 100,
		Messages: []Message{
			{Role: "user", Content: TextContent("Hi")},
		},
	}

	resp, err := sendWithRetry(context.Background(), mock, req, nil)
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if resp == nil {
		t.Fatal("expected response, got nil")
	}

	// Should be called exactly once (no provider-level retry)
	if mock.sendMessageCalls != 1 {
		t.Errorf("SendMessage called %d times, want 1 (should skip provider retry)", mock.sendMessageCalls)
	}
}
