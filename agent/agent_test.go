package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestWithCacheBreakpoint(t *testing.T) {
	tests := []struct {
		name     string
		messages []anthropic.Message
		wantIdx  int // index that should get cache_control (-1 for none)
	}{
		{
			name:     "empty",
			messages: nil,
			wantIdx:  -1,
		},
		{
			name: "single message",
			messages: []anthropic.Message{
				{Role: "user", Content: anthropic.TextContent("hi")},
			},
			wantIdx: -1,
		},
		{
			name: "two messages",
			messages: []anthropic.Message{
				{Role: "user", Content: anthropic.TextContent("hi")},
				{Role: "user", Content: anthropic.TextContent("second")},
			},
			wantIdx: 0, // second-to-last
		},
		{
			name: "three messages",
			messages: []anthropic.Message{
				{Role: "user", Content: anthropic.TextContent("first")},
				{Role: "assistant", Content: anthropic.TextContent("reply")},
				{Role: "user", Content: anthropic.TextContent("second")},
			},
			wantIdx: 1, // second-to-last
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := withCacheBreakpoint(tt.messages)

			if tt.wantIdx < 0 {
				// No cache_control should be set
				for i, msg := range result {
					for j, block := range msg.Content {
						if block.CacheControl != nil {
							t.Errorf("msg[%d].content[%d] has unexpected cache_control", i, j)
						}
					}
				}
				return
			}

			// Verify cache_control on expected message
			lastBlock := result[tt.wantIdx].Content[len(result[tt.wantIdx].Content)-1]
			if lastBlock.CacheControl == nil {
				t.Fatalf("msg[%d] missing cache_control", tt.wantIdx)
			}
			if lastBlock.CacheControl.Type != "ephemeral" {
				t.Errorf("cache_control.type = %q, want ephemeral", lastBlock.CacheControl.Type)
			}

			// Verify original messages not modified
			if len(tt.messages) > tt.wantIdx {
				origBlock := tt.messages[tt.wantIdx].Content[len(tt.messages[tt.wantIdx].Content)-1]
				if origBlock.CacheControl != nil {
					t.Error("original message was modified — cache_control should only be on the copy")
				}
			}
		})
	}
}

func TestCacheBreakpointInRequest(t *testing.T) {
	// Verify that the API request includes cache_control but saved session does not
	var receivedReq *anthropic.MessageRequest

	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		receivedReq = req
		return &anthropic.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("reply"),
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

	// First message — no breakpoint (only 1 message)
	ag.HandleMessage(context.Background(), "agent:test:cache", "First")

	// Second message — should have breakpoint on the previous assistant turn
	ag.HandleMessage(context.Background(), "agent:test:cache", "Second")

	if receivedReq == nil {
		t.Fatal("no request received")
	}

	// API request should have cache_control on second-to-last message
	if len(receivedReq.Messages) < 2 {
		t.Fatalf("got %d messages in request", len(receivedReq.Messages))
	}
	breakpointMsg := receivedReq.Messages[len(receivedReq.Messages)-2]
	lastBlock := breakpointMsg.Content[len(breakpointMsg.Content)-1]
	if lastBlock.CacheControl == nil {
		t.Error("API request missing cache_control on second-to-last message")
	}

	// Saved session should NOT have cache_control
	saved, _ := store.Load("agent:test:cache")
	for i, msg := range saved {
		for j, block := range msg.Content {
			if block.CacheControl != nil {
				t.Errorf("saved msg[%d].content[%d] has cache_control — should not be persisted", i, j)
			}
		}
	}
}

func TestHandleMessageCancellation(t *testing.T) {
	// Verify that a cancelled context causes HandleMessage to return ctx.Err()
	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		return &anthropic.MessageResponse{
			ID:   "msg_1",
			Type: "message",
			Role: "assistant",
			Content: []anthropic.ContentBlock{
				{
					Type:  "tool_use",
					ID:    "tu_001",
					Name:  "slow_tool",
					Input: json.RawMessage(`{}`),
				},
			},
			StopReason: "tool_use",
			Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()

	// Register a tool that blocks until context is cancelled
	registry.Register(&tools.Tool{
		Name:        "slow_tool",
		Description: "blocks forever",
		Parameters:  json.RawMessage(`{"type":"object"}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
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

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after a short delay
	go func() {
		// Wait for tool to start
		for !ag.IsProcessing() {
			// spin until processing starts
		}
		cancel()
	}()

	_, err := ag.HandleMessage(ctx, "agent:test:cancel", "Do something slow")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

func TestIsProcessing(t *testing.T) {
	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		return &anthropic.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("done"),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	if ag.IsProcessing() {
		t.Error("should not be processing before HandleMessage")
	}

	ag.HandleMessage(context.Background(), "agent:test:proc", "Hi")

	if ag.IsProcessing() {
		t.Error("should not be processing after HandleMessage returns")
	}
}

// newTestClientWithBase creates a test client with a custom base URL.
func newTestClientWithBase(baseURL, apiKey string) *anthropic.Client {
	return anthropic.NewClientWithBase(baseURL, apiKey)
}

func TestFormatGap(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{38 * time.Second, "38s"},
		{90 * time.Second, "1m30s"},
		{3*time.Hour + 12*time.Minute, "3h12m"},
		{49*time.Hour + 30*time.Minute, "2d1h"},
	}
	for _, tt := range tests {
		got := formatGap(tt.d)
		if got != tt.want {
			t.Errorf("formatGap(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestBuildMetaPrefix(t *testing.T) {
	now := time.Date(2026, 2, 21, 5, 30, 0, 0, time.UTC)

	// First message — no previous turn data
	sm := &sessionMeta{}
	prefix := buildMetaPrefix(now, sm)
	if !strings.Contains(prefix, "time=2026-02-21T05:30:00Z") {
		t.Errorf("missing timestamp in prefix: %q", prefix)
	}
	if !strings.Contains(prefix, "gap=none") {
		t.Errorf("first message should have gap=none: %q", prefix)
	}
	if strings.Contains(prefix, "prev_cost") {
		t.Errorf("first message should not have prev_cost: %q", prefix)
	}

	// Subsequent message — has previous turn data
	sm.lastMessageTime = now.Add(-3*time.Hour - 12*time.Minute)
	sm.prevCost = 0.043
	sm.prevInput = 2400
	sm.prevOutput = 312
	sm.prevCacheRead = 18000
	sm.prevCacheWrite = 200

	prefix = buildMetaPrefix(now, sm)
	if !strings.Contains(prefix, "gap=3h12m") {
		t.Errorf("missing gap in prefix: %q", prefix)
	}
	if !strings.Contains(prefix, "prev_cost=$0.0430") {
		t.Errorf("missing prev_cost in prefix: %q", prefix)
	}
	if !strings.Contains(prefix, "prev_tokens=in:2400/out:312/cR:18000/cW:200") {
		t.Errorf("missing prev_tokens in prefix: %q", prefix)
	}
}

func TestMetadataInjectedInMessage(t *testing.T) {
	var receivedReq *anthropic.MessageRequest

	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		receivedReq = req
		return &anthropic.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	ag.HandleMessage(context.Background(), "agent:test:meta", "Hello")

	if receivedReq == nil {
		t.Fatal("no request received")
	}

	// The user message should have the meta prefix
	lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	text := anthropic.TextOf(lastMsg.Content)
	if !strings.Contains(text, "[meta]") {
		t.Errorf("user message missing [meta] prefix: %q", text)
	}
	if !strings.Contains(text, "Hello") {
		t.Errorf("user message missing original text: %q", text)
	}
}
