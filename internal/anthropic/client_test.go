package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendMessageSuccess(t *testing.T) {
	// Proves that SendMessage correctly serializes the request, sends it to the endpoint, deserializes the response, and surfaces all response fields (ID, content, usage, stop_reason).
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

	client := newTestClientWithBase(server.URL, "sk-ant-oat01-test-token")

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
	// Proves that SendMessage sends the correct HTTP headers: Bearer authorization, anthropic-version, Content-Type, and the oauth beta flag (but not the now-GA prompt-caching flag).
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

	client := newTestClientWithBase(server.URL, "test-api-key")
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

	// anthropic-beta (oauth only — prompt caching is GA)
	beta := receivedHeaders.Get("anthropic-beta")
	if !strings.Contains(beta, "oauth-2025-04-20") {
		t.Errorf("anthropic-beta missing oauth: %q", beta)
	}
	if strings.Contains(beta, "prompt-caching") {
		t.Errorf("anthropic-beta should not contain prompt-caching (GA): %q", beta)
	}
}

func TestSendMessageAPIError(t *testing.T) {
	// Proves that a 4xx HTTP response is surfaced as an *APIError with the correct status code and that IsRateLimit() returns false for non-429 errors.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"max_tokens too large"}}`))
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-key")

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

	// Should be an APIError
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatal("expected *APIError")
	}
	if apiErr.StatusCode != 400 {
		t.Errorf("StatusCode = %d, want 400", apiErr.StatusCode)
	}
	if apiErr.IsRateLimit() {
		t.Error("400 should not be rate limit")
	}
}

func TestSendMessageRateLimit(t *testing.T) {
	// Proves that a 429 response is surfaced as an *APIError with IsRateLimit() true and that the Retry-After header value is correctly parsed and exposed via RetryAfterSeconds().
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"rate limited"}}`))
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-key")

	_, err := client.SendMessage(context.Background(), &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 256,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	})

	if err == nil {
		t.Fatal("expected error for 429 status")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatal("expected *APIError")
	}
	if !apiErr.IsRateLimit() {
		t.Error("expected IsRateLimit() == true")
	}
	if apiErr.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", apiErr.StatusCode)
	}
	if apiErr.RetryAfter != "30" {
		t.Errorf("RetryAfter = %q, want 30", apiErr.RetryAfter)
	}
	if apiErr.RetryAfterSeconds() != 30 {
		t.Errorf("RetryAfterSeconds = %d, want 30", apiErr.RetryAfterSeconds())
	}
}


func TestSignalRecoveryNoOp(t *testing.T) {
	// Proves that signalRecovery is safe to call when no recovery channel has been configured — it should be a no-op that does not panic.
	client := NewClient(StaticToken("test-key"), 60*time.Second)
	client.signalRecovery() // no-op, no panic
}


func TestSendMessageInvalidJSON(t *testing.T) {
	// Proves that a 200 response with malformed JSON body returns a descriptive unmarshal error rather than silently returning a zero-value response.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not valid json{{{"))
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-key")

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

func TestCountTokensSuccess(t *testing.T) {
	// Proves that CountTokens sends to the correct endpoint (/v1/messages/count_tokens), properly serializes the request, and returns the token count from the response.
	var receivedPath string
	var receivedReq *MessageRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		var req MessageRequest
		json.NewDecoder(r.Body).Decode(&req)
		receivedReq = &req

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CountTokensResponse{InputTokens: 4523})
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-key")

	count, err := client.CountTokens(context.Background(), &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 1024,
		System:    []SystemBlock{{Type: "text", Text: "You are helpful."}},
		Messages:  []Message{{Role: "user", Content: TextContent("Hello")}},
	})
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if count != 4523 {
		t.Errorf("count = %d, want 4523", count)
	}
	if receivedPath != "/v1/messages/count_tokens" {
		t.Errorf("path = %q, want /v1/messages/count_tokens", receivedPath)
	}
	if receivedReq == nil {
		t.Fatal("no request received")
	}
	if receivedReq.Model != "claude-haiku-4-5" {
		t.Errorf("model = %q", receivedReq.Model)
	}
}

func TestCountTokensAPIError(t *testing.T) {
	// Proves that CountTokens propagates API errors correctly, returning an *APIError with the right status code on a 400 response.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"bad request"}}`))
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-key")

	_, err := client.CountTokens(context.Background(), &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 1024,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	})
	if err == nil {
		t.Fatal("expected error for 400 status")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatal("expected *APIError")
	}
	if apiErr.StatusCode != 400 {
		t.Errorf("StatusCode = %d, want 400", apiErr.StatusCode)
	}
}

func TestCountTokensInvalidJSON(t *testing.T) {
	// Proves that CountTokens returns a descriptive unmarshal error when the server responds with malformed JSON.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not json{{{"))
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-key")

	_, err := client.CountTokens(context.Background(), &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 1024,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	})
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "unmarshal response") {
		t.Errorf("error = %q, want 'unmarshal response'", err.Error())
	}
}

func TestNewClientDefaults(t *testing.T) {
	// Proves that NewClient sets the production Anthropic API base URL and SDK enabled.
	client := NewClient(StaticToken("my-key"), 60*time.Second)
	if client.baseURL != "https://api.anthropic.com" {
		t.Errorf("baseURL = %q", client.baseURL)
	}
	if !client.useSDK {
		t.Error("useSDK should default to true")
	}
}

func TestSetBaseURL(t *testing.T) {
	// Proves that SetBaseURL overrides the base URL, enabling tests to point the client at a local mock server.
	client := NewClient(StaticToken("test-key"), 60*time.Second)
	client.SetBaseURL("http://localhost:8080")
	if client.baseURL != "http://localhost:8080" {
		t.Errorf("baseURL = %q", client.baseURL)
	}
}

