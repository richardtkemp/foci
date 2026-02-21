package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"clod/anthropic"
	"clod/session"
	"clod/tools"
	"clod/workspace"
)

// mockServer returns a test HTTP server that returns canned Anthropic responses.
// responseFunc is called for each request and should return the MessageResponse.
func mockServer(responseFunc func(req *anthropic.MessageRequest) *anthropic.MessageResponse) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req anthropic.MessageRequest
		json.NewDecoder(r.Body).Decode(&req)

		resp := responseFunc(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

// newClientWithURL creates an Anthropic client pointing at a custom URL.
// Since Client.baseURL is unexported, we create a minimal client struct.
// Workaround: use environment or build a test-specific client.
// For now, we'll test the agent loop logic by testing components individually
// and have one integration test that uses a real mock server.

func TestHandleMessageEndTurn(t *testing.T) {
	// Set up mock API server
	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		return &anthropic.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("Hello from mock!"),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	resp, err := ag.HandleMessage(context.Background(), "agent:test:main", "Hi there")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if resp != "Hello from mock!" {
		t.Errorf("response = %q", resp)
	}

	// Verify messages were saved to session
	msgs, _ := store.Load("agent:test:main")
	if len(msgs) != 2 {
		t.Fatalf("saved %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("msgs[0].Role = %q", msgs[0].Role)
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msgs[1].Role = %q", msgs[1].Role)
	}
}

func TestHandleMessageWithToolUse(t *testing.T) {
	var callCount atomic.Int32

	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		n := callCount.Add(1)

		if n == 1 {
			// First call: respond with tool_use
			return &anthropic.MessageResponse{
				ID:   "msg_1",
				Type: "message",
				Role: "assistant",
				Content: []anthropic.ContentBlock{
					{Type: "text", Text: "Let me run that."},
					{
						Type:  "tool_use",
						ID:    "tu_001",
						Name:  "echo_tool",
						Input: json.RawMessage(`{"text":"hello"}`),
					},
				},
				StopReason: "tool_use",
				Usage:      anthropic.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}

		// Second call: after tool result, respond with end_turn
		return &anthropic.MessageResponse{
			ID:         "msg_2",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("The tool returned: hello"),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 30, OutputTokens: 15},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()

	// Register a simple echo tool
	registry.Register(&tools.Tool{
		Name:        "echo_tool",
		Description: "echoes text",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct{ Text string }
			json.Unmarshal(params, &p)
			return fmt.Sprintf("echo: %s", p.Text), nil
		},
	})

	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	resp, err := ag.HandleMessage(context.Background(), "agent:test:main", "Run echo")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if resp != "The tool returned: hello" {
		t.Errorf("response = %q", resp)
	}

	if int(callCount.Load()) != 2 {
		t.Errorf("API calls = %d, want 2", callCount.Load())
	}

	// Should have saved: user msg, assistant (tool_use), user (tool_result), assistant (final)
	msgs, _ := store.Load("agent:test:main")
	if len(msgs) != 4 {
		t.Fatalf("saved %d messages, want 4", len(msgs))
	}
}

func TestHandleMessageUnknownTool(t *testing.T) {
	var callCount atomic.Int32

	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		n := callCount.Add(1)
		if n == 1 {
			return &anthropic.MessageResponse{
				ID:   "msg_1",
				Type: "message",
				Role: "assistant",
				Content: []anthropic.ContentBlock{
					{
						Type:  "tool_use",
						ID:    "tu_001",
						Name:  "nonexistent_tool",
						Input: json.RawMessage(`{}`),
					},
				},
				StopReason: "tool_use",
				Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		return &anthropic.MessageResponse{
			ID:         "msg_2",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("Sorry, that tool doesn't exist."),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 20, OutputTokens: 10},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry() // empty — no tools registered

	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	resp, err := ag.HandleMessage(context.Background(), "agent:test:main", "Use a fake tool")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if resp != "Sorry, that tool doesn't exist." {
		t.Errorf("response = %q", resp)
	}
}

func TestHandleMessageSessionContinuity(t *testing.T) {
	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		// Count messages in request to verify session history is sent
		msgCount := len(req.Messages)
		return &anthropic.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent(fmt.Sprintf("Received %d messages", msgCount)),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	// First message
	resp, _ := ag.HandleMessage(context.Background(), "agent:test:main", "First")
	if resp != "Received 1 messages" {
		t.Errorf("first response = %q", resp)
	}

	// Second message — should include history
	resp, _ = ag.HandleMessage(context.Background(), "agent:test:main", "Second")
	if resp != "Received 3 messages" {
		t.Errorf("second response = %q, want 'Received 3 messages' (2 history + 1 new)", resp)
	}
}

// newTestClientWithBase creates a test client with a custom base URL.
// This requires exposing or working around the unexported baseURL field.
// We do this by creating a custom Client via reflection or by making it testable.
// For now, use a simpler approach: create the client and set the field.
func newTestClientWithBase(baseURL, apiKey string) *anthropic.Client {
	return anthropic.NewClientWithBase(baseURL, apiKey)
}
