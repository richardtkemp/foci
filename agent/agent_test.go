package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clod/anthropic"
	"clod/compaction"
	"clod/memory"
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

func TestDeferredReply(t *testing.T) {
	// Verify that text in tool_use responses is sent via ReplyFunc
	var callCount atomic.Int32

	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		n := callCount.Add(1)
		if n == 1 {
			// First response: text + tool_use (deferred reply scenario)
			return &anthropic.MessageResponse{
				ID:   "msg_1",
				Type: "message",
				Role: "assistant",
				Content: []anthropic.ContentBlock{
					{Type: "text", Text: "Looking into this, give me a moment..."},
					{Type: "tool_use", ID: "tu_001", Name: "test_tool", Input: json.RawMessage(`{}`)},
				},
				StopReason: "tool_use",
				Usage:      anthropic.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}
		// Second response: final answer
		return &anthropic.MessageResponse{
			ID:         "msg_2",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("Here's the full answer."),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 30, OutputTokens: 15},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	registry.Register(&tools.Tool{
		Name:       "test_tool",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return "tool result", nil
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

	// Track intermediate replies
	var intermediateReplies []string
	ag.SetReplyFunc(func(text string) {
		intermediateReplies = append(intermediateReplies, text)
	})
	defer ag.SetReplyFunc(nil)

	finalResp, err := ag.HandleMessage(context.Background(), "agent:test:deferred", "Complex question")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// Should have received the intermediate reply
	if len(intermediateReplies) != 1 {
		t.Fatalf("expected 1 intermediate reply, got %d", len(intermediateReplies))
	}
	if intermediateReplies[0] != "Looking into this, give me a moment..." {
		t.Errorf("intermediate reply = %q", intermediateReplies[0])
	}

	// Final response should be the end_turn text
	if finalResp != "Here's the full answer." {
		t.Errorf("final response = %q", finalResp)
	}
}

// newTestClientWithBase creates a test client with a custom base URL.
func newTestClientWithBase(baseURL, apiKey string) *anthropic.Client {
	return anthropic.NewClientWithBase(baseURL, apiKey)
}

func TestHandleMessageWithImages(t *testing.T) {
	var receivedReq *anthropic.MessageRequest

	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		receivedReq = req
		return &anthropic.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("I see a cat!"),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 100, OutputTokens: 10},
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

	images := []ImageData{
		{MediaType: "image/jpeg", Data: []byte("fake-jpeg-data")},
	}
	resp, err := ag.HandleMessageWithImages(context.Background(), "agent:test:img", "What is this?", images)
	if err != nil {
		t.Fatalf("HandleMessageWithImages: %v", err)
	}
	if resp != "I see a cat!" {
		t.Errorf("response = %q", resp)
	}

	if receivedReq == nil {
		t.Fatal("no request received")
	}

	// Check the user message has image + text blocks
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	if len(userMsg.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(userMsg.Content))
	}

	// First block should be image
	if userMsg.Content[0].Type != "image" {
		t.Errorf("content[0].Type = %q, want image", userMsg.Content[0].Type)
	}
	if userMsg.Content[0].Source == nil {
		t.Fatal("content[0].Source is nil")
	}
	if userMsg.Content[0].Source.MediaType != "image/jpeg" {
		t.Errorf("content[0].Source.MediaType = %q", userMsg.Content[0].Source.MediaType)
	}

	// Second block should be text with metadata + user text
	if userMsg.Content[1].Type != "text" {
		t.Errorf("content[1].Type = %q, want text", userMsg.Content[1].Type)
	}
	if !strings.Contains(userMsg.Content[1].Text, "What is this?") {
		t.Errorf("content[1].Text missing user text: %q", userMsg.Content[1].Text)
	}
	if !strings.Contains(userMsg.Content[1].Text, "[meta]") {
		t.Errorf("content[1].Text missing [meta]: %q", userMsg.Content[1].Text)
	}
}

func TestHandleMessageWithImagesNoText(t *testing.T) {
	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		return &anthropic.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("I see an image."),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 100, OutputTokens: 10},
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

	images := []ImageData{
		{MediaType: "image/png", Data: []byte("fake-png-data")},
	}
	// Empty text — image only
	resp, err := ag.HandleMessageWithImages(context.Background(), "agent:test:imgonly", "", images)
	if err != nil {
		t.Fatalf("HandleMessageWithImages: %v", err)
	}
	if resp != "I see an image." {
		t.Errorf("response = %q", resp)
	}
}

func TestHandleMessageDelegatesToWithImages(t *testing.T) {
	// Verify HandleMessage (text-only) still works correctly
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

	ag.HandleMessage(context.Background(), "agent:test:delegate", "Hello")

	// Text-only message should have exactly 1 content block (text)
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	if len(userMsg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(userMsg.Content))
	}
	if userMsg.Content[0].Type != "text" {
		t.Errorf("content[0].Type = %q, want text", userMsg.Content[0].Type)
	}
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
	prefix := buildMetaPrefix(now, "claude-haiku-4-5", "", sm)
	if !strings.Contains(prefix, "time=2026-02-21T05:30:00Z") {
		t.Errorf("missing timestamp in prefix: %q", prefix)
	}
	if !strings.Contains(prefix, "gap=none") {
		t.Errorf("first message should have gap=none: %q", prefix)
	}
	if !strings.Contains(prefix, "model=claude-haiku-4-5") {
		t.Errorf("missing model in prefix: %q", prefix)
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

	prefix = buildMetaPrefix(now, "claude-haiku-4-5", "", sm)
	if !strings.Contains(prefix, "gap=3h12m") {
		t.Errorf("missing gap in prefix: %q", prefix)
	}
	if !strings.Contains(prefix, "model=claude-haiku-4-5") {
		t.Errorf("missing model in prefix: %q", prefix)
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
	if !strings.Contains(text, "model=claude-haiku-4-5") {
		t.Errorf("user message missing model in [meta]: %q", text)
	}
	if !strings.Contains(text, "Hello") {
		t.Errorf("user message missing original text: %q", text)
	}
}

func TestVoiceMode(t *testing.T) {
	ag := &Agent{Model: "test"}

	// Default: voice mode off
	if ag.VoiceMode("session1") {
		t.Error("voice mode should be off by default")
	}

	// Turn on
	ag.SetVoiceMode("session1", true)
	if !ag.VoiceMode("session1") {
		t.Error("voice mode should be on after SetVoiceMode(true)")
	}

	// Other session unaffected
	if ag.VoiceMode("session2") {
		t.Error("voice mode should be off for different session")
	}

	// Turn off
	ag.SetVoiceMode("session1", false)
	if ag.VoiceMode("session1") {
		t.Error("voice mode should be off after SetVoiceMode(false)")
	}
}

func TestBuildMetaPrefix_VoiceMode(t *testing.T) {
	now := time.Date(2026, 2, 21, 12, 0, 0, 0, time.UTC)

	// Without voice mode
	sm := &sessionMeta{}
	prefix := buildMetaPrefix(now, "claude-haiku-4-5", "", sm)
	if strings.Contains(prefix, "voice=on") {
		t.Errorf("should not contain voice=on when voice mode off: %q", prefix)
	}

	// With voice mode
	sm.voiceMode = true
	prefix = buildMetaPrefix(now, "claude-haiku-4-5", "", sm)
	if !strings.Contains(prefix, "voice=on") {
		t.Errorf("should contain voice=on when voice mode on: %q", prefix)
	}
}

func TestBuildMetaPrefix_Mana(t *testing.T) {
	now := time.Date(2026, 2, 21, 12, 0, 0, 0, time.UTC)

	// Without mana
	sm := &sessionMeta{}
	prefix := buildMetaPrefix(now, "claude-haiku-4-5", "", sm)
	if strings.Contains(prefix, "mana=") {
		t.Errorf("should not contain mana when empty: %q", prefix)
	}

	// With mana (first message)
	prefix = buildMetaPrefix(now, "claude-haiku-4-5", "75%", sm)
	if !strings.Contains(prefix, "mana=75%") {
		t.Errorf("should contain mana=75%%: %q", prefix)
	}

	// With mana (subsequent message with cost data)
	sm.prevCost = 0.01
	sm.prevInput = 100
	prefix = buildMetaPrefix(now, "claude-haiku-4-5", "50%", sm)
	if !strings.Contains(prefix, "mana=50%") {
		t.Errorf("should contain mana=50%% in subsequent message: %q", prefix)
	}
}

func TestDuplicateMessages(t *testing.T) {
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
		Client:            client,
		Sessions:          store,
		Tools:             tools.NewRegistry(),
		Bootstrap:         bootstrap,
		Model:             "claude-haiku-4-5",
		DuplicateMessages: true,
	}

	ag.HandleMessage(context.Background(), "agent:test:dup", "Do the thing")

	// The user message text should contain the instruction twice
	lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	text := anthropic.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 2 {
		t.Errorf("expected user text duplicated (2 occurrences), got %d in: %q", count, text)
	}

	// Meta prefix should only appear once
	if count := strings.Count(text, "[meta]"); count != 1 {
		t.Errorf("expected [meta] once, got %d", count)
	}

	// Saved session should also have the duplicated text (for cache coherence)
	saved, _ := store.Load("agent:test:dup")
	savedText := anthropic.TextOf(saved[0].Content)
	if count := strings.Count(savedText, "Do the thing"); count != 2 {
		t.Errorf("saved session should have duplicated text, got %d occurrences", count)
	}
}

func TestDuplicateMessagesDisabled(t *testing.T) {
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
		Client:            client,
		Sessions:          store,
		Tools:             tools.NewRegistry(),
		Bootstrap:         bootstrap,
		Model:             "claude-haiku-4-5",
		DuplicateMessages: false,
	}

	ag.HandleMessage(context.Background(), "agent:test:nodup", "Do the thing")

	lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	text := anthropic.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 1 {
		t.Errorf("expected user text once (no duplication), got %d in: %q", count, text)
	}
}

func TestRepairInterruptedToolCalls(t *testing.T) {
	t.Run("empty messages", func(t *testing.T) {
		if got := repairInterruptedToolCalls(nil); got != nil {
			t.Errorf("expected nil for empty messages, got %v", got)
		}
	})

	t.Run("last message is user", func(t *testing.T) {
		msgs := []anthropic.Message{
			{Role: "user", Content: anthropic.TextContent("hi")},
		}
		if got := repairInterruptedToolCalls(msgs); got != nil {
			t.Errorf("expected nil when last message is user, got %v", got)
		}
	})

	t.Run("assistant with text only", func(t *testing.T) {
		msgs := []anthropic.Message{
			{Role: "user", Content: anthropic.TextContent("hi")},
			{Role: "assistant", Content: anthropic.TextContent("hello")},
		}
		if got := repairInterruptedToolCalls(msgs); got != nil {
			t.Errorf("expected nil when no tool_use blocks, got %v", got)
		}
	})

	t.Run("single tool_use", func(t *testing.T) {
		msgs := []anthropic.Message{
			{Role: "user", Content: anthropic.TextContent("hi")},
			{Role: "assistant", Content: []anthropic.ContentBlock{
				{Type: "text", Text: "Let me check."},
				{Type: "tool_use", ID: "tu_123", Name: "some_tool", Input: json.RawMessage(`{}`)},
			}},
		}
		got := repairInterruptedToolCalls(msgs)
		if got == nil {
			t.Fatal("expected repair message, got nil")
		}
		if got.Role != "user" {
			t.Errorf("repair message role = %q, want user", got.Role)
		}
		if len(got.Content) != 1 {
			t.Fatalf("expected 1 tool_result block, got %d", len(got.Content))
		}
		if got.Content[0].Type != "tool_result" {
			t.Errorf("block type = %q, want tool_result", got.Content[0].Type)
		}
		if got.Content[0].ToolUseID != "tu_123" {
			t.Errorf("tool_use_id = %q, want tu_123", got.Content[0].ToolUseID)
		}
		if !got.Content[0].IsError {
			t.Error("expected is_error = true")
		}
		if got.Content[0].Content != "Tool call interrupted by service restart" {
			t.Errorf("content = %q, want %q", got.Content[0].Content, "Tool call interrupted by service restart")
		}
	})

	t.Run("multiple tool_use blocks", func(t *testing.T) {
		msgs := []anthropic.Message{
			{Role: "user", Content: anthropic.TextContent("hi")},
			{Role: "assistant", Content: []anthropic.ContentBlock{
				{Type: "tool_use", ID: "tu_a", Name: "tool_a", Input: json.RawMessage(`{}`)},
				{Type: "tool_use", ID: "tu_b", Name: "tool_b", Input: json.RawMessage(`{}`)},
			}},
		}
		got := repairInterruptedToolCalls(msgs)
		if got == nil {
			t.Fatal("expected repair message, got nil")
		}
		if len(got.Content) != 2 {
			t.Fatalf("expected 2 tool_result blocks, got %d", len(got.Content))
		}
		if got.Content[0].ToolUseID != "tu_a" {
			t.Errorf("block[0].tool_use_id = %q, want tu_a", got.Content[0].ToolUseID)
		}
		if got.Content[1].ToolUseID != "tu_b" {
			t.Errorf("block[1].tool_use_id = %q, want tu_b", got.Content[1].ToolUseID)
		}
	})
}

func TestRepairInterruptedToolCallsPersisted(t *testing.T) {
	// Simulate a session with an interrupted tool call, then verify
	// HandleMessage repairs it before sending to the API.
	var receivedReq *anthropic.MessageRequest

	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		receivedReq = req
		return &anthropic.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("Recovered."),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 50, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	// Pre-populate session with an interrupted tool call
	sessionKey := "agent:test:repair"
	store.Append(sessionKey, anthropic.Message{
		Role: "user", Content: anthropic.TextContent("do something"),
	})
	store.Append(sessionKey, anthropic.Message{
		Role: "assistant", Content: []anthropic.ContentBlock{
			{Type: "tool_use", ID: "tu_interrupted", Name: "some_tool", Input: json.RawMessage(`{}`)},
		},
	})

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	_, err := ag.HandleMessage(context.Background(), sessionKey, "continue")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// The API request should include the repair tool_result before the new user message
	if receivedReq == nil {
		t.Fatal("no request received")
	}

	// Messages: user("do something"), assistant(tool_use), user(tool_result repair), user("continue")
	// But Anthropic requires alternating roles, so the repair and new message are separate user turns.
	// Let's check the repair is in there.
	found := false
	for _, msg := range receivedReq.Messages {
		for _, block := range msg.Content {
			if block.Type == "tool_result" && block.ToolUseID == "tu_interrupted" && block.IsError {
				found = true
			}
		}
	}
	if !found {
		t.Error("API request missing repair tool_result for tu_interrupted")
	}

	// Verify repair was persisted to the session store
	saved, _ := store.Load(sessionKey)
	// Should have: user, assistant(tool_use), user(tool_result repair), user(continue), assistant(Recovered.)
	repairFound := false
	for _, msg := range saved {
		for _, block := range msg.Content {
			if block.Type == "tool_result" && block.ToolUseID == "tu_interrupted" {
				repairFound = true
			}
		}
	}
	if !repairFound {
		t.Error("repair tool_result not persisted to session store")
	}
}

func TestVoiceReplyFunc(t *testing.T) {
	ag := &Agent{Model: "test"}

	var received []byte
	ag.SetVoiceReplyFunc(func(data []byte) {
		received = data
	})

	ag.sendVoice([]byte("test-audio"))
	if string(received) != "test-audio" {
		t.Errorf("got %q, want %q", string(received), "test-audio")
	}

	// Clear and verify
	ag.SetVoiceReplyFunc(nil)
	received = nil
	ag.sendVoice([]byte("should-not-deliver"))
	if received != nil {
		t.Error("should not deliver after clearing voice reply func")
	}
}

func TestAgentCompactionIntegration(t *testing.T) {
	// compactionMockServer creates a mock that returns a canned summary for
	// compaction requests (detected by "concise summary" in the last message)
	// and normal end_turn responses otherwise. Turn highTokenTurn gets
	// InputTokens=170000 to exceed the 160k threshold (0.8 * 200k).
	compactionMockServer := func(turnCount *atomic.Int32, highTokenTurn int32) *httptest.Server {
		return mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
			lastMsg := req.Messages[len(req.Messages)-1]
			if strings.Contains(anthropic.TextOf(lastMsg.Content), "concise summary") {
				return &anthropic.MessageResponse{
					ID:         "msg_summary",
					Type:       "message",
					Role:       "assistant",
					Content:    anthropic.TextContent("This is the compacted summary of the conversation."),
					StopReason: "end_turn",
					Usage:      anthropic.Usage{InputTokens: 500, OutputTokens: 100},
				}
			}

			n := turnCount.Add(1)
			inputTokens := 1000
			if n == highTokenTurn {
				inputTokens = 170_000
			}
			return &anthropic.MessageResponse{
				ID:         fmt.Sprintf("msg_%d", n),
				Type:       "message",
				Role:       "assistant",
				Content:    anthropic.TextContent(fmt.Sprintf("Response %d", n)),
				StopReason: "end_turn",
				Usage:      anthropic.Usage{InputTokens: inputTokens, OutputTokens: 50},
			}
		})
	}

	t.Run("basic", func(t *testing.T) {
		var turnCount atomic.Int32
		server := compactionMockServer(&turnCount, 5)
		defer server.Close()

		client := newTestClientWithBase(server.URL, "test-token")
		store := session.NewStore(t.TempDir())
		bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
		compactor := compaction.NewCompactor(client, store, "claude-haiku-4-5", 0.8)

		ag := &Agent{
			Client:    client,
			Sessions:  store,
			Tools:     tools.NewRegistry(),
			Bootstrap: bootstrap,
			Compactor: compactor,
			Model:     "claude-haiku-4-5",
		}

		sessionKey := "agent:test:compact"

		// Phase 1: 4 turns with low tokens — no compaction
		for i := 1; i <= 4; i++ {
			resp, err := ag.HandleMessage(context.Background(), sessionKey, fmt.Sprintf("Turn %d", i))
			if err != nil {
				t.Fatalf("Turn %d: %v", i, err)
			}
			if resp != fmt.Sprintf("Response %d", i) {
				t.Errorf("Turn %d: response = %q", i, resp)
			}
		}

		msgs, _ := store.Load(sessionKey)
		if len(msgs) != 8 {
			t.Fatalf("after 4 turns: %d messages, want 8", len(msgs))
		}

		// Phase 2: Turn 5 — high tokens triggers compaction
		resp, err := ag.HandleMessage(context.Background(), sessionKey, "Turn 5")
		if err != nil {
			t.Fatalf("Turn 5: %v", err)
		}
		if resp != "Response 5" {
			t.Errorf("Turn 5: response = %q", resp)
		}

		// After compaction, session should have exactly 3 messages
		msgs, _ = store.Load(sessionKey)
		if len(msgs) != 3 {
			t.Fatalf("after compaction: %d messages, want 3", len(msgs))
		}

		// msg[0]: marker
		if !strings.Contains(anthropic.TextOf(msgs[0].Content), "compacted") {
			t.Errorf("msg[0] should contain 'compacted': %q", anthropic.TextOf(msgs[0].Content))
		}
		// msg[1]: summary from mock
		if !strings.Contains(anthropic.TextOf(msgs[1].Content), "compacted summary") {
			t.Errorf("msg[1] should contain summary: %q", anthropic.TextOf(msgs[1].Content))
		}
		// msg[2]: handoff
		if !strings.Contains(anthropic.TextOf(msgs[2].Content), "Compaction complete") {
			t.Errorf("msg[2] should contain handoff: %q", anthropic.TextOf(msgs[2].Content))
		}

		// Phase 3: Turn 6 — post-compaction continuity
		resp, err = ag.HandleMessage(context.Background(), sessionKey, "Turn 6")
		if err != nil {
			t.Fatalf("Turn 6: %v", err)
		}
		if resp != "Response 6" {
			t.Errorf("Turn 6: response = %q", resp)
		}

		// 3 compacted + user turn 6 + assistant turn 6 = 5
		msgs, _ = store.Load(sessionKey)
		if len(msgs) != 5 {
			t.Fatalf("after Turn 6: %d messages, want 5", len(msgs))
		}
	})

	t.Run("scratchpad", func(t *testing.T) {
		var turnCount atomic.Int32
		server := compactionMockServer(&turnCount, 5)
		defer server.Close()

		client := newTestClientWithBase(server.URL, "test-token")
		store := session.NewStore(t.TempDir())
		bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
		compactor := compaction.NewCompactor(client, store, "claude-haiku-4-5", 0.8)

		// Set up scratchpad with entries
		scratchpad, err := memory.NewScratchpad(filepath.Join(t.TempDir(), "scratchpad.db"))
		if err != nil {
			t.Fatalf("create scratchpad: %v", err)
		}
		defer scratchpad.Close()

		if err := scratchpad.Write("current_task", "implementing feature X"); err != nil {
			t.Fatalf("write scratchpad: %v", err)
		}
		if err := scratchpad.Write("blockers", "need API key for auth"); err != nil {
			t.Fatalf("write scratchpad: %v", err)
		}

		compactor.Scratchpad = scratchpad

		ag := &Agent{
			Client:    client,
			Sessions:  store,
			Tools:     tools.NewRegistry(),
			Bootstrap: bootstrap,
			Compactor: compactor,
			Model:     "claude-haiku-4-5",
		}

		sessionKey := "agent:test:compactsp"

		// Build up 4 turns then trigger compaction on turn 5
		for i := 1; i <= 5; i++ {
			if _, err := ag.HandleMessage(context.Background(), sessionKey, fmt.Sprintf("Turn %d", i)); err != nil {
				t.Fatalf("Turn %d: %v", i, err)
			}
		}

		msgs, _ := store.Load(sessionKey)
		if len(msgs) != 3 {
			t.Fatalf("after compaction: %d messages, want 3", len(msgs))
		}

		// Verify handoff message contains scratchpad data
		handoff := anthropic.TextOf(msgs[2].Content)
		if !strings.Contains(handoff, "scratchpad") {
			t.Errorf("handoff should mention scratchpad: %q", handoff)
		}
		if !strings.Contains(handoff, "current_task") {
			t.Errorf("handoff should contain key 'current_task': %q", handoff)
		}
		if !strings.Contains(handoff, "implementing feature X") {
			t.Errorf("handoff should contain scratchpad value: %q", handoff)
		}
		if !strings.Contains(handoff, "blockers") {
			t.Errorf("handoff should contain key 'blockers': %q", handoff)
		}
		if !strings.Contains(handoff, "need API key for auth") {
			t.Errorf("handoff should contain scratchpad value: %q", handoff)
		}
	})
}

func TestIntermediateTextBeforeToolCalls(t *testing.T) {
	// Verify the agent calls sendIntermediate before notifyToolCall when the
	// API response contains both text and tool_use blocks. This ordering is
	// critical for Telegram message display: thinking text must appear above
	// tool call notifications in the chat.
	var callCount atomic.Int32

	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		n := callCount.Add(1)
		if n == 1 {
			return &anthropic.MessageResponse{
				ID:   "msg_1",
				Type: "message",
				Role: "assistant",
				Content: []anthropic.ContentBlock{
					{Type: "text", Text: "Let me check..."},
					{Type: "tool_use", ID: "tu_001", Name: "test_tool", Input: json.RawMessage(`{}`)},
				},
				StopReason: "tool_use",
				Usage:      anthropic.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}
		return &anthropic.MessageResponse{
			ID:         "msg_2",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("Done."),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 30, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	registry.Register(&tools.Tool{
		Name:       "test_tool",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return "ok", nil
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

	// Record callback invocation order
	var mu sync.Mutex
	var order []string

	ag.SetReplyFunc(func(text string) {
		mu.Lock()
		order = append(order, "reply:"+text)
		mu.Unlock()
	})
	defer ag.SetReplyFunc(nil)

	ag.SetToolCallObserver(func(name string, params json.RawMessage) {
		mu.Lock()
		order = append(order, "tool:"+name)
		mu.Unlock()
	})
	defer ag.SetToolCallObserver(nil)

	_, err := ag.HandleMessage(context.Background(), "agent:test:order", "Check something")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(order) < 2 {
		t.Fatalf("expected at least 2 callbacks, got %d: %v", len(order), order)
	}
	if order[0] != "reply:Let me check..." {
		t.Errorf("order[0] = %q, want reply callback first", order[0])
	}
	if order[1] != "tool:test_tool" {
		t.Errorf("order[1] = %q, want tool callback second", order[1])
	}
}

func TestConcurrentTurnSerialization(t *testing.T) {
	// Verify that concurrent HandleMessage calls on the same session
	// are serialized — messages never interleave. This is critical for
	// Anthropic's prefix-matched prompt cache: the conversation history
	// sent to the API must be a strict append-only extension of the
	// previous request.

	var mu sync.Mutex
	var apiCallOrder []string // tracks which turn's messages were seen by API

	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		// Identify which turn this is by looking at the last user message
		lastMsg := req.Messages[len(req.Messages)-1]
		text := anthropic.TextOf(lastMsg.Content)

		mu.Lock()
		apiCallOrder = append(apiCallOrder, text)
		mu.Unlock()

		// Slow down the first turn so concurrent callers pile up
		if strings.Contains(text, "Turn A") {
			time.Sleep(100 * time.Millisecond)
		}

		return &anthropic.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("Reply to: " + text),
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

	sessionKey := "agent:test:concurrent"

	// Launch two concurrent turns on the same session
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		ag.HandleMessage(context.Background(), sessionKey, "Turn A")
	}()

	// Small delay to ensure Turn A acquires the lock first
	time.Sleep(10 * time.Millisecond)

	go func() {
		defer wg.Done()
		ag.HandleMessage(context.Background(), sessionKey, "Turn B")
	}()

	wg.Wait()

	// Verify session has 4 messages in order: user A, assistant A, user B, assistant B
	msgs, err := store.Load(sessionKey)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4", len(msgs))
	}

	// Messages must be strictly ordered: Turn A's pair first, then Turn B's pair
	textA := anthropic.TextOf(msgs[0].Content)
	if !strings.Contains(textA, "Turn A") {
		t.Errorf("msgs[0] should be Turn A's user message, got: %s", textA)
	}
	if msgs[0].Role != "user" {
		t.Errorf("msgs[0].Role = %q, want user", msgs[0].Role)
	}

	replyA := anthropic.TextOf(msgs[1].Content)
	if !strings.Contains(replyA, "Reply to") {
		t.Errorf("msgs[1] should be Turn A's assistant reply, got: %s", replyA)
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msgs[1].Role = %q, want assistant", msgs[1].Role)
	}

	textB := anthropic.TextOf(msgs[2].Content)
	if !strings.Contains(textB, "Turn B") {
		t.Errorf("msgs[2] should be Turn B's user message, got: %s", textB)
	}
	if msgs[2].Role != "user" {
		t.Errorf("msgs[2].Role = %q, want user", msgs[2].Role)
	}

	replyB := anthropic.TextOf(msgs[3].Content)
	if !strings.Contains(replyB, "Reply to") {
		t.Errorf("msgs[3] should be Turn B's assistant reply, got: %s", replyB)
	}
	if msgs[3].Role != "assistant" {
		t.Errorf("msgs[3].Role = %q, want assistant", msgs[3].Role)
	}

	// Verify Turn B's API request included Turn A's messages (prefix stability)
	mu.Lock()
	defer mu.Unlock()
	if len(apiCallOrder) != 2 {
		t.Fatalf("expected 2 API calls, got %d", len(apiCallOrder))
	}
	// Turn A should have been processed first
	if !strings.Contains(apiCallOrder[0], "Turn A") {
		t.Errorf("first API call should be Turn A, got: %s", apiCallOrder[0])
	}
	if !strings.Contains(apiCallOrder[1], "Turn B") {
		t.Errorf("second API call should be Turn B, got: %s", apiCallOrder[1])
	}
}

func TestConcurrentTurnsDifferentSessions(t *testing.T) {
	// Verify that turns on DIFFERENT sessions can run concurrently
	// (the per-session lock should not serialize across sessions).
	var mu sync.Mutex
	var activeConcurrent int32
	var maxConcurrent int32

	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		cur := atomic.AddInt32(&activeConcurrent, 1)
		defer atomic.AddInt32(&activeConcurrent, -1)

		mu.Lock()
		if cur > maxConcurrent {
			maxConcurrent = cur
		}
		mu.Unlock()

		time.Sleep(50 * time.Millisecond) // ensure overlap

		return &anthropic.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("OK"),
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

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		ag.HandleMessage(context.Background(), "agent:test:sessionA", "Hello A")
	}()

	go func() {
		defer wg.Done()
		ag.HandleMessage(context.Background(), "agent:test:sessionB", "Hello B")
	}()

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if maxConcurrent < 2 {
		t.Errorf("different sessions should run concurrently, max concurrent = %d", maxConcurrent)
	}
}

func TestConcurrentTurnCancellation(t *testing.T) {
	// Verify that a cancelled context while waiting for the turn lock
	// returns immediately without processing.
	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		time.Sleep(200 * time.Millisecond) // slow turn
		return &anthropic.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("Done"),
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

	sessionKey := "agent:test:cancel-queue"

	// Start a slow turn
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ag.HandleMessage(context.Background(), sessionKey, "Slow turn")
	}()

	time.Sleep(20 * time.Millisecond) // let the slow turn start

	// Start a second turn with a context that we'll cancel
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := ag.HandleMessage(ctx, sessionKey, "Should not process")
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got: %v", err)
	}

	wg.Wait()

	// Only the first turn's messages should be in the session
	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (only the slow turn)", len(msgs))
	}
}

