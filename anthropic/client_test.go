package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendMessageSuccess(t *testing.T) {
	var receivedReq *MessageRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req MessageRequest
		json.NewDecoder(r.Body).Decode(&req)
		receivedReq = &req

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(MessageResponse{
			ID:         "msg_test123",
			Type:       "message",
			Role:       "assistant",
			Content:    TextContent("Hello from API"),
			StopReason: "end_turn",
			Usage:      Usage{InputTokens: 100, OutputTokens: 20},
		})
	}))
	defer server.Close()

	client := NewClientWithBase(server.URL, "sk-ant-oat01-test-token")

	resp, err := client.SendMessage(context.Background(), &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 1024,
		System: []SystemBlock{
			{Type: "text", Text: "You are a helper."},
		},
		Messages: []Message{
			{Role: "user", Content: TextContent("Hi there")},
		},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Verify response
	if resp.ID != "msg_test123" {
		t.Errorf("resp.ID = %q", resp.ID)
	}
	if TextOf(resp.Content) != "Hello from API" {
		t.Errorf("resp text = %q", TextOf(resp.Content))
	}
	if resp.Usage.InputTokens != 100 {
		t.Errorf("input tokens = %d", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 20 {
		t.Errorf("output tokens = %d", resp.Usage.OutputTokens)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", resp.StopReason)
	}

	// Verify request body
	if receivedReq == nil {
		t.Fatal("no request received")
	}
	if receivedReq.Model != "claude-haiku-4-5" {
		t.Errorf("model = %q", receivedReq.Model)
	}
	if receivedReq.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d", receivedReq.MaxTokens)
	}
	if len(receivedReq.Messages) != 1 {
		t.Fatalf("messages len = %d", len(receivedReq.Messages))
	}
	if receivedReq.Messages[0].Role != "user" {
		t.Errorf("message role = %q", receivedReq.Messages[0].Role)
	}
}

func TestSendMessageHeaders(t *testing.T) {
	var receivedHeaders http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(MessageResponse{
			ID:         "msg_hdr",
			Content:    TextContent("ok"),
			StopReason: "end_turn",
		})
	}))
	defer server.Close()

	client := NewClientWithBase(server.URL, "test-api-key")
	client.SendMessage(context.Background(), &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 256,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	})

	// Content-Type
	if ct := receivedHeaders.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}

	// Authorization (Bearer, not x-api-key)
	auth := receivedHeaders.Get("Authorization")
	if auth != "Bearer test-api-key" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer test-api-key")
	}

	// anthropic-version
	if v := receivedHeaders.Get("anthropic-version"); v != "2023-06-01" {
		t.Errorf("anthropic-version = %q", v)
	}

	// anthropic-beta (both prompt caching and oauth)
	beta := receivedHeaders.Get("anthropic-beta")
	if !strings.Contains(beta, "prompt-caching-2024-07-31") {
		t.Errorf("anthropic-beta missing prompt-caching: %q", beta)
	}
	if !strings.Contains(beta, "oauth-2025-04-20") {
		t.Errorf("anthropic-beta missing oauth: %q", beta)
	}
}

func TestSendMessageAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"max_tokens too large"}}`))
	}))
	defer server.Close()

	client := NewClientWithBase(server.URL, "test-key")

	_, err := client.SendMessage(context.Background(), &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 999999,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	})

	if err == nil {
		t.Fatal("expected error for 400 status")
	}
	if !strings.Contains(err.Error(), "API error (status 400)") {
		t.Errorf("error = %q, want API error with status", err.Error())
	}
	if !strings.Contains(err.Error(), "max_tokens") {
		t.Errorf("error = %q, want body content", err.Error())
	}
}

func TestSendMessageInvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not valid json{{{"))
	}))
	defer server.Close()

	client := NewClientWithBase(server.URL, "test-key")

	_, err := client.SendMessage(context.Background(), &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 256,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	})

	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
	if !strings.Contains(err.Error(), "unmarshal response") {
		t.Errorf("error = %q, want 'unmarshal response'", err.Error())
	}
}

func TestNewClientDefaults(t *testing.T) {
	client := NewClient("my-key")
	if client.baseURL != "https://api.anthropic.com" {
		t.Errorf("baseURL = %q", client.baseURL)
	}
	if client.apiKey != "my-key" {
		t.Errorf("apiKey = %q", client.apiKey)
	}
}

func TestNewClientWithBase(t *testing.T) {
	client := NewClientWithBase("http://localhost:8080", "test-key")
	if client.baseURL != "http://localhost:8080" {
		t.Errorf("baseURL = %q", client.baseURL)
	}
}
