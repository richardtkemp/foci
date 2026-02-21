package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"clod/anthropic"
)

// mockBootstrap implements SystemBlocksProvider for tests.
type mockBootstrap struct {
	blocks []anthropic.SystemBlock
}

func (m *mockBootstrap) SystemBlocks() []anthropic.SystemBlock {
	return m.blocks
}

// mockModelServer returns a test server that captures requests and returns canned responses.
func mockModelServer(handler func(req *anthropic.MessageRequest) *anthropic.MessageResponse) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req anthropic.MessageRequest
		json.NewDecoder(r.Body).Decode(&req)
		resp := handler(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestRequestModelSyncCall(t *testing.T) {
	var receivedReq *anthropic.MessageRequest

	server := mockModelServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		receivedReq = req
		return &anthropic.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("The architecture looks correct."),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 50, OutputTokens: 20},
		}
	})
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	bootstrap := &mockBootstrap{}
	tool := NewRequestModelTool(client, bootstrap)

	params, _ := json.Marshal(map[string]string{
		"model":  "opus",
		"prompt": "Is this cache-sharing architecture correct?",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should return the model's response text
	if result != "The architecture looks correct." {
		t.Errorf("result = %q", result)
	}

	// Should have sent request to opus
	if receivedReq.Model != "claude-opus-4-6" {
		t.Errorf("model = %q, want claude-opus-4-6", receivedReq.Model)
	}

	// Should have no tools in request
	if len(receivedReq.Tools) != 0 {
		t.Errorf("expected no tools, got %d", len(receivedReq.Tools))
	}

	// Should have the prompt as the user message
	if len(receivedReq.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(receivedReq.Messages))
	}
	text := anthropic.TextOf(receivedReq.Messages[0].Content)
	if !strings.Contains(text, "cache-sharing architecture") {
		t.Errorf("message text = %q", text)
	}
}

func TestRequestModelPromptWeightLight(t *testing.T) {
	var receivedReq *anthropic.MessageRequest

	server := mockModelServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		receivedReq = req
		return &anthropic.MessageResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: anthropic.TextContent("ok"), StopReason: "end_turn",
			Usage: anthropic.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	bootstrap := &mockBootstrap{blocks: []anthropic.SystemBlock{
		{Type: "text", Text: "I am a character with a long backstory."},
	}}
	tool := NewRequestModelTool(client, bootstrap)

	// Default weight is "light" — should NOT include character files
	params, _ := json.Marshal(map[string]string{
		"model":  "sonnet",
		"prompt": "Quick question",
	})
	tool.Execute(context.Background(), params)

	// Light weight: minimal system prompt, not the full character files
	if len(receivedReq.System) != 1 {
		t.Fatalf("expected 1 system block (light), got %d", len(receivedReq.System))
	}
	if !strings.Contains(receivedReq.System[0].Text, "AI assistant") {
		t.Errorf("light system prompt = %q", receivedReq.System[0].Text)
	}
}

func TestRequestModelPromptWeightFull(t *testing.T) {
	var receivedReq *anthropic.MessageRequest

	server := mockModelServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		receivedReq = req
		return &anthropic.MessageResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: anthropic.TextContent("ok"), StopReason: "end_turn",
			Usage: anthropic.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	bootstrap := &mockBootstrap{blocks: []anthropic.SystemBlock{
		{Type: "text", Text: "I am the identity file."},
		{Type: "text", Text: "I am the soul file."},
	}}
	tool := NewRequestModelTool(client, bootstrap)

	params, _ := json.Marshal(map[string]string{
		"model":        "opus",
		"prompt":       "Think about this deeply",
		"prompt_weight": "full",
	})
	tool.Execute(context.Background(), params)

	// Full weight: should include character files
	if len(receivedReq.System) != 2 {
		t.Fatalf("expected 2 system blocks (full), got %d", len(receivedReq.System))
	}
	if receivedReq.System[0].Text != "I am the identity file." {
		t.Errorf("system[0] = %q", receivedReq.System[0].Text)
	}
}

func TestRequestModelPromptWeightNone(t *testing.T) {
	var receivedReq *anthropic.MessageRequest

	server := mockModelServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		receivedReq = req
		return &anthropic.MessageResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: anthropic.TextContent("ok"), StopReason: "end_turn",
			Usage: anthropic.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	tool := NewRequestModelTool(client, nil)

	params, _ := json.Marshal(map[string]string{
		"model":        "haiku",
		"prompt":       "Just answer this",
		"prompt_weight": "none",
	})
	tool.Execute(context.Background(), params)

	// None weight: no system prompt
	if len(receivedReq.System) != 0 {
		t.Errorf("expected 0 system blocks (none), got %d", len(receivedReq.System))
	}
}

func TestRequestModelShortNames(t *testing.T) {
	tests := []struct {
		short string
		full  string
	}{
		{"haiku", "claude-haiku-4-5"},
		{"sonnet", "claude-sonnet-4-5"},
		{"opus", "claude-opus-4-6"},
		{"claude-haiku-4-5", "claude-haiku-4-5"},
	}

	for _, tt := range tests {
		var receivedModel string
		server := mockModelServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
			receivedModel = req.Model
			return &anthropic.MessageResponse{
				ID: "msg_test", Type: "message", Role: "assistant",
				Content: anthropic.TextContent("ok"), StopReason: "end_turn",
				Usage: anthropic.Usage{InputTokens: 10, OutputTokens: 5},
			}
		})

		client := anthropic.NewClientWithBase(server.URL, "test-token")
		tool := NewRequestModelTool(client, nil)

		params, _ := json.Marshal(map[string]string{
			"model":  tt.short,
			"prompt": "test",
		})
		tool.Execute(context.Background(), params)
		server.Close()

		if receivedModel != tt.full {
			t.Errorf("short=%q: model=%q, want %q", tt.short, receivedModel, tt.full)
		}
	}
}
