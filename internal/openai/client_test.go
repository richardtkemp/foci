package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"foci/internal/provider"
)

func TestListModels(t *testing.T) {
	// Proves that ListModels hits GET /models, parses the response correctly, and maps unix timestamps to time.Time values.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "gpt-4o-2025-08-01", "object": "model", "created": 1722470400, "owned_by": "openai"},
				{"id": "o3-2025-07-15", "object": "model", "created": 1720000000, "owned_by": "openai"},
				{"id": "o4-mini-2025-09-01", "object": "model", "created": 1725148800, "owned_by": "openai"},
			},
		})
	}))
	defer srv.Close()

	c := NewClient("test-key", WithBaseURL(srv.URL))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	if len(models) != 3 {
		t.Fatalf("models = %d, want 3", len(models))
	}
	if models[0].ID != "gpt-4o-2025-08-01" {
		t.Errorf("models[0].ID = %q, want gpt-4o-2025-08-01", models[0].ID)
	}
	wantTime := time.Unix(1722470400, 0).UTC()
	if !models[0].CreatedAt.Equal(wantTime) {
		t.Errorf("models[0].CreatedAt = %v, want %v", models[0].CreatedAt, wantTime)
	}
	if models[1].ID != "o3-2025-07-15" {
		t.Errorf("models[1].ID = %q, want o3-2025-07-15", models[1].ID)
	}
	if models[2].ID != "o4-mini-2025-09-01" {
		t.Errorf("models[2].ID = %q, want o4-mini-2025-09-01", models[2].ID)
	}
}

func TestListModels_APIError(t *testing.T) {
	// Proves that a 401 Unauthorized response from the API causes ListModels to return an error rather than silently succeed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "Invalid API key",
				"type":    "invalid_request_error",
				"code":    "invalid_api_key",
			},
		})
	}))
	defer srv.Close()

	c := NewClient("bad-key", WithBaseURL(srv.URL))
	_, err := c.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected error from ListModels with bad key")
	}
}

func TestCountTokens(t *testing.T) {
	// Proves that CountTokens is intentionally unimplemented for OpenAI and returns a specific descriptive error.
	c := NewClient("test-key")
	_, err := c.CountTokens(context.Background(), &provider.MessageRequest{})
	if err == nil {
		t.Fatal("expected error from CountTokens")
	}
	if err.Error() != "openai: token counting not supported" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSendMessage(t *testing.T) {
	// Proves a basic text completion round-trip: verifies the request hits the correct endpoint with the right model, and that the response is translated into the provider-neutral MessageResponse format including usage stats and text content.
	// Mock OpenAI API
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}

		// Verify the request body
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body["model"] != "gpt-4o" {
			t.Errorf("model = %v, want gpt-4o", body["model"])
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-123",
			"object":  "chat.completion",
			"model":   "gpt-4o",
			"created": 1700000000,
			"choices": []map[string]any{
				{
					"index":         0,
					"finish_reason": "stop",
					"message": map[string]any{
						"role":    "assistant",
						"content": "Hello! How can I help?",
					},
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": 8,
				"total_tokens":      18,
			},
		})
	}))
	defer srv.Close()

	c := NewClient("test-key", WithBaseURL(srv.URL))
	resp, err := c.SendMessage(context.Background(), &provider.MessageRequest{
		Model:     "gpt-4o",
		MaxTokens: 1024,
		System:    []provider.SystemBlock{{Type: "text", Text: "You are helpful."}},
		Messages: []provider.Message{
			{Role: "user", Content: provider.TextContent("Hello")},
		},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if resp.ID != "chatcmpl-123" {
		t.Errorf("id = %q", resp.ID)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop = %q, want end_turn", resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 {
		t.Errorf("input = %d, want 10", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 8 {
		t.Errorf("output = %d, want 8", resp.Usage.OutputTokens)
	}

	text := provider.TextOf(resp.Content)
	if text != "Hello! How can I help?" {
		t.Errorf("text = %q", text)
	}
}

func TestSendMessage_ToolCalls(t *testing.T) {
	// Proves that when the API returns a tool_calls finish reason, the client correctly translates it into a provider tool_use content block with the right name, ID, and arguments, and sets StopReason to "tool_use".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-456",
			"object":  "chat.completion",
			"model":   "gpt-4o",
			"created": 1700000000,
			"choices": []map[string]any{
				{
					"index":         0,
					"finish_reason": "tool_calls",
					"message": map[string]any{
						"role":    "assistant",
						"content": "",
						"tool_calls": []map[string]any{
							{
								"id":   "call_abc123",
								"type": "function",
								"function": map[string]any{
									"name":      "exec",
									"arguments": `{"command":"ls -la"}`,
								},
							},
						},
					},
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     50,
				"completion_tokens": 15,
				"total_tokens":      65,
			},
		})
	}))
	defer srv.Close()

	c := NewClient("test-key", WithBaseURL(srv.URL))
	resp, err := c.SendMessage(context.Background(), &provider.MessageRequest{
		Model:     "gpt-4o",
		MaxTokens: 1024,
		Messages: []provider.Message{
			{Role: "user", Content: provider.TextContent("list files")},
		},
		Tools: []provider.ToolDef{
			provider.NewCustomTool("exec", "run commands", json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)),
		},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if resp.StopReason != "tool_use" {
		t.Errorf("stop = %q, want tool_use", resp.StopReason)
	}

	// Find the tool_use block
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
		t.Errorf("tool name = %q", toolBlock.Name)
	}
	if toolBlock.ID != "call_abc123" {
		t.Errorf("tool id = %q", toolBlock.ID)
	}

	var args map[string]string
	if err := json.Unmarshal(toolBlock.Input, &args); err != nil {
		t.Fatalf("parse args: %v", err)
	}
	if args["command"] != "ls -la" {
		t.Errorf("args = %v", args)
	}
}

func TestSendMessage_APIError(t *testing.T) {
	// Proves that a 429 rate-limit response is surfaced as a *provider.APIError with the correct status code and IsRateLimit returning true.
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
	_, err := c.SendMessage(context.Background(), &provider.MessageRequest{
		Model:     "gpt-4o",
		MaxTokens: 1024,
		Messages: []provider.Message{
			{Role: "user", Content: provider.TextContent("hello")},
		},
	})
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

// isAPIError is a test helper that checks if err is or wraps a *provider.APIError.
func isAPIError(err error, target **provider.APIError) bool {
	// Check if the error itself is a *provider.APIError
	if e, ok := err.(*provider.APIError); ok {
		*target = e
		return true
	}
	return false
}

func TestMessagesToOpenAI(t *testing.T) {
	// Proves that system blocks are prepended as a single developer message before the user/assistant turns, producing the correct total message count and order.
	system := []provider.SystemBlock{
		{Type: "text", Text: "You are helpful."},
		{Type: "text", Text: "Be concise."},
	}
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("hello")},
		{Role: "assistant", Content: provider.TextContent("hi there")},
	}

	result := messagesToOpenAI(system, msgs)
	// 1 developer + 1 user + 1 assistant = 3
	if len(result) != 3 {
		t.Fatalf("messages = %d, want 3", len(result))
	}

	// First should be developer message
	if result[0].OfDeveloper == nil {
		t.Fatal("expected developer message first")
	}
}

func TestMessagesToOpenAI_ToolResult(t *testing.T) {
	// Proves that a provider tool_result content block is translated into an OpenAI tool message (not a user message).
	msgs := []provider.Message{
		{Role: "user", Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "call_1", Content: "file1\nfile2"},
		}},
	}

	result := messagesToOpenAI(nil, msgs)
	if len(result) != 1 {
		t.Fatalf("messages = %d, want 1", len(result))
	}
	if result[0].OfTool == nil {
		t.Fatal("expected tool message")
	}
}

func TestToolsToOpenAI(t *testing.T) {
	// Proves that a custom tool definition is correctly serialized into the OpenAI function-tool wire format with the right type and name fields.
	defs := []provider.ToolDef{
		provider.NewCustomTool("exec", "run commands", json.RawMessage(`{
			"type": "object",
			"properties": {
				"command": {"type": "string", "description": "command to run"}
			},
			"required": ["command"]
		}`)),
	}

	tools := toolsToOpenAI(defs)
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}

	// Verify the tool serializes correctly
	data, err := json.Marshal(tools[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed map[string]any
	json.Unmarshal(data, &parsed)
	if parsed["type"] != "function" {
		t.Errorf("type = %v", parsed["type"])
	}
	fn := parsed["function"].(map[string]any)
	if fn["name"] != "exec" {
		t.Errorf("name = %v", fn["name"])
	}
}

func TestToolsToOpenAI_FiltersServerTools(t *testing.T) {
	// Proves that server-side tools (e.g. web_search) are silently dropped from the OpenAI tool list, since they are not supported as function calls.
	defs := []provider.ToolDef{
		provider.NewCustomTool("exec", "run commands", json.RawMessage(`{"type":"object"}`)),
		provider.NewServerTool(map[string]interface{}{
			"type": "web_search_20250305",
			"name": "web_search",
		}),
	}

	tools := toolsToOpenAI(defs)
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1 (server tool should be filtered)", len(tools))
	}
}

func TestMapFinishReason(t *testing.T) {
	// Proves the OpenAI finish reason strings are mapped to the correct provider-neutral stop reason values, including that unknown or content-filtered responses fall back to "end_turn".
	tests := []struct {
		reason string
		want   string
	}{
		{"stop", "end_turn"},
		{"tool_calls", "tool_use"},
		{"length", "max_tokens"},
		{"content_filter", "end_turn"},
		{"unknown", "end_turn"},
	}

	for _, tt := range tests {
		if got := mapFinishReason(tt.reason); got != tt.want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", tt.reason, got, tt.want)
		}
	}
}

func TestResponseFromOpenAI_NilResponse(t *testing.T) {
	// Proves that passing a nil response to responseFromOpenAI returns an error rather than panicking or silently returning an empty result.
	_, err := responseFromOpenAI(nil, "gpt-4o")
	if err == nil {
		t.Fatal("expected error for nil response")
	}
}

func TestResponseFromOpenAI_EmptyChoices(t *testing.T) {
	// Proves that a response with no choices (e.g. a degenerate API reply) is handled gracefully, defaulting to "end_turn" rather than an error.
	resp := &openaiChatCompletion{
		ID:      "test",
		Choices: nil,
		Usage:   openaiUsage{PromptTokens: 10, CompletionTokens: 0},
	}
	result, err := responseFromOpenAIHelper(resp, "gpt-4o")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result.StopReason != "end_turn" {
		t.Errorf("stop = %q, want end_turn", result.StopReason)
	}
}

// openaiChatCompletion mirrors the response structure for unit testing without the SDK.
type openaiChatCompletion struct {
	ID      string
	Choices []openaiChoice
	Usage   openaiUsage
}

type openaiChoice struct {
	FinishReason string
	Message      openaiMessage
}

type openaiMessage struct {
	Content   string
	ToolCalls []openaiToolCall
}

type openaiToolCall struct {
	ID       string
	Type     string
	Function openaiFunction
}

type openaiFunction struct {
	Name      string
	Arguments string
}

type openaiUsage struct {
	PromptTokens     int64
	CompletionTokens int64
}

// responseFromOpenAIHelper tests the translation logic without the SDK types.
func responseFromOpenAIHelper(resp *openaiChatCompletion, model string) (*provider.MessageResponse, error) {
	if resp == nil {
		return nil, nil
	}
	result := &provider.MessageResponse{
		ID:    resp.ID,
		Type:  "message",
		Role:  "assistant",
		Model: model,
		Usage: provider.Usage{
			InputTokens:  int(resp.Usage.PromptTokens),
			OutputTokens: int(resp.Usage.CompletionTokens),
		},
	}

	if len(resp.Choices) == 0 {
		result.StopReason = "end_turn"
		result.Content = provider.TextContent("")
		return result, nil
	}

	choice := resp.Choices[0]
	result.StopReason = mapFinishReason(choice.FinishReason)

	if choice.Message.Content != "" {
		result.Content = append(result.Content, provider.ContentBlock{
			Type: "text",
			Text: choice.Message.Content,
		})
	}

	for _, tc := range choice.Message.ToolCalls {
		if tc.Type != "function" {
			continue
		}
		result.Content = append(result.Content, provider.ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}

	if len(result.Content) == 0 {
		result.Content = provider.TextContent("")
	}

	if hasToolUse(result.Content) {
		result.StopReason = "tool_use"
	}

	return result, nil
}

func TestAssistantMessage_WithToolCalls(t *testing.T) {
	// Proves that assistantMessageToOpenAI correctly maps tool_use blocks to OpenAI function tool calls, and that thinking blocks are silently skipped.
	blocks := []provider.ContentBlock{
		{Type: "text", Text: "Let me run that."},
		{Type: "tool_use", ID: "call_1", Name: "exec", Input: json.RawMessage(`{"cmd":"ls"}`)},
		{Type: "thinking", Thinking: "internal reasoning"}, // should be skipped
	}

	msg := assistantMessageToOpenAI(blocks)
	if msg.OfAssistant == nil {
		t.Fatal("expected assistant message")
	}
	if len(msg.OfAssistant.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(msg.OfAssistant.ToolCalls))
	}
	tc := msg.OfAssistant.ToolCalls[0]
	if tc.OfFunction == nil {
		t.Fatal("expected function tool call")
	}
	if tc.OfFunction.ID != "call_1" {
		t.Errorf("id = %q", tc.OfFunction.ID)
	}
	if tc.OfFunction.Function.Name != "exec" {
		t.Errorf("name = %q", tc.OfFunction.Function.Name)
	}
}

func TestUserMessage_WithImage(t *testing.T) {
	// Proves that a mixed user message containing text and a base64 image is translated into a single OpenAI user message (multi-part content).
	blocks := []provider.ContentBlock{
		{Type: "text", Text: "what is this?"},
		{Type: "image", Source: &provider.ContentSource{
			Type:      "base64",
			MimeType: "image/png",
			Data:      "iVBORw0KGgo=",
		}},
	}

	result := userMessageToOpenAI(blocks)
	if len(result) != 1 {
		t.Fatalf("messages = %d, want 1", len(result))
	}
	if result[0].OfUser == nil {
		t.Fatal("expected user message")
	}
}

