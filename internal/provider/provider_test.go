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
	resp, err := Send(context.Background(), mock, req, nil)
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
	resp, err := Send(context.Background(), mock, req, handler)
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
	resp, err := Send(context.Background(), mock, req, handler)
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

	resp, err := Send(context.Background(), mock, req, handler)
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
	// Verify that clients implementing HandlesOwnRetries() skip provider-level retry
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

	resp, err := Send(context.Background(), mock, req, nil)
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
