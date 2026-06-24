package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient creates a Client pointed at an httptest server with a static token.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewClient(func() (string, error) { return "test-key", nil }, 10*time.Second)
	client.SetBaseURL(server.URL)
	return client
}

func TestSignalRecoveryNoOp(t *testing.T) {
	// Proves that signalRecovery is safe to call when no recovery channel has been configured — it should be a no-op that does not panic.
	client := NewClient(func() (string, error) { return "test-key", nil }, 60*time.Second)
	client.signalRecovery() // no-op, no panic
}

func TestNewClientDefaults(t *testing.T) {
	// Proves that NewClient sets the production Anthropic API base URL.
	client := NewClient(func() (string, error) { return "my-key", nil }, 60*time.Second)
	if client.baseURL != "https://api.anthropic.com" {
		t.Errorf("baseURL = %q", client.baseURL)
	}
}

func TestSetBaseURL(t *testing.T) {
	// Proves that SetBaseURL overrides the base URL, enabling tests to point the client at a local mock server.
	client := NewClient(func() (string, error) { return "test-key", nil }, 60*time.Second)
	client.SetBaseURL("http://localhost:8080")
	if client.baseURL != "http://localhost:8080" {
		t.Errorf("baseURL = %q", client.baseURL)
	}
}

func TestSendMessageSuccess(t *testing.T) {
	// Proves SendMessage translates the response wire format (text + tool_use blocks, usage with cache fields) into a MessageResponse and records the wire request for debugging.
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q, want /v1/messages", r.URL.Path)
		}
		if beta := r.Header.Get("anthropic-beta"); !strings.Contains(beta, "oauth-2025-04-20") {
			t.Errorf("anthropic-beta = %q, want oauth beta header", beta)
		}
		// Server tools must arrive as JSON objects (raw passthrough, not
		// double-encoded strings).
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		tools, ok := body["tools"].([]any)
		if !ok || len(tools) != 1 {
			t.Fatalf("tools = %v, want 1 entry", body["tools"])
		}
		if st, ok := tools[0].(map[string]any); !ok || st["type"] != "web_search_20250305" {
			t.Errorf("tools[0] = %v, want server tool object", tools[0])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id": "msg_01", "type": "message", "role": "assistant",
			"model": "claude-haiku-4-5", "stop_reason": "tool_use",
			"content": [
				{"type": "text", "text": "Let me check."},
				{"type": "tool_use", "id": "tu_1", "name": "get_weather", "input": {"city": "London"}}
			],
			"usage": {"input_tokens": 100, "output_tokens": 20, "cache_creation_input_tokens": 50, "cache_read_input_tokens": 200}
		}`)
	})

	resp, err := client.SendMessage(context.Background(), &MessageRequest{
		Model: "anthropic/claude-haiku-4-5", MaxTokens: 256,
		Messages: []Message{{Role: "user", Content: TextContent("weather?")}},
		Tools:    []ToolDef{NewServerTool(map[string]interface{}{"type": "web_search_20250305", "name": "web_search"})},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if resp.ID != "msg_01" || resp.StopReason != "tool_use" {
		t.Errorf("id/stop_reason = %q/%q", resp.ID, resp.StopReason)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(resp.Content))
	}
	if resp.Content[0].Type != "text" || resp.Content[0].Text != "Let me check." {
		t.Errorf("content[0] = %+v", resp.Content[0])
	}
	tu := resp.Content[1]
	if tu.Type != "tool_use" || tu.ID != "tu_1" || tu.Name != "get_weather" {
		t.Errorf("content[1] = %+v", tu)
	}
	var input map[string]string
	if err := json.Unmarshal(tu.Input, &input); err != nil || input["city"] != "London" {
		t.Errorf("tool input = %s (err %v)", tu.Input, err)
	}
	u := resp.Usage
	if u.InputTokens != 100 || u.OutputTokens != 20 || u.CacheCreationInputTokens != 50 || u.CacheReadInputTokens != 200 {
		t.Errorf("usage = %+v", u)
	}
	if len(resp.WireRequest) == 0 || !strings.Contains(string(resp.WireRequest), "claude-haiku-4-5") {
		t.Errorf("WireRequest = %s, want marshaled request with stripped model", resp.WireRequest)
	}
}

func TestSendMessageAPIError(t *testing.T) {
	// Proves an HTTP error response is classified into an APIError carrying the status code, Retry-After header, and the wire request that triggered it.
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	})

	_, err := client.SendMessage(context.Background(), &MessageRequest{
		Model: "claude-haiku-4-5", MaxTokens: 64,
		Messages: []Message{{Role: "user", Content: TextContent("hi")}},
	})
	if err == nil {
		t.Fatal("expected error")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", apiErr.StatusCode)
	}
	if apiErr.RetryAfter != "7" {
		t.Errorf("RetryAfter = %q, want 7", apiErr.RetryAfter)
	}
	if len(apiErr.WireRequest) == 0 {
		t.Error("WireRequest not attached to APIError")
	}
}

func TestSendMessageTokenError(t *testing.T) {
	// Proves a token resolution failure aborts the request before any HTTP call and surfaces the underlying error.
	called := false
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	client.tokenFunc = func() (string, error) { return "", fmt.Errorf("no creds") }

	_, err := client.SendMessage(context.Background(), &MessageRequest{
		Model: "claude-haiku-4-5", MaxTokens: 64,
		Messages: []Message{{Role: "user", Content: TextContent("hi")}},
	})
	if err == nil || !strings.Contains(err.Error(), "no creds") {
		t.Fatalf("err = %v, want token error", err)
	}
	if called {
		t.Error("HTTP request was made despite token failure")
	}
}

func TestCountTokensSuccess(t *testing.T) {
	// Proves CountTokens posts to the count_tokens endpoint with system, tools, and adaptive thinking translated, and returns the reported token count.
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("path = %q, want /v1/messages/count_tokens", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if _, ok := body["system"]; !ok {
			t.Error("system blocks missing from count request")
		}
		// Both tool kinds must arrive as JSON objects on the wire (raw passthrough,
		// not double-encoded strings).
		tools, ok := body["tools"].([]any)
		if !ok || len(tools) != 2 {
			t.Fatalf("tools = %v, want 2 entries", body["tools"])
		}
		custom, ok := tools[0].(map[string]any)
		if !ok || custom["name"] != "get_weather" {
			t.Errorf("tools[0] = %v, want custom tool object", tools[0])
		}
		server, ok := tools[1].(map[string]any)
		if !ok || server["type"] != "web_search_20250305" {
			t.Errorf("tools[1] = %v, want server tool object", tools[1])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"input_tokens": 1234}`)
	})

	n, err := client.CountTokens(context.Background(), &MessageRequest{
		Model:    "claude-opus-4-6",
		Messages: []Message{{Role: "user", Content: TextContent("hi")}},
		System:   []SystemBlock{{Type: "text", Text: "be brief"}},
		Tools: []ToolDef{
			NewCustomTool("get_weather", "weather lookup", json.RawMessage(`{"type":"object"}`)),
			NewServerTool(map[string]interface{}{"type": "web_search_20250305", "name": "web_search"}),
		},
		Thinking: &ThinkingConfig{Type: "adaptive"},
	})
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if n != 1234 {
		t.Errorf("count = %d, want 1234", n)
	}
}

func TestCountTokensErrors(t *testing.T) {
	// Proves CountTokens surfaces token resolution failures and classifies API error responses into APIError.
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)
	})

	req := &MessageRequest{
		Model:    "claude-haiku-4-5",
		Messages: []Message{{Role: "user", Content: TextContent("hi")}},
	}

	_, err := client.CountTokens(context.Background(), req)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		t.Errorf("err = %v, want *APIError with status 400", err)
	}

	client.tokenFunc = func() (string, error) { return "", fmt.Errorf("no creds") }
	if _, err := client.CountTokens(context.Background(), req); err == nil || !strings.Contains(err.Error(), "no creds") {
		t.Errorf("err = %v, want token error", err)
	}
}

func TestEndpoint(t *testing.T) {
	// Proves Endpoint names the official API "Anthropic API" and derives a readable name from the host for other base URLs.
	tests := []struct {
		baseURL string
		want    string
	}{
		{"https://api.anthropic.com", "Anthropic API"},
		{"https://gateway.anthropic.com/v1", "Anthropic API"},
		{"https://llm.example.com", "Example API"},
	}
	for _, tt := range tests {
		client := NewClient(func() (string, error) { return "k", nil }, time.Second)
		client.SetBaseURL(tt.baseURL)
		if got := client.Endpoint(); got != tt.want {
			t.Errorf("Endpoint(%q) = %q, want %q", tt.baseURL, got, tt.want)
		}
	}
}

func TestIsCachingAvailable(t *testing.T) {
	// Proves the Anthropic client always reports prompt caching as available — the provider layer relies on this to enable cache markers.
	if !NewClient(func() (string, error) { return "k", nil }, time.Second).IsCachingAvailable() {
		t.Error("IsCachingAvailable = false, want true")
	}
}

func TestRetryDelayKnobs(t *testing.T) {
	// Proves the retry timing accessors return production defaults when unset, and scale proportionally in test mode (retryBaseDelay < 1s) so retry tests run fast.
	prod := NewClient(func() (string, error) { return "k", nil }, time.Second)
	if got := prod.RetryBaseDelay(); got != 2*time.Second {
		t.Errorf("RetryBaseDelay default = %v, want 2s", got)
	}
	if got := prod.OverloadBaseDelay(); got != 5*time.Second {
		t.Errorf("OverloadBaseDelay default = %v, want 5s", got)
	}
	if got := prod.OverloadMaxDuration(); got != 2*time.Hour {
		t.Errorf("OverloadMaxDuration default = %v, want 2h", got)
	}
	if got := prod.ServerErrorMaxDuration(); got != 5*time.Minute {
		t.Errorf("ServerErrorMaxDuration default = %v, want 5m", got)
	}

	fast := NewClient(func() (string, error) { return "k", nil }, time.Second)
	fast.SetRetryBaseDelay(2 * time.Millisecond)
	if got := fast.RetryBaseDelay(); got != 2*time.Millisecond {
		t.Errorf("RetryBaseDelay set = %v, want 2ms", got)
	}
	if got := fast.OverloadBaseDelay(); got != 10*time.Millisecond {
		t.Errorf("OverloadBaseDelay test mode = %v, want 10ms (base*5)", got)
	}
	if got := fast.OverloadMaxDuration(); got != time.Second {
		t.Errorf("OverloadMaxDuration test mode = %v, want 1s (base*500)", got)
	}
	if got := fast.ServerErrorMaxDuration(); got != 200*time.Millisecond {
		t.Errorf("ServerErrorMaxDuration test mode = %v, want 200ms (base*100)", got)
	}
}

func TestWaitForRecoverySignaling(t *testing.T) {
	// Proves WaitForRecovery hands out a shared channel that OnRetrySuccess closes, waking overload waiters; a second wait gets a fresh, still-open channel.
	client := NewClient(func() (string, error) { return "k", nil }, time.Second)

	ch1 := client.WaitForRecovery()
	ch2 := client.WaitForRecovery()
	select {
	case <-ch1:
		t.Fatal("recovery channel closed before OnRetrySuccess")
	default:
	}

	client.OnRetrySuccess()
	for _, ch := range []<-chan struct{}{ch1, ch2} {
		select {
		case <-ch:
		default:
			t.Fatal("recovery channel not closed after OnRetrySuccess")
		}
	}

	// A fresh wait after recovery must get a new open channel.
	ch3 := client.WaitForRecovery()
	select {
	case <-ch3:
		t.Fatal("fresh recovery channel already closed")
	default:
	}
}
