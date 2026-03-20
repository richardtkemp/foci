package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"foci/internal/provider"
)

func TestStreamMessageSSESuccess(t *testing.T) {
	// Proves that StreamMessage correctly reassembles text deltas from a sequence
	// of SSE chunks, invokes OnTextDelta for each delta, and produces a complete
	// MessageResponse with the right ID, stop reason, usage, and concatenated text.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("server doesn't support flushing")
		}

		chunks := []string{
			`data: {"id":"chatcmpl-stream1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-stream1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-stream1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-stream1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"id":"chatcmpl-stream1","object":"chat.completion.chunk","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
			`data: [DONE]`,
		}

		for _, chunk := range chunks {
			fmt.Fprintf(w, "%s\n\n", chunk)
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := NewClient("test-key", WithBaseURL(srv.URL))

	var textDeltas []string
	handler := &provider.StreamHandler{
		OnTextDelta: func(delta string) {
			textDeltas = append(textDeltas, delta)
		},
	}

	resp, err := c.StreamMessage(context.Background(), &provider.MessageRequest{
		Model:     "gpt-4o",
		MaxTokens: 256,
		Messages: []provider.Message{
			{Role: "user", Content: provider.TextContent("hi")},
		},
	}, handler)

	if err != nil {
		t.Fatalf("StreamMessage: %v", err)
	}

	if resp.ID != "chatcmpl-stream1" {
		t.Errorf("resp.ID = %q, want chatcmpl-stream1", resp.ID)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 {
		t.Errorf("input_tokens = %d, want 10", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 5 {
		t.Errorf("output_tokens = %d, want 5", resp.Usage.OutputTokens)
	}

	fullText := provider.TextOf(resp.Content)
	if fullText != "Hello world" {
		t.Errorf("response text = %q, want 'Hello world'", fullText)
	}

	if len(textDeltas) != 2 {
		t.Fatalf("text deltas = %d, want 2", len(textDeltas))
	}
	if textDeltas[0] != "Hello" {
		t.Errorf("delta[0] = %q, want 'Hello'", textDeltas[0])
	}
	if textDeltas[1] != " world" {
		t.Errorf("delta[1] = %q, want ' world'", textDeltas[1])
	}
}

func TestStreamMessageSSEWithToolCalls(t *testing.T) {
	// Proves that tool call deltas are correctly accumulated across multiple chunks
	// into a complete tool_use block with the right ID, name, and arguments, and
	// that the stop reason is set to "tool_use".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		chunks := []string{
			// Tool call start with name
			`data: {"id":"chatcmpl-tc","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_abc123","type":"function","function":{"name":"exec","arguments":""}}]},"finish_reason":null}]}`,
			// Arguments fragment 1
			`data: {"id":"chatcmpl-tc","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
			// Arguments fragment 2
			`data: {"id":"chatcmpl-tc","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls -la\"}"}}]},"finish_reason":null}]}`,
			// Finish
			`data: {"id":"chatcmpl-tc","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			// Usage
			`data: {"id":"chatcmpl-tc","object":"chat.completion.chunk","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":15,"total_tokens":65}}`,
			`data: [DONE]`,
		}

		for _, chunk := range chunks {
			fmt.Fprintf(w, "%s\n\n", chunk)
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := NewClient("test-key", WithBaseURL(srv.URL))

	resp, err := c.StreamMessage(context.Background(), &provider.MessageRequest{
		Model:     "gpt-4o",
		MaxTokens: 1024,
		Messages: []provider.Message{
			{Role: "user", Content: provider.TextContent("list files")},
		},
		Tools: []provider.ToolDef{
			provider.NewCustomTool("exec", "run commands", json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)),
		},
	}, nil)

	if err != nil {
		t.Fatalf("StreamMessage: %v", err)
	}

	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", resp.StopReason)
	}

	// Find the tool_use block.
	var toolBlock *provider.ContentBlock
	for i := range resp.Content {
		if resp.Content[i].Type == "tool_use" {
			toolBlock = &resp.Content[i]
			break
		}
	}
	if toolBlock == nil {
		t.Fatal("no tool_use block found")
	}
	if toolBlock.Name != "exec" {
		t.Errorf("tool name = %q, want exec", toolBlock.Name)
	}
	if toolBlock.ID != "call_abc123" {
		t.Errorf("tool id = %q, want call_abc123", toolBlock.ID)
	}

	var args map[string]string
	if err := json.Unmarshal(toolBlock.Input, &args); err != nil {
		t.Fatalf("parse args: %v", err)
	}
	if args["command"] != "ls -la" {
		t.Errorf("args = %v, want {command: ls -la}", args)
	}
}

func TestStreamMessageSSEWithReasoning(t *testing.T) {
	// Proves that OpenRouter reasoning_content extra fields on chunk deltas are
	// correctly accumulated: OnThinkingDelta is fired for each reasoning delta,
	// OnTextDelta for text deltas, and the final response contains a thinking
	// block followed by a text block.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		chunks := []string{
			`data: {"id":"chatcmpl-reason","object":"chat.completion.chunk","model":"openrouter/model","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Let me think"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-reason","object":"chat.completion.chunk","model":"openrouter/model","choices":[{"index":0,"delta":{"reasoning_content":" about this"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-reason","object":"chat.completion.chunk","model":"openrouter/model","choices":[{"index":0,"delta":{"content":"The answer"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-reason","object":"chat.completion.chunk","model":"openrouter/model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"id":"chatcmpl-reason","object":"chat.completion.chunk","model":"openrouter/model","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}}`,
			`data: [DONE]`,
		}

		for _, chunk := range chunks {
			fmt.Fprintf(w, "%s\n\n", chunk)
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := NewClient("test-key", WithBaseURL(srv.URL))

	var thinkingDeltas []string
	var textDeltas []string
	handler := &provider.StreamHandler{
		OnTextDelta: func(delta string) {
			textDeltas = append(textDeltas, delta)
		},
		OnThinkingDelta: func(delta string) {
			thinkingDeltas = append(thinkingDeltas, delta)
		},
	}

	resp, err := c.StreamMessage(context.Background(), &provider.MessageRequest{
		Model:     "openrouter/model",
		MaxTokens: 256,
		Messages: []provider.Message{
			{Role: "user", Content: provider.TextContent("think about this")},
		},
		Thinking: &provider.ThinkingConfig{Type: "adaptive"},
	}, handler)

	if err != nil {
		t.Fatalf("StreamMessage: %v", err)
	}

	// Should have thinking block + text block.
	if len(resp.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(resp.Content))
	}
	if resp.Content[0].Type != "thinking" {
		t.Errorf("content[0].type = %q, want thinking", resp.Content[0].Type)
	}
	if resp.Content[0].Thinking != "Let me think about this" {
		t.Errorf("thinking = %q, want 'Let me think about this'", resp.Content[0].Thinking)
	}
	// ReasoningRaw should be set for faithful round-trip.
	if len(resp.Content[0].ReasoningRaw) == 0 {
		t.Error("expected ReasoningRaw to be set")
	}

	if resp.Content[1].Type != "text" {
		t.Errorf("content[1].type = %q, want text", resp.Content[1].Type)
	}
	if resp.Content[1].Text != "The answer" {
		t.Errorf("text = %q, want 'The answer'", resp.Content[1].Text)
	}

	if len(thinkingDeltas) != 2 {
		t.Fatalf("thinking deltas = %d, want 2", len(thinkingDeltas))
	}
	if thinkingDeltas[0] != "Let me think" {
		t.Errorf("thinking delta[0] = %q, want 'Let me think'", thinkingDeltas[0])
	}
	if thinkingDeltas[1] != " about this" {
		t.Errorf("thinking delta[1] = %q, want ' about this'", thinkingDeltas[1])
	}

	if len(textDeltas) != 1 || textDeltas[0] != "The answer" {
		t.Errorf("text deltas = %v, want [The answer]", textDeltas)
	}
}

func TestStreamMessageNilHandler(t *testing.T) {
	// Proves that StreamMessage completes successfully when passed a nil handler,
	// allowing callers that only want the final response to omit the handler.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		chunks := []string{
			`data: {"id":"chatcmpl-nil","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-nil","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"id":"chatcmpl-nil","object":"chat.completion.chunk","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`,
			`data: [DONE]`,
		}
		for _, chunk := range chunks {
			fmt.Fprintf(w, "%s\n\n", chunk)
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := NewClient("test-key", WithBaseURL(srv.URL))

	resp, err := c.StreamMessage(context.Background(), &provider.MessageRequest{
		Model:     "gpt-4o",
		MaxTokens: 256,
		Messages: []provider.Message{
			{Role: "user", Content: provider.TextContent("hi")},
		},
	}, nil)

	if err != nil {
		t.Fatalf("StreamMessage with nil handler: %v", err)
	}
	if resp.ID != "chatcmpl-nil" {
		t.Errorf("resp.ID = %q, want chatcmpl-nil", resp.ID)
	}
	fullText := provider.TextOf(resp.Content)
	if fullText != "hello" {
		t.Errorf("text = %q, want hello", fullText)
	}
}

func TestStreamMessagePreStreamError(t *testing.T) {
	// Proves that a 429 error returned before any streaming data is correctly
	// classified as a provider.APIError with IsRateLimit() == true.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "Rate limit exceeded",
				"type":    "rate_limit_error",
				"code":    "rate_limit_exceeded",
			},
		})
	}))
	defer srv.Close()

	c := NewClient("test-key", WithBaseURL(srv.URL))

	_, err := c.StreamMessage(context.Background(), &provider.MessageRequest{
		Model:     "gpt-4o",
		MaxTokens: 1024,
		Messages: []provider.Message{
			{Role: "user", Content: provider.TextContent("hello")},
		},
	}, nil)

	if err == nil {
		t.Fatal("expected error")
	}

	var apiErr *provider.APIError
	if ok := isAPIError(err, &apiErr); !ok {
		t.Fatalf("expected provider.APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 429 {
		t.Errorf("status = %d, want 429", apiErr.StatusCode)
	}
	if !apiErr.IsRateLimit() {
		t.Error("expected IsRateLimit to return true")
	}
}

func TestStreamingClientInterface(t *testing.T) {
	// Proves that *Client satisfies the provider.StreamingClient interface at
	// runtime (compile-time assertion exists in stream.go; this documents the
	// contract explicitly in the test suite).
	var _ provider.StreamingClient = (*Client)(nil)
}
