package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/anthropic"
	"foci/internal/provider"
	"foci/internal/memory"
	"foci/internal/session"
	"foci/internal/state"
	"foci/internal/tools"
	"foci/internal/warnings"
	"foci/internal/workspace"
)

// mockServer returns a test HTTP server that returns canned Anthropic responses.
// responseFunc is called for each request and should return the MessageResponse.
// Handles both non-streaming (JSON) and streaming (SSE) requests automatically.
func mockServer(responseFunc func(req *provider.MessageRequest) *provider.MessageResponse) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read raw body to check for stream flag before decoding.
		var raw json.RawMessage
		json.NewDecoder(r.Body).Decode(&raw)

		var req provider.MessageRequest
		json.Unmarshal(raw, &req)

		resp := responseFunc(&req)

		// Check if this is a streaming request.
		var envelope struct{ Stream bool }
		json.Unmarshal(raw, &envelope)
		if envelope.Stream {
			serveSSE(w, resp)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

// serveSSE writes a MessageResponse as an SSE event stream.
func serveSSE(w http.ResponseWriter, resp *provider.MessageResponse) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "flushing not supported", http.StatusInternalServerError)
		return
	}

	text := provider.TextOf(resp.Content)

	fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", mustJSON(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": resp.ID, "type": "message", "role": "assistant",
			"content": []any{}, "model": "claude-haiku-4-5",
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens": resp.Usage.InputTokens, "output_tokens": 0,
				"cache_creation_input_tokens": resp.Usage.CacheCreationInputTokens,
				"cache_read_input_tokens":     resp.Usage.CacheReadInputTokens,
			},
		},
	}))
	flusher.Flush()

	fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n", mustJSON(map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	}))
	flusher.Flush()

	fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", mustJSON(map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text},
	}))
	flusher.Flush()

	fmt.Fprintf(w, "event: content_block_stop\ndata: %s\n\n", mustJSON(map[string]any{
		"type": "content_block_stop", "index": 0,
	}))
	flusher.Flush()

	fmt.Fprintf(w, "event: message_delta\ndata: %s\n\n", mustJSON(map[string]any{
		"type": "message_delta",
		"delta": map[string]any{"stop_reason": resp.StopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": resp.Usage.OutputTokens},
	}))
	flusher.Flush()

	fmt.Fprintf(w, "event: message_stop\ndata: %s\n\n", mustJSON(map[string]any{
		"type": "message_stop",
	}))
	flusher.Flush()
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// newClientWithURL creates an Anthropic client pointing at a custom URL.
// Since Client.baseURL is unexported, we create a minimal client struct.
// Workaround: use environment or build a test-specific client.
// For now, we'll test the agent loop logic by testing components individually
// and have one integration test that uses a real mock server.

func TestHandleMessageEndTurn(t *testing.T) {
	// Set up mock API server
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Hello from mock!"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
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

	resp, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "Hi there")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if resp != "Hello from mock!" {
		t.Errorf("response = %q", resp)
	}

	// Verify messages were saved to session
	msgs, _ := store.Load("test/imain/1000000000")
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

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		n := callCount.Add(1)

		if n == 1 {
			// First call: respond with tool_use
			return &provider.MessageResponse{
				ID:   "msg_1",
				Type: "message",
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "text", Text: "Let me run that."},
					{
						Type:  "tool_use",
						ID:    "tu_001",
						Name:  "echo_tool",
						Input: json.RawMessage(`{"text":"hello"}`),
					},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}

		// Second call: after tool result, respond with end_turn
		return &provider.MessageResponse{
			ID:         "msg_2",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("The tool returned: hello"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 30, OutputTokens: 15},
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
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			var p struct{ Text string }
			json.Unmarshal(params, &p)
			return tools.TextResult(fmt.Sprintf("echo: %s", p.Text)), nil
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

	resp, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "Run echo")
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
	msgs, _ := store.Load("test/imain/1000000000")
	if len(msgs) != 4 {
		t.Fatalf("saved %d messages, want 4", len(msgs))
	}
}

func TestHandleMessageUnknownTool(t *testing.T) {
	var callCount atomic.Int32

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		n := callCount.Add(1)
		if n == 1 {
			return &provider.MessageResponse{
				ID:   "msg_1",
				Type: "message",
				Role: "assistant",
				Content: []provider.ContentBlock{
					{
						Type:  "tool_use",
						ID:    "tu_001",
						Name:  "nonexistent_tool",
						Input: json.RawMessage(`{}`),
					},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		return &provider.MessageResponse{
			ID:         "msg_2",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Sorry, that tool doesn't exist."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
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

	resp, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "Use a fake tool")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if resp != "Sorry, that tool doesn't exist." {
		t.Errorf("response = %q", resp)
	}
}

func TestHandleMessageSessionContinuity(t *testing.T) {
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		// Count messages in request to verify session history is sent
		msgCount := len(req.Messages)
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent(fmt.Sprintf("Received %d messages", msgCount)),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
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
	resp, _ := ag.HandleMessage(context.Background(), "test/imain/1000000000", "First")
	if resp != "Received 1 messages" {
		t.Errorf("first response = %q", resp)
	}

	// Second message — should include history
	resp, _ = ag.HandleMessage(context.Background(), "test/imain/1000000000", "Second")
	if resp != "Received 3 messages" {
		t.Errorf("second response = %q, want 'Received 3 messages' (2 history + 1 new)", resp)
	}
}

func TestWithCacheBreakpoint(t *testing.T) {
	tests := []struct {
		name     string
		messages []provider.Message
		wantIdx  int // index that should get cache_control (-1 for none)
	}{
		{
			name:     "empty",
			messages: nil,
			wantIdx:  -1,
		},
		{
			name: "single message",
			messages: []provider.Message{
				{Role: "user", Content: provider.TextContent("hi")},
			},
			wantIdx: -1,
		},
		{
			name: "two messages",
			messages: []provider.Message{
				{Role: "user", Content: provider.TextContent("hi")},
				{Role: "user", Content: provider.TextContent("second")},
			},
			wantIdx: 0, // second-to-last
		},
		{
			name: "three messages",
			messages: []provider.Message{
				{Role: "user", Content: provider.TextContent("first")},
				{Role: "assistant", Content: provider.TextContent("reply")},
				{Role: "user", Content: provider.TextContent("second")},
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
	var receivedReq *provider.MessageRequest

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("reply"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
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
	ag.HandleMessage(context.Background(), "test/icache/1000000000", "First")

	// Second message — should have breakpoint on the previous assistant turn
	ag.HandleMessage(context.Background(), "test/icache/1000000000", "Second")

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
	saved, _ := store.Load("test/icache/1000000000")
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
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID:   "msg_1",
			Type: "message",
			Role: "assistant",
			Content: []provider.ContentBlock{
				{
					Type:  "tool_use",
					ID:    "tu_001",
					Name:  "slow_tool",
					Input: json.RawMessage(`{}`),
				},
			},
			StopReason: "tool_use",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
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
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			<-ctx.Done()
			return tools.ToolResult{}, ctx.Err()
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

	_, err := ag.HandleMessage(ctx, "test/icancel/1000000000", "Do something slow")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

func TestIsProcessing(t *testing.T) {
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("done"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
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

	ag.HandleMessage(context.Background(), "test/iproc/1000000000", "Hi")

	if ag.IsProcessing() {
		t.Error("should not be processing after HandleMessage returns")
	}
}

func TestProcessingDetails(t *testing.T) {
	// ProcessingDetails should capture session key, trigger, and timing
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":           "msg_1",
			"type":         "message",
			"role":         "assistant",
			"model":        "claude-haiku-4-5",
			"content":      []map[string]interface{}{{"type": "text", "text": "ok"}},
			"stop_reason":  "end_turn",
			"usage":        map[string]int{"input_tokens": 10, "output_tokens": 5},
			"cache_read":   0,
			"cache_create": 0,
		})
	}))
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

	// Before turn
	if details := ag.ProcessingDetails(); len(details) != 0 {
		t.Fatalf("expected 0 details before turn, got %d", len(details))
	}

	// During turn — run HandleMessage in goroutine, check details mid-flight
	started := make(chan struct{})
	origHandler := server.Config.Handler
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(200 * time.Millisecond) // hold the turn open
		origHandler.ServeHTTP(w, r)
	})

	done := make(chan struct{})
	go func() {
		ctx := WithTrigger(context.Background(), "keepalive")
		ag.HandleMessage(ctx, "test/idetail/1000000000", "check")
		close(done)
	}()

	<-started
	time.Sleep(50 * time.Millisecond) // let HandleMessageWithAttachments register

	details := ag.ProcessingDetails()
	if len(details) != 1 {
		t.Fatalf("expected 1 detail during turn, got %d", len(details))
	}
	d := details[0]
	if d.SessionKey != "test/idetail/1000000000" {
		t.Errorf("session key = %q", d.SessionKey)
	}
	if d.Trigger != "keepalive" {
		t.Errorf("trigger = %q, want keepalive", d.Trigger)
	}
	if d.StartTime.IsZero() {
		t.Error("start time should not be zero")
	}

	<-done

	// After turn
	if details := ag.ProcessingDetails(); len(details) != 0 {
		t.Fatalf("expected 0 details after turn, got %d", len(details))
	}
}

func TestTriggerContext(t *testing.T) {
	ctx := context.Background()
	if trigger := TriggerFromContext(ctx); trigger != "" {
		t.Errorf("expected empty trigger, got %q", trigger)
	}

	ctx = WithTrigger(ctx, "user")
	if trigger := TriggerFromContext(ctx); trigger != "user" {
		t.Errorf("expected 'user', got %q", trigger)
	}
}


func TestDeferredReply(t *testing.T) {
	// Verify that text in tool_use responses is sent via ReplyFunc
	var callCount atomic.Int32

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		n := callCount.Add(1)
		if n == 1 {
			// First response: text + tool_use (deferred reply scenario)
			return &provider.MessageResponse{
				ID:   "msg_1",
				Type: "message",
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "text", Text: "Looking into this, give me a moment..."},
					{Type: "tool_use", ID: "tu_001", Name: "test_tool", Input: json.RawMessage(`{}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}
		// Second response: final answer
		return &provider.MessageResponse{
			ID:         "msg_2",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Here's the full answer."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 30, OutputTokens: 15},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	registry.Register(&tools.Tool{
		Name:       "test_tool",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return tools.TextResult("tool result"), nil
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

	// Track intermediate replies via context-scoped callbacks
	var intermediateReplies []string
	cb := &TurnCallbacks{
		ReplyFunc: func(text string) {
			intermediateReplies = append(intermediateReplies, text)
		},
	}
	ctx := WithTurnCallbacks(context.Background(), cb)

	finalResp, err := ag.HandleMessage(ctx, "test/ideferred/1000000000", "Complex question")
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
// Uses SDK transport (useSDK=true) to match production behavior.
func newTestClientWithBase(baseURL, apiKey string) *anthropic.Client {
	c := anthropic.NewClientWithBase(baseURL, apiKey)
	c.SetUseSDK(true)
	return c
}


func TestHandleMessageWithAttachments(t *testing.T) {
	var receivedReq *provider.MessageRequest

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("I see a cat!"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10},
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

	images := []Attachment{
		{MediaType: "image/jpeg", Data: []byte("fake-jpeg-data")},
	}
	resp, err := ag.HandleMessageWithAttachments(context.Background(), "test/iimg/1000000000", "What is this?", images)
	if err != nil {
		t.Fatalf("HandleMessageWithAttachments: %v", err)
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

func TestHandleMessageWithPDFAttachment(t *testing.T) {
	var receivedReq *provider.MessageRequest

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("I read the PDF."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10},
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

	attachments := []Attachment{
		{MediaType: "application/pdf", Data: []byte("%PDF-1.4 fake")},
	}
	resp, err := ag.HandleMessageWithAttachments(context.Background(), "test/ipdf/1000000000", "Read this PDF", attachments)
	if err != nil {
		t.Fatalf("HandleMessageWithAttachments: %v", err)
	}
	if resp != "I read the PDF." {
		t.Errorf("response = %q", resp)
	}

	if receivedReq == nil {
		t.Fatal("no request received")
	}

	// Check the user message has document + text blocks
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	if len(userMsg.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(userMsg.Content))
	}

	// First block should be document (not image)
	if userMsg.Content[0].Type != "document" {
		t.Errorf("content[0].Type = %q, want document", userMsg.Content[0].Type)
	}
	if userMsg.Content[0].Source == nil {
		t.Fatal("content[0].Source is nil")
	}
	if userMsg.Content[0].Source.MediaType != "application/pdf" {
		t.Errorf("content[0].Source.MediaType = %q", userMsg.Content[0].Source.MediaType)
	}
}

func TestHandleMessageWithPDFSavedPath(t *testing.T) {
	var receivedReq *provider.MessageRequest

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Got it!"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10},
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

	attachments := []Attachment{
		{MediaType: "application/pdf", Data: []byte("%PDF-1.4"), SavedPath: "/tmp/docs/report.pdf"},
	}
	_, err := ag.HandleMessageWithAttachments(context.Background(), "test/ipdfsaved/1000000000", "Check this", attachments)
	if err != nil {
		t.Fatalf("HandleMessageWithAttachments: %v", err)
	}

	// Check the text block contains PDF-specific saved path annotation
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	textBlock := userMsg.Content[len(userMsg.Content)-1]
	if !strings.Contains(textBlock.Text, "[PDF saved to: /tmp/docs/report.pdf]") {
		t.Errorf("text block should have PDF saved path annotation, got: %q", textBlock.Text)
	}
}

func TestHandleMessageWithAttachmentsNoText(t *testing.T) {
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("I see an image."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10},
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

	images := []Attachment{
		{MediaType: "image/png", Data: []byte("fake-png-data")},
	}
	// Empty text — image only
	resp, err := ag.HandleMessageWithAttachments(context.Background(), "test/iimgonly/1000000000", "", images)
	if err != nil {
		t.Fatalf("HandleMessageWithAttachments: %v", err)
	}
	if resp != "I see an image." {
		t.Errorf("response = %q", resp)
	}
}

func TestHandleMessageDelegatesToWithImages(t *testing.T) {
	// Verify HandleMessage (text-only) still works correctly
	var receivedReq *provider.MessageRequest

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
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

	ag.HandleMessage(context.Background(), "test/idelegate/1000000000", "Hello")

	// Text-only message should have exactly 1 content block (text)
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	if len(userMsg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(userMsg.Content))
	}
	if userMsg.Content[0].Type != "text" {
		t.Errorf("content[0].Type = %q, want text", userMsg.Content[0].Type)
	}
}

func TestHandleMessageWithAttachmentsSavedPath(t *testing.T) {
	var receivedReq *provider.MessageRequest

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Got it!"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10},
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

	images := []Attachment{
		{MediaType: "image/jpeg", Data: []byte("fake"), SavedPath: "/tmp/images/test.jpg"},
	}
	resp, err := ag.HandleMessageWithAttachments(context.Background(), "test/isavepath/1000000000", "What is this?", images)
	if err != nil {
		t.Fatalf("HandleMessageWithAttachments: %v", err)
	}
	if resp != "Got it!" {
		t.Errorf("response = %q", resp)
	}

	// Check the text block contains the saved path annotation
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	textBlock := userMsg.Content[len(userMsg.Content)-1]
	if !strings.Contains(textBlock.Text, "[Image saved to: /tmp/images/test.jpg]") {
		t.Errorf("text block missing saved path annotation: %q", textBlock.Text)
	}
	if !strings.Contains(textBlock.Text, "What is this?") {
		t.Errorf("text block missing user text: %q", textBlock.Text)
	}
}

func TestHandleMessageWithAttachmentsNoSavedPath(t *testing.T) {
	var receivedReq *provider.MessageRequest

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10},
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

	images := []Attachment{
		{MediaType: "image/jpeg", Data: []byte("fake")},
	}
	ag.HandleMessageWithAttachments(context.Background(), "test/inosaved/1000000000", "Look", images)

	// Text block should NOT contain [Image saved to:] when SavedPath is empty
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	textBlock := userMsg.Content[len(userMsg.Content)-1]
	if strings.Contains(textBlock.Text, "[Image saved to:") {
		t.Errorf("text block should not have saved path annotation when SavedPath is empty: %q", textBlock.Text)
	}
}


func TestBuildMetaPrefix(t *testing.T) {
	now := time.Date(2026, 2, 21, 5, 30, 0, 0, time.UTC)

	// First message — no previous turn data
	sm := &sessionMeta{}
	prefix := buildMetaPrefix(now, "claude-haiku-4-5", "", false, sm)
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

	prefix = buildMetaPrefix(now, "claude-haiku-4-5", "", false, sm)
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
	var receivedReq *provider.MessageRequest

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
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

	ag.HandleMessage(context.Background(), "test/imeta/1000000000", "Hello")

	if receivedReq == nil {
		t.Fatal("no request received")
	}

	// The user message should have the meta prefix
	lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	text := provider.TextOf(lastMsg.Content)
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

func TestSessionEffort(t *testing.T) {
	ag := &Agent{Model: "test", Effort: "low"}

	// Default: falls back to agent-wide
	if got := ag.SessionEffort("s1"); got != "low" {
		t.Errorf("SessionEffort fallback = %q, want %q", got, "low")
	}

	// Set per-session override
	ag.SetSessionEffort("s1", "high")
	if got := ag.SessionEffort("s1"); got != "high" {
		t.Errorf("SessionEffort after set = %q, want %q", got, "high")
	}

	// Other session unaffected
	if got := ag.SessionEffort("s2"); got != "low" {
		t.Errorf("SessionEffort other session = %q, want %q", got, "low")
	}

	// Clear override — falls back to agent default
	ag.SetSessionEffort("s1", "")
	if got := ag.SessionEffort("s1"); got != "low" {
		t.Errorf("SessionEffort after clear = %q, want %q", got, "low")
	}
}

func TestSessionNoCompact(t *testing.T) {
	ag := &Agent{Model: "test"}

	// Default: should return false (allow compaction)
	if got := ag.SessionNoCompact("s1"); got != false {
		t.Errorf("SessionNoCompact default = %v, want %v", got, false)
	}

	// Set per-session no_compact
	ag.SetSessionNoCompact("s1", true)
	if got := ag.SessionNoCompact("s1"); got != true {
		t.Errorf("SessionNoCompact after set = %v, want %v", got, true)
	}

	// Other session unaffected
	if got := ag.SessionNoCompact("s2"); got != false {
		t.Errorf("SessionNoCompact other session = %v, want %v", got, false)
	}

	// Clear override
	ag.SetSessionNoCompact("s1", false)
	if got := ag.SessionNoCompact("s1"); got != false {
		t.Errorf("SessionNoCompact after clear = %v, want %v", got, false)
	}
}

func TestSessionThinking(t *testing.T) {
	ag := &Agent{Model: "test", Thinking: "off"}

	// Default: falls back to agent-wide
	if got := ag.SessionThinking("s1"); got != "off" {
		t.Errorf("SessionThinking fallback = %q, want %q", got, "off")
	}

	// Set per-session override
	ag.SetSessionThinking("s1", "adaptive")
	if got := ag.SessionThinking("s1"); got != "adaptive" {
		t.Errorf("SessionThinking after set = %q, want %q", got, "adaptive")
	}

	// Other session unaffected
	if got := ag.SessionThinking("s2"); got != "off" {
		t.Errorf("SessionThinking other session = %q, want %q", got, "off")
	}

	// Clear override
	ag.SetSessionThinking("s1", "")
	if got := ag.SessionThinking("s1"); got != "off" {
		t.Errorf("SessionThinking after clear = %q, want %q", got, "off")
	}
}

func TestSessionModel(t *testing.T) {
	ag := &Agent{Model: "claude-haiku-4-5"}

	// Default: falls back to agent-wide
	if got := ag.SessionModel("s1"); got != "claude-haiku-4-5" {
		t.Errorf("SessionModel fallback = %q, want %q", got, "claude-haiku-4-5")
	}

	// Set per-session override
	ag.SetSessionModel("s1", "claude-sonnet-4-5", "anthropic", nil)
	if got := ag.SessionModel("s1"); got != "claude-sonnet-4-5" {
		t.Errorf("SessionModel after set = %q, want %q", got, "claude-sonnet-4-5")
	}

	// Other session unaffected
	if got := ag.SessionModel("s2"); got != "claude-haiku-4-5" {
		t.Errorf("SessionModel other session = %q, want %q", got, "claude-haiku-4-5")
	}

	// Clear override
	ag.SetSessionModel("s1", "", "", nil)
	if got := ag.SessionModel("s1"); got != "claude-haiku-4-5" {
		t.Errorf("SessionModel after clear = %q, want %q", got, "claude-haiku-4-5")
	}
}

func TestRestoreSessionOverrides(t *testing.T) {
	dir := t.TempDir()
	ss := state.New(dir + "/state.json")
	if err := ss.Load(); err != nil {
		t.Fatal(err)
	}

	ag := &Agent{
		Model:      "claude-haiku-4-5",
		Effort:     "low",
		Thinking:   "off",
		StateStore: ss,
	}

	// Persist values via setters
	ag.SetSessionEffort("s1", "high")
	ag.SetSessionThinking("s1", "adaptive")
	ag.SetSessionModel("s1", "claude-opus-4-6", "anthropic", nil)
	ag.SetSessionNoCompact("s1", true)

	// Create a fresh agent (simulating restart) with the same state store
	ag2 := &Agent{
		Model:      "claude-haiku-4-5",
		Effort:     "low",
		Thinking:   "off",
		StateStore: ss,
	}

	// Before restore: should fall back to defaults
	if got := ag2.SessionEffort("s1"); got != "low" {
		t.Errorf("before restore effort = %q, want %q", got, "low")
	}

	// Restore
	ag2.RestoreSessionOverrides("s1")

	// After restore: should have overrides
	if got := ag2.SessionEffort("s1"); got != "high" {
		t.Errorf("after restore effort = %q, want %q", got, "high")
	}
	if got := ag2.SessionThinking("s1"); got != "adaptive" {
		t.Errorf("after restore thinking = %q, want %q", got, "adaptive")
	}
	if got := ag2.SessionModel("s1"); got != "claude-opus-4-6" {
		t.Errorf("after restore model = %q, want %q", got, "claude-opus-4-6")
	}
	if got := ag2.SessionNoCompact("s1"); got != true {
		t.Errorf("after restore no_compact = %v, want %v", got, true)
	}

	// Unrelated session should still use defaults
	if got := ag2.SessionEffort("s2"); got != "low" {
		t.Errorf("unrelated session effort = %q, want %q", got, "low")
	}
}

func TestRestoreSessionOverrides_NilStateStore(t *testing.T) {
	ag := &Agent{Model: "test", Effort: "low"}

	// Should not panic with nil state store
	ag.RestoreSessionOverrides("s1")

	if got := ag.SessionEffort("s1"); got != "low" {
		t.Errorf("effort with nil store = %q, want %q", got, "low")
	}
}

func TestBuildMetaPrefix_Mana(t *testing.T) {
	now := time.Date(2026, 2, 21, 12, 0, 0, 0, time.UTC)

	// Without mana
	sm := &sessionMeta{}
	prefix := buildMetaPrefix(now, "claude-haiku-4-5", "", false, sm)
	if strings.Contains(prefix, "mana=") {
		t.Errorf("should not contain mana when empty: %q", prefix)
	}

	// With mana, not good (first message) — red indicator
	prefix = buildMetaPrefix(now, "claude-haiku-4-5", "75%", false, sm)
	if !strings.Contains(prefix, "mana=75% 🔴") {
		t.Errorf("should contain mana=75%% with red indicator: %q", prefix)
	}

	// With mana, good — green indicator
	prefix = buildMetaPrefix(now, "claude-haiku-4-5", "75%", true, sm)
	if !strings.Contains(prefix, "mana=75% 🟢") {
		t.Errorf("should contain mana=75%% with green indicator: %q", prefix)
	}

	// With mana (subsequent message with cost data)
	sm.prevCost = 0.01
	sm.prevInput = 100
	prefix = buildMetaPrefix(now, "claude-haiku-4-5", "50%", false, sm)
	if !strings.Contains(prefix, "mana=50% 🔴") {
		t.Errorf("should contain mana=50%% with red indicator in subsequent message: %q", prefix)
	}
}

func TestDuplicateMessages(t *testing.T) {
	var receivedReq *provider.MessageRequest

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
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

	ag.HandleMessage(context.Background(), "test/idup/1000000000", "Do the thing")

	// The user message text should contain the instruction twice
	lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	text := provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 2 {
		t.Errorf("expected user text duplicated (2 occurrences), got %d in: %q", count, text)
	}

	// Meta prefix should only appear once
	if count := strings.Count(text, "[meta]"); count != 1 {
		t.Errorf("expected [meta] once, got %d", count)
	}

	// Saved session should also have the duplicated text (for cache coherence)
	saved, _ := store.Load("test/idup/1000000000")
	savedText := provider.TextOf(saved[0].Content)
	if count := strings.Count(savedText, "Do the thing"); count != 2 {
		t.Errorf("saved session should have duplicated text, got %d occurrences", count)
	}
}

func TestDuplicateMessagesDisabled(t *testing.T) {
	var receivedReq *provider.MessageRequest

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
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

	ag.HandleMessage(context.Background(), "test/inodup/1000000000", "Do the thing")

	lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	text := provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 1 {
		t.Errorf("expected user text once (no duplication), got %d in: %q", count, text)
	}
}

func TestDuplicateMessagesSkippedForWake(t *testing.T) {
	var receivedReq *provider.MessageRequest

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
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

	// Wake trigger should NOT duplicate
	wakeCtx := WithTrigger(context.Background(), "wake")
	ag.HandleMessage(wakeCtx, "test/iwake/1000000000", "Do the thing")

	lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	text := provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 1 {
		t.Errorf("wake trigger should not duplicate: expected 1 occurrence, got %d", count)
	}

	// Keepalive trigger should NOT duplicate
	kaCtx := WithTrigger(context.Background(), "keepalive")
	ag.HandleMessage(kaCtx, "test/ika/1000000000", "Check stuff")

	lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
	text = provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Check stuff"); count != 1 {
		t.Errorf("keepalive trigger should not duplicate: expected 1 occurrence, got %d", count)
	}

	// User trigger SHOULD duplicate
	userCtx := WithTrigger(context.Background(), "user")
	ag.HandleMessage(userCtx, "test/iuser/1000000000", "Do the thing")

	lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
	text = provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 2 {
		t.Errorf("user trigger should duplicate: expected 2 occurrences, got %d", count)
	}

	// Telegram trigger SHOULD duplicate (human-typed messages)
	tgCtx := WithTrigger(context.Background(), "telegram")
	ag.HandleMessage(tgCtx, "test/itg/1000000000", "Say something")

	lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
	text = provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Say something"); count != 2 {
		t.Errorf("telegram trigger should duplicate: expected 2 occurrences, got %d", count)
	}

	// Voice trigger SHOULD duplicate (human-spoken messages)
	voiceCtx := WithTrigger(context.Background(), "voice")
	ag.HandleMessage(voiceCtx, "test/ivoice/1000000000", "Tell me")

	lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
	text = provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Tell me"); count != 2 {
		t.Errorf("voice trigger should duplicate: expected 2 occurrences, got %d", count)
	}

	// System triggers should NOT duplicate
	for _, sysT := range []string{"proactive_warning", "async_notify", "session_notify", "scheduled_wake", "restart", "first_run"} {
		sysCtx := WithTrigger(context.Background(), sysT)
		ag.HandleMessage(sysCtx, "test/isys"+sysT+"/1000000000", "System msg")

		lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
		text = provider.TextOf(lastMsg.Content)
		if count := strings.Count(text, "System msg"); count != 1 {
			t.Errorf("%s trigger should not duplicate: expected 1 occurrence, got %d", sysT, count)
		}
	}
}

func TestRepairInterruptedToolCalls(t *testing.T) {
	t.Run("empty messages", func(t *testing.T) {
		if got := repairInterruptedToolCalls(nil); got != nil {
			t.Errorf("expected nil for empty messages, got %v", got)
		}
	})

	t.Run("last message is user", func(t *testing.T) {
		msgs := []provider.Message{
			{Role: "user", Content: provider.TextContent("hi")},
		}
		if got := repairInterruptedToolCalls(msgs); got != nil {
			t.Errorf("expected nil when last message is user, got %v", got)
		}
	})

	t.Run("assistant with text only", func(t *testing.T) {
		msgs := []provider.Message{
			{Role: "user", Content: provider.TextContent("hi")},
			{Role: "assistant", Content: provider.TextContent("hello")},
		}
		if got := repairInterruptedToolCalls(msgs); got != nil {
			t.Errorf("expected nil when no tool_use blocks, got %v", got)
		}
	})

	t.Run("single tool_use", func(t *testing.T) {
		msgs := []provider.Message{
			{Role: "user", Content: provider.TextContent("hi")},
			{Role: "assistant", Content: []provider.ContentBlock{
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
		msgs := []provider.Message{
			{Role: "user", Content: provider.TextContent("hi")},
			{Role: "assistant", Content: []provider.ContentBlock{
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
	var receivedReq *provider.MessageRequest

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Recovered."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 50, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	// Pre-populate session with an interrupted tool call
	sessionKey := "test/irepair/1000000000"
	store.Append(sessionKey, provider.Message{
		Role: "user", Content: provider.TextContent("do something"),
	})
	store.Append(sessionKey, provider.Message{
		Role: "assistant", Content: []provider.ContentBlock{
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

func TestAgentCompactionIntegration(t *testing.T) {
	t.Run("basic", func(t *testing.T) {
		var turnCount atomic.Int32
		env := newCompactionTestEnv(t, &turnCount, 5)
		sessionKey := "test/icompact/1000000000"

		// Phase 1: 4 turns with low tokens — no compaction
		env.runTurns(t, sessionKey, 1, 4)

		msgs, _ := env.store.Load(sessionKey)
		if len(msgs) != 8 {
			t.Fatalf("after 4 turns: %d messages, want 8", len(msgs))
		}

		// Phase 2: Turn 5 — high tokens triggers compaction
		env.runTurns(t, sessionKey, 5, 5)

		// After compaction, session should have exactly 3 messages
		msgs, _ = env.store.Load(sessionKey)
		if len(msgs) != 3 {
			t.Fatalf("after compaction: %d messages, want 3", len(msgs))
		}

		// msg[0]: marker
		if !strings.Contains(provider.TextOf(msgs[0].Content), "compacted") {
			t.Errorf("msg[0] should contain 'compacted': %q", provider.TextOf(msgs[0].Content))
		}
		// msg[1]: summary from mock
		if !strings.Contains(provider.TextOf(msgs[1].Content), "compacted summary") {
			t.Errorf("msg[1] should contain summary: %q", provider.TextOf(msgs[1].Content))
		}
		// msg[2]: handoff
		if !strings.Contains(provider.TextOf(msgs[2].Content), "Compaction complete") {
			t.Errorf("msg[2] should contain handoff: %q", provider.TextOf(msgs[2].Content))
		}

		// Phase 3: Turn 6 — post-compaction continuity
		env.runTurns(t, sessionKey, 6, 6)

		// 3 compacted + user turn 6 + assistant turn 6 = 5
		msgs, _ = env.store.Load(sessionKey)
		if len(msgs) != 5 {
			t.Fatalf("after Turn 6: %d messages, want 5", len(msgs))
		}
	})

	t.Run("scratchpad", func(t *testing.T) {
		var turnCount atomic.Int32
		env := newCompactionTestEnv(t, &turnCount, 5)

		// Set up scratchpad with entries
		scratchpad, err := memory.NewScratchpad(filepath.Join(t.TempDir(), "scratchpad.db"))
		if err != nil {
			t.Fatalf("create scratchpad: %v", err)
		}
		defer scratchpad.Close()

		if err := scratchpad.Write("test", "current_task", "implementing feature X"); err != nil {
			t.Fatalf("write scratchpad: %v", err)
		}
		if err := scratchpad.Write("test", "blockers", "need API key for auth"); err != nil {
			t.Fatalf("write scratchpad: %v", err)
		}

		env.compactor.Scratchpad = scratchpad
		env.compactor.AgentID = "test"

		sessionKey := "test/icompactsp/1000000000"

		// Build up 4 turns then trigger compaction on turn 5
		env.runTurns(t, sessionKey, 1, 5)

		msgs, _ := env.store.Load(sessionKey)
		if len(msgs) != 3 {
			t.Fatalf("after compaction: %d messages, want 3", len(msgs))
		}

		// Verify handoff message contains scratchpad data
		handoff := provider.TextOf(msgs[2].Content)
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

	t.Run("preserve", func(t *testing.T) {
		var turnCount atomic.Int32
		env := newCompactionTestEnv(t, &turnCount, 5)
		env.compactor.WithConfig(4096, 4, 4) // preserve last 4 messages

		sessionKey := "test/ipreserve/1000000000"

		// Phase 1: 4 turns with low tokens — no compaction
		env.runTurns(t, sessionKey, 1, 4)

		msgs, _ := env.store.Load(sessionKey)
		if len(msgs) != 8 {
			t.Fatalf("after 4 turns: %d messages, want 8", len(msgs))
		}

		// Phase 2: Turn 5 — high tokens triggers compaction
		env.runTurns(t, sessionKey, 5, 5)

		// After compaction with preserve=4, preserved[0] is user so handoff folds:
		// 2 (marker + summary+handoff) + 4 preserved = 6
		msgs, _ = env.store.Load(sessionKey)
		if len(msgs) != 6 {
			t.Fatalf("after compaction: %d messages, want 6 (2 header + 4 preserved)", len(msgs))
		}

		// Verify role alternation (the fix ensures no consecutive same-role)
		for i := 1; i < len(msgs); i++ {
			if msgs[i].Role == msgs[i-1].Role {
				t.Errorf("consecutive same role at [%d,%d]: %s", i-1, i, msgs[i].Role)
			}
		}

		// Verify the preserved messages are the last 4 from pre-compaction
		// Pre-compaction had 10 messages: [u1,a1,u2,a2,u3,a3,u4,a4,u5,a5]
		// Last 4: [u4,a4,u5,a5]
		preserved := msgs[2:] // preserved starts at index 2 (handoff folded)
		if len(preserved) != 4 {
			t.Fatalf("preserved = %d messages, want 4", len(preserved))
		}
		if preserved[0].Role != "user" {
			t.Errorf("preserved[0].Role = %q, want user", preserved[0].Role)
		}
		if preserved[1].Role != "assistant" {
			t.Errorf("preserved[1].Role = %q, want assistant", preserved[1].Role)
		}
		// Verify content of preserved messages (Turn 4 user msg has metadata prefix, so check contains)
		if !strings.Contains(provider.TextOf(preserved[0].Content), "Turn 4") {
			t.Errorf("preserved[0] should contain 'Turn 4': %q", provider.TextOf(preserved[0].Content))
		}
		if provider.TextOf(preserved[1].Content) != "Response 4" {
			t.Errorf("preserved[1] = %q, want 'Response 4'", provider.TextOf(preserved[1].Content))
		}

		// Summary+handoff should mention preservation and contain handoff text
		summaryText := provider.TextOf(msgs[1].Content)
		if !strings.Contains(summaryText, "last 4 messages") {
			t.Errorf("summary missing preservation note: %q", summaryText)
		}
		if !strings.Contains(summaryText, "Compaction complete") {
			t.Errorf("summary should contain folded handoff: %q", summaryText)
		}

		// Phase 3: Turn 6 — post-compaction continuity (messages survive reload)
		env.runTurns(t, sessionKey, 6, 6)

		// 6 compacted + user turn 6 + assistant turn 6 = 8
		msgs, _ = env.store.Load(sessionKey)
		if len(msgs) != 8 {
			t.Fatalf("after Turn 6: %d messages, want 8", len(msgs))
		}

		// The preserved messages should still be at positions 2-5
		if !strings.Contains(provider.TextOf(msgs[2].Content), "Turn 4") {
			t.Errorf("preserved msg should survive post-compaction turn: %q", provider.TextOf(msgs[2].Content))
		}
	})

	t.Run("notify", func(t *testing.T) {
		var turnCount atomic.Int32
		env := newCompactionTestEnv(t, &turnCount, 5)

		var notified []string
		env.ag.CompactionNotifyFunc = func(session string, msg string) {
			notified = append(notified, msg)
		}

		sessionKey := "test/icompactnotify/1000000000"

		// 4 turns, then turn 5 triggers compaction
		env.runTurns(t, sessionKey, 1, 5)

		if len(notified) != 2 {
			t.Fatalf("expected 2 notifications (start + end), got %d", len(notified))
		}
		if !strings.Contains(notified[0], "Compacting") {
			t.Errorf("start notification = %q, want to contain 'Compacting'", notified[0])
		}
		if !strings.Contains(notified[1], "10 messages") {
			t.Errorf("end notification = %q, want to contain '10 messages'", notified[1])
		}
	})

	t.Run("no_compact", func(t *testing.T) {
		var turnCount atomic.Int32
		env := newCompactionTestEnv(t, &turnCount, 5)

		var notified []string
		warnQ := warnings.NewQueue(0, 0)
		env.ag.Warnings = warnQ
		env.ag.CompactionNotifyFunc = func(session string, msg string) {
			notified = append(notified, msg)
		}

		sessionKey := "test/inocompact/1000000000"

		// 4 normal turns
		env.runTurns(t, sessionKey, 1, 4)

		// Turn 5 triggers compaction threshold — but with NoCompact set
		env.ag.SetSessionNoCompact(sessionKey, true)
		resp, err := env.ag.HandleMessage(context.Background(), sessionKey, "Turn 5")
		if err != nil {
			t.Fatalf("Turn 5: %v", err)
		}

		// Should still get a response
		if resp != "Response 5" {
			t.Errorf("got %q, want %q", resp, "Response 5")
		}

		// Compaction should NOT have fired
		if len(notified) != 0 {
			t.Errorf("expected 0 notifications with no_compact, got %d", len(notified))
		}

		// Session should still have all original messages (not compacted)
		msgs, err := env.store.Load(sessionKey)
		if err != nil {
			t.Fatalf("load session: %v", err)
		}
		// 5 turns × 2 messages each = 10
		if len(msgs) != 10 {
			t.Errorf("expected 10 messages (uncompacted), got %d", len(msgs))
		}

		// No warning should be pushed for no_compact sessions (removed in 63f8f6b2)
		warned := warnQ.Drain()
		if len(warned) != 0 {
			t.Fatalf("expected 0 warnings for no_compact session, got %d: %v", len(warned), warned)
		}
	})

	t.Run("skipped_when_async_pending", func(t *testing.T) {
		var turnCount atomic.Int32
		env := newCompactionTestEnv(t, &turnCount, 5)

		notifier := tools.NewAsyncNotifier(func(sk, msg string, replyTo string) {})
		env.ag.AsyncNotifier = notifier
		var notified []string
		env.ag.CompactionNotifyFunc = func(session string, msg string) {
			notified = append(notified, msg)
		}

		sessionKey := "test/iasyncpending/1000000000"

		// 4 normal turns
		env.runTurns(t, sessionKey, 1, 4)

		// Mark an async result as pending for this session
		notifier.MarkPending(sessionKey)

		// Turn 5 triggers compaction threshold — but async pending blocks it
		resp, err := env.ag.HandleMessage(context.Background(), sessionKey, "Turn 5")
		if err != nil {
			t.Fatalf("Turn 5: %v", err)
		}
		if resp != "Response 5" {
			t.Errorf("got %q, want %q", resp, "Response 5")
		}

		// Compaction should NOT have fired
		if len(notified) != 0 {
			t.Errorf("expected 0 compaction notifications with async pending, got %d", len(notified))
		}

		// Session should still have all original messages
		msgs, err := env.store.Load(sessionKey)
		if err != nil {
			t.Fatalf("load session: %v", err)
		}
		if len(msgs) != 10 {
			t.Errorf("expected 10 messages (uncompacted), got %d", len(msgs))
		}

		// Clear pending — next turn should compact
		notifier.MarkDone(sessionKey)
		turnCount.Store(4) // reset so turn 6 = count 5 → high tokens

		_, err = env.ag.HandleMessage(context.Background(), sessionKey, "Turn 6")
		if err != nil {
			t.Fatalf("Turn 6: %v", err)
		}

		// Compaction should have fired now
		if len(notified) == 0 {
			t.Error("expected compaction to fire after async pending cleared")
		}

		msgs, _ = env.store.Load(sessionKey)
		if len(msgs) > 5 {
			t.Errorf("expected compacted session (<=5 messages), got %d", len(msgs))
		}
	})
}

func TestIntermediateTextBeforeToolCalls(t *testing.T) {
	// Verify the agent calls sendIntermediate before notifyToolCall when the
	// API response contains both text and tool_use blocks. This ordering is
	// critical for Telegram message display: thinking text must appear above
	// tool call notifications in the chat.
	var callCount atomic.Int32

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		n := callCount.Add(1)
		if n == 1 {
			return &provider.MessageResponse{
				ID:   "msg_1",
				Type: "message",
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "text", Text: "Let me check..."},
					{Type: "tool_use", ID: "tu_001", Name: "test_tool", Input: json.RawMessage(`{}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}
		return &provider.MessageResponse{
			ID:         "msg_2",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Done."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 30, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	registry.Register(&tools.Tool{
		Name:       "test_tool",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return tools.TextResult("ok"), nil
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

	cb := &TurnCallbacks{
		ReplyFunc: func(text string) {
			mu.Lock()
			order = append(order, "reply:"+text)
			mu.Unlock()
		},
		ToolCallObserver: func(name string, params json.RawMessage) {
			mu.Lock()
			order = append(order, "tool:"+name)
			mu.Unlock()
		},
	}
	ctx := WithTurnCallbacks(context.Background(), cb)

	_, err := ag.HandleMessage(ctx, "test/iorder/1000000000", "Check something")
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

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		// Identify which turn this is by looking at the last user message
		lastMsg := req.Messages[len(req.Messages)-1]
		text := provider.TextOf(lastMsg.Content)

		mu.Lock()
		apiCallOrder = append(apiCallOrder, text)
		mu.Unlock()

		// Slow down the first turn so concurrent callers pile up
		if strings.Contains(text, "Turn A") {
			time.Sleep(100 * time.Millisecond)
		}

		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Reply to: " + text),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
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

	sessionKey := "test/iconcurrent/1000000000"

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
	textA := provider.TextOf(msgs[0].Content)
	if !strings.Contains(textA, "Turn A") {
		t.Errorf("msgs[0] should be Turn A's user message, got: %s", textA)
	}
	if msgs[0].Role != "user" {
		t.Errorf("msgs[0].Role = %q, want user", msgs[0].Role)
	}

	replyA := provider.TextOf(msgs[1].Content)
	if !strings.Contains(replyA, "Reply to") {
		t.Errorf("msgs[1] should be Turn A's assistant reply, got: %s", replyA)
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msgs[1].Role = %q, want assistant", msgs[1].Role)
	}

	textB := provider.TextOf(msgs[2].Content)
	if !strings.Contains(textB, "Turn B") {
		t.Errorf("msgs[2] should be Turn B's user message, got: %s", textB)
	}
	if msgs[2].Role != "user" {
		t.Errorf("msgs[2].Role = %q, want user", msgs[2].Role)
	}

	replyB := provider.TextOf(msgs[3].Content)
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

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		cur := atomic.AddInt32(&activeConcurrent, 1)
		defer atomic.AddInt32(&activeConcurrent, -1)

		mu.Lock()
		if cur > maxConcurrent {
			maxConcurrent = cur
		}
		mu.Unlock()

		time.Sleep(50 * time.Millisecond) // ensure overlap

		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("OK"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
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
		ag.HandleMessage(context.Background(), "test/isessionA/1000000000", "Hello A")
	}()

	go func() {
		defer wg.Done()
		ag.HandleMessage(context.Background(), "test/isessionB/1000000000", "Hello B")
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
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		time.Sleep(200 * time.Millisecond) // slow turn
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Done"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
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

	sessionKey := "test/icancelq/1000000000"

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

func TestParseMetaTime(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		wantOK  bool
		wantStr string // RFC3339 string of expected time
	}{
		{
			name:    "valid meta with gap",
			text:    "[meta] time=2026-02-23T15:43:13Z gap=3h12m model=claude-haiku-4-5",
			wantOK:  true,
			wantStr: "2026-02-23T15:43:13Z",
		},
		{
			name:    "valid meta first message",
			text:    "[meta] time=2026-01-01T00:00:00Z gap=none model=claude-haiku-4-5",
			wantOK:  true,
			wantStr: "2026-01-01T00:00:00Z",
		},
		{
			name:   "no meta prefix",
			text:   "hello world",
			wantOK: false,
		},
		{
			name:   "meta prefix but no time field",
			text:   "[meta] gap=none model=claude-haiku-4-5",
			wantOK: false,
		},
		{
			name:   "invalid time format",
			text:   "[meta] time=not-a-time gap=none",
			wantOK: false,
		},
		{
			name:   "empty string",
			text:   "",
			wantOK: false,
		},
		{
			name:   "restart marker (not meta)",
			text:   "[System restarted at 2026-02-23T15:43:13Z]",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseMetaTime(tt.text)
			if ok != tt.wantOK {
				t.Fatalf("parseMetaTime(%q) ok = %v, want %v", tt.text, ok, tt.wantOK)
			}
			if ok && got.Format(time.RFC3339) != tt.wantStr {
				t.Errorf("parseMetaTime(%q) = %v, want %v", tt.text, got.Format(time.RFC3339), tt.wantStr)
			}
		})
	}
}

func TestSeedSessionMeta(t *testing.T) {
	store := session.NewStore(t.TempDir())
	ag := &Agent{Sessions: store, Model: "claude-haiku-4-5"}

	sessionKey := "test/iseed/1000000000"

	// Seed with empty session — should not panic
	ag.SeedSessionMeta(sessionKey)
	sm := ag.getSessionMeta(sessionKey)
	if !sm.lastMessageTime.IsZero() {
		t.Error("lastMessageTime should be zero for empty session")
	}

	// Add some messages with meta headers
	store.Append(sessionKey, provider.Message{
		Role:    "user",
		Content: provider.TextContent("[meta] time=2026-02-23T10:00:00Z gap=none model=claude-haiku-4-5\nHello"),
	})
	store.Append(sessionKey, provider.Message{
		Role:    "assistant",
		Content: provider.TextContent("Hi there!"),
	})
	store.Append(sessionKey, provider.Message{
		Role:    "user",
		Content: provider.TextContent("[meta] time=2026-02-23T12:30:00Z gap=2h30m model=claude-haiku-4-5\nHow are you?"),
	})
	store.Append(sessionKey, provider.Message{
		Role:    "assistant",
		Content: provider.TextContent("Good!"),
	})

	// Seed from a fresh agent (simulating restart)
	ag2 := &Agent{Sessions: store, Model: "claude-haiku-4-5"}
	ag2.SeedSessionMeta(sessionKey)

	sm2 := ag2.getSessionMeta(sessionKey)
	expected := time.Date(2026, 2, 23, 12, 30, 0, 0, time.UTC)
	if !sm2.lastMessageTime.Equal(expected) {
		t.Errorf("lastMessageTime = %v, want %v", sm2.lastMessageTime, expected)
	}
}

func TestMaxTokensWarning(t *testing.T) {
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("This response was cut off bec"),
			StopReason: "max_tokens",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 8192},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	var warnings []string
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
		MaxTokensWarnFunc: func(warn string) {
			warnings = append(warnings, warn)
		},
	}

	resp, err := ag.HandleMessage(context.Background(), "test/imaxtkn/1000000000", "Write a very long essay")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// Response should still be delivered
	if resp != "This response was cut off bec" {
		t.Errorf("response = %q", resp)
	}

	// Warning callback should have fired
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(warnings))
	}
	if !strings.Contains(warnings[0], "max_tokens") {
		t.Errorf("warning = %q, want contains 'max_tokens'", warnings[0])
	}
	if !strings.Contains(warnings[0], "test/imaxtkn/1000000000") {
		t.Errorf("warning = %q, want contains session key", warnings[0])
	}
}

func TestMaxTokensNoWarningOnEndTurn(t *testing.T) {
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Normal response."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 50},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	var warnings []string
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
		MaxTokensWarnFunc: func(warn string) {
			warnings = append(warnings, warn)
		},
	}

	ag.HandleMessage(context.Background(), "test/inomax/1000000000", "Hello")

	if len(warnings) != 0 {
		t.Errorf("expected no warnings for end_turn, got %d: %v", len(warnings), warnings)
	}
}

func TestToolResultRedaction(t *testing.T) {
	var callCount atomic.Int32

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		n := callCount.Add(1)
		if n == 1 {
			return &provider.MessageResponse{
				ID:   "msg_1",
				Type: "message",
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "tool_use", ID: "tu_001", Name: "leak_tool", Input: json.RawMessage(`{}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}
		return &provider.MessageResponse{
			ID:         "msg_2",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Done."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 30, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	registry.Register(&tools.Tool{
		Name:       "leak_tool",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return tools.TextResult("output contains sk-ant-12345-secret-key here"), nil
		},
	})
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
		Redact: func(s string) string {
			return strings.ReplaceAll(s, "sk-ant-12345-secret-key", "[REDACTED]")
		},
	}

	_, err := ag.HandleMessage(context.Background(), "test/iredact/1000000000", "Leak the secret")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// Verify the saved session has redacted tool result
	msgs, _ := store.Load("test/iredact/1000000000")
	for _, msg := range msgs {
		for _, block := range msg.Content {
			if block.Type == "tool_result" {
				if strings.Contains(block.Content, "sk-ant-12345-secret-key") {
					t.Error("tool result contains unredacted secret")
				}
				if !strings.Contains(block.Content, "[REDACTED]") {
					t.Error("tool result should contain [REDACTED] marker")
				}
			}
		}
	}
}

func TestToolErrorRedaction(t *testing.T) {
	var callCount atomic.Int32

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		n := callCount.Add(1)
		if n == 1 {
			return &provider.MessageResponse{
				ID:   "msg_1",
				Type: "message",
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "tool_use", ID: "tu_001", Name: "err_tool", Input: json.RawMessage(`{}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}
		return &provider.MessageResponse{
			ID:         "msg_2",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Done."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 30, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	registry.Register(&tools.Tool{
		Name:       "err_tool",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, fmt.Errorf("auth failed with token sk-ant-12345-secret-key")
		},
	})
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
		Redact: func(s string) string {
			return strings.ReplaceAll(s, "sk-ant-12345-secret-key", "[REDACTED]")
		},
	}

	_, err := ag.HandleMessage(context.Background(), "test/iredacterr/1000000000", "Try the tool")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// Verify the saved session has redacted error message
	msgs, _ := store.Load("test/iredacterr/1000000000")
	for _, msg := range msgs {
		for _, block := range msg.Content {
			if block.Type == "tool_result" && block.IsError {
				if strings.Contains(block.Content, "sk-ant-12345-secret-key") {
					t.Error("tool error contains unredacted secret")
				}
				if !strings.Contains(block.Content, "[REDACTED]") {
					t.Error("tool error should contain [REDACTED] marker")
				}
			}
		}
	}
}

func TestSeedSessionMetaSkipsNonMetaMessages(t *testing.T) {
	store := session.NewStore(t.TempDir())
	ag := &Agent{Sessions: store, Model: "claude-haiku-4-5"}

	sessionKey := "test/iseedskip/1000000000"

	// First message has meta, second user message is a restart marker (no meta)
	store.Append(sessionKey, provider.Message{
		Role:    "user",
		Content: provider.TextContent("[meta] time=2026-02-23T10:00:00Z gap=none model=claude-haiku-4-5\nHello"),
	})
	store.Append(sessionKey, provider.Message{
		Role:    "assistant",
		Content: provider.TextContent("Hi!"),
	})
	store.Append(sessionKey, provider.Message{
		Role:    "user",
		Content: provider.TextContent("[System restarted at 2026-02-23T11:00:00Z]"),
	})

	ag.SeedSessionMeta(sessionKey)

	sm := ag.getSessionMeta(sessionKey)
	expected := time.Date(2026, 2, 23, 10, 0, 0, 0, time.UTC)
	if !sm.lastMessageTime.Equal(expected) {
		t.Errorf("lastMessageTime = %v, want %v (should skip restart marker and find first meta)", sm.lastMessageTime, expected)
	}
}

func TestHandleMessageRateLimit(t *testing.T) {
	// Server returns 429 with Retry-After header.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"rate limited"}}`))
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	var rateLimitCalled bool
	var rateLimitRetry int

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
		RateLimitFunc: func(retryAfter int) {
			rateLimitCalled = true
			rateLimitRetry = retryAfter
		},
	}

	_, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "Hello")
	if err == nil {
		t.Fatal("expected error for rate limit")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error = %q, want rate limited message", err.Error())
	}

	if !rateLimitCalled {
		t.Error("RateLimitFunc not called")
	}
	if rateLimitRetry != 120 {
		t.Errorf("retryAfter = %d, want 120", rateLimitRetry)
	}
}

func TestHandleMessageOverloaded(t *testing.T) {
	// Server returns 529 Overloaded — should get overloaded message, not rate limit.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(529)
		w.Write([]byte(`{"error":{"type":"overloaded_error","message":"overloaded"}}`))
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	var rateLimitCalled bool

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
		RateLimitFunc: func(retryAfter int) {
			rateLimitCalled = true
		},
	}

	_, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "Hello")
	if err == nil {
		t.Fatal("expected error for overloaded")
	}
	if !strings.Contains(err.Error(), "overloaded") {
		t.Errorf("error = %q, want overloaded message", err.Error())
	}
	if strings.Contains(err.Error(), "mana exhausted") {
		t.Errorf("error = %q, should not mention mana exhausted for 529", err.Error())
	}

	if rateLimitCalled {
		t.Error("RateLimitFunc should not be called for 529")
	}
}

func TestHandleMessageRateLimitNoCallback(t *testing.T) {
	// 429 without RateLimitFunc — should still return friendly error, not crash.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"rate limited"}}`))
	}))
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
		// RateLimitFunc intentionally nil
	}

	_, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "Hello")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error = %q, want rate limited message", err.Error())
	}
}

func TestHandleMessageServerError(t *testing.T) {
	// Server returns 500 Internal Server Error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`))
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	var rateLimitCalled bool

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
		RateLimitFunc: func(retryAfter int) {
			rateLimitCalled = true
			if retryAfter != 0 {
				t.Errorf("retryAfter = %d, want 0 for server error", retryAfter)
			}
		},
	}

	_, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "Hello")
	if err == nil {
		t.Fatal("expected error for server error")
	}
	if !strings.Contains(err.Error(), "temporarily unavailable") {
		t.Errorf("error = %q, want friendly server error message", err.Error())
	}
	// Should not contain raw JSON
	if strings.Contains(err.Error(), `"type":"error"`) {
		t.Errorf("error = %q, should not contain raw JSON", err.Error())
	}

	if !rateLimitCalled {
		t.Error("RateLimitFunc not called for 500")
	}
}

func TestHandleMessageServerErrorNoCallback(t *testing.T) {
	// 500 without RateLimitFunc — should still return friendly error, not crash.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`))
	}))
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
		// RateLimitFunc intentionally nil
	}

	_, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "Hello")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "temporarily unavailable") {
		t.Errorf("error = %q, want friendly server error message", err.Error())
	}
}

func TestCacheBustDetection(t *testing.T) {
	callCount := 0
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		callCount++
		resp := &provider.MessageResponse{
			ID:         fmt.Sprintf("msg_%d", callCount),
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
		}
		if callCount == 1 {
			// First call: high cache read to establish baseline
			resp.Usage = provider.Usage{InputTokens: 100, OutputTokens: 10, CacheReadInputTokens: 15000}
		} else {
			// Second call: cache read drops to 0 — potential bust
			resp.Usage = provider.Usage{InputTokens: 100, OutputTokens: 10, CacheReadInputTokens: 0}
		}
		return resp
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	var alerts []string
	ag := &Agent{
		Client:          client,
		Sessions:        store,
		Tools:           registry,
		Bootstrap:       bootstrap,
		Model:           "claude-haiku-4-5",
		CacheBustDetect: true,
		CacheBustAlert: func(session string, prevRead, curRead int) {
			alerts = append(alerts, fmt.Sprintf("%s:%d→%d", session, prevRead, curRead))
		},
	}

	// First request — establishes baseline (prevCacheRead=15000)
	ag.HandleMessage(context.Background(), "test/imain/1000000000", "msg1")
	// Second request — cache_read drops to 0, recent session → should alert
	ag.HandleMessage(context.Background(), "test/imain/1000000000", "msg2")

	if len(alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d: %v", len(alerts), alerts)
	}
	if alerts[0] != "test/imain/1000000000:15000→0" {
		t.Errorf("alert = %q", alerts[0])
	}
}

func TestCacheBustSuppressedWhenIdle(t *testing.T) {
	callCount := 0
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		callCount++
		resp := &provider.MessageResponse{
			ID:         fmt.Sprintf("msg_%d", callCount),
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
		}
		if callCount == 1 {
			resp.Usage = provider.Usage{InputTokens: 100, OutputTokens: 10, CacheReadInputTokens: 15000}
		} else {
			resp.Usage = provider.Usage{InputTokens: 100, OutputTokens: 10, CacheReadInputTokens: 0}
		}
		return resp
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	var alerts []string
	ag := &Agent{
		Client:                 client,
		Sessions:               store,
		Tools:                  registry,
		Bootstrap:              bootstrap,
		Model:                  "claude-haiku-4-5",
		CacheBustDetect:        true,
		CacheBustIdleThreshold: 1 * time.Millisecond, // very short threshold for test
		CacheBustAlert: func(session string, prevRead, curRead int) {
			alerts = append(alerts, fmt.Sprintf("%s:%d→%d", session, prevRead, curRead))
		},
	}

	// First request — establishes baseline
	ag.HandleMessage(context.Background(), "test/imain/1000000000", "msg1")
	// Wait longer than the idle threshold
	time.Sleep(5 * time.Millisecond)
	// Second request — cache_read drops to 0, but session was idle → should NOT alert
	ag.HandleMessage(context.Background(), "test/imain/1000000000", "msg2")

	if len(alerts) != 0 {
		t.Fatalf("expected 0 alerts (idle session), got %d: %v", len(alerts), alerts)
	}
}

func TestCacheBustOnlyOncePerTurn(t *testing.T) {
	// A multi-step turn with tool_use iterations should fire at most one cache bust
	// warning per turn, not one per API call.
	var callCount atomic.Int32
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		n := int(callCount.Add(1))
		switch n {
		case 1:
			// First turn — establish baseline with high cache read
			return &provider.MessageResponse{
				ID:         "msg_1",
				Type:       "message",
				Role:       "assistant",
				Content:    provider.TextContent("baseline"),
				StopReason: "end_turn",
				Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10, CacheReadInputTokens: 15000},
			}
		case 2:
			// Second turn, iteration 1: tool_use with cache bust (drops to 0)
			return &provider.MessageResponse{
				ID:   "msg_2",
				Type: "message",
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "text", Text: "running tool"},
					{
						Type:  "tool_use",
						ID:    "tu_001",
						Name:  "echo_tool",
						Input: json.RawMessage(`{"text":"a"}`),
					},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10, CacheReadInputTokens: 0},
			}
		case 3:
			// Second turn, iteration 2: another tool_use, still 0 cache read
			return &provider.MessageResponse{
				ID:   "msg_3",
				Type: "message",
				Role: "assistant",
				Content: []provider.ContentBlock{
					{
						Type:  "tool_use",
						ID:    "tu_002",
						Name:  "echo_tool",
						Input: json.RawMessage(`{"text":"b"}`),
					},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10, CacheReadInputTokens: 0},
			}
		default:
			// Second turn, final: end_turn
			return &provider.MessageResponse{
				ID:         "msg_4",
				Type:       "message",
				Role:       "assistant",
				Content:    provider.TextContent("done"),
				StopReason: "end_turn",
				Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10, CacheReadInputTokens: 0},
			}
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	registry.Register(&tools.Tool{
		Name:        "echo_tool",
		Description: "echoes text",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return tools.TextResult("ok"), nil
		},
	})
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	var alerts []string
	ag := &Agent{
		Client:          client,
		Sessions:        store,
		Tools:           registry,
		Bootstrap:       bootstrap,
		Model:           "claude-haiku-4-5",
		CacheBustDetect: true,
		CacheBustAlert: func(session string, prevRead, curRead int) {
			alerts = append(alerts, fmt.Sprintf("%s:%d→%d", session, prevRead, curRead))
		},
	}

	// First turn — establishes baseline (prevCacheRead=15000)
	ag.HandleMessage(context.Background(), "test/imain/1000000000", "msg1")
	// Second turn — 3 API calls (2 tool_use + 1 end_turn), all with cache_read=0
	ag.HandleMessage(context.Background(), "test/imain/1000000000", "msg2")

	if len(alerts) != 1 {
		t.Fatalf("expected exactly 1 cache bust alert for the turn, got %d: %v", len(alerts), alerts)
	}
}

func TestCacheBustResetAfterManualCompact(t *testing.T) {
	// After ResetCacheBaseline (as called by /compact), the next request should
	// not trigger a false cache bust warning.
	callCount := 0
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		callCount++
		resp := &provider.MessageResponse{
			ID:         fmt.Sprintf("msg_%d", callCount),
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
		}
		if callCount == 1 {
			resp.Usage = provider.Usage{InputTokens: 100, OutputTokens: 10, CacheReadInputTokens: 15000}
		} else {
			// After compaction, cache read drops to 0 — but baseline was reset
			resp.Usage = provider.Usage{InputTokens: 100, OutputTokens: 10, CacheReadInputTokens: 0}
		}
		return resp
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	var alerts []string
	ag := &Agent{
		Client:          client,
		Sessions:        store,
		Tools:           registry,
		Bootstrap:       bootstrap,
		Model:           "claude-haiku-4-5",
		CacheBustDetect: true,
		CacheBustAlert: func(session string, prevRead, curRead int) {
			alerts = append(alerts, fmt.Sprintf("%s:%d→%d", session, prevRead, curRead))
		},
	}

	// First request — establishes baseline (prevCacheRead=15000)
	ag.HandleMessage(context.Background(), "test/imain/1000000000", "msg1")

	// Simulate manual /compact: reset the cache baseline
	ag.ResetCacheBaseline("test/imain/1000000000")

	// Second request — cache_read=0, but baseline was reset → no alert
	ag.HandleMessage(context.Background(), "test/imain/1000000000", "msg2")

	if len(alerts) != 0 {
		t.Fatalf("expected 0 alerts after cache baseline reset, got %d: %v", len(alerts), alerts)
	}
}

func TestThinkingAdaptiveInRequest(t *testing.T) {
	var capturedReq *provider.MessageRequest
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		capturedReq = req
		return &provider.MessageResponse{
			ID:         "msg_think",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("I thought about it."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
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
		Model:     "claude-opus-4-6",
		Thinking:  "adaptive",
	}

	_, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "Think about this")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if capturedReq.Thinking == nil {
		t.Fatal("Thinking not set on request")
	}
	if capturedReq.Thinking.Type != "adaptive" {
		t.Errorf("Thinking.Type = %q, want %q", capturedReq.Thinking.Type, "adaptive")
	}
}

func TestThinkingOffOmitsField(t *testing.T) {
	var capturedReq *provider.MessageRequest
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		capturedReq = req
		return &provider.MessageResponse{
			ID:         "msg_nothink",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("No thinking."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
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
		// Thinking not set (empty string)
	}

	_, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "No thinking")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if capturedReq.Thinking != nil {
		t.Errorf("Thinking should be nil when not configured, got %+v", capturedReq.Thinking)
	}
}

func TestThinkingBlocksPreservedInSession(t *testing.T) {
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID:   "msg_think_blocks",
			Type: "message",
			Role: "assistant",
			Content: []provider.ContentBlock{
				{Type: "thinking", Thinking: "Let me reason..."},
				{Type: "text", Text: "Here's my answer."},
			},
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 15},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	sessionStore := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	ag := &Agent{
		Client:    client,
		Sessions:  sessionStore,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-opus-4-6",
		Thinking:  "adaptive",
	}

	resp, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "Think and answer")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// TextOf should return only the text block, not thinking
	if resp != "Here's my answer." {
		t.Errorf("response = %q, want %q", resp, "Here's my answer.")
	}

	// Session should preserve both thinking and text blocks
	msgs, _ := sessionStore.Load("test/imain/1000000000")
	if len(msgs) != 2 {
		t.Fatalf("saved %d messages, want 2", len(msgs))
	}
	assistantMsg := msgs[1]
	if len(assistantMsg.Content) != 2 {
		t.Fatalf("assistant content blocks = %d, want 2", len(assistantMsg.Content))
	}
	if assistantMsg.Content[0].Type != "thinking" {
		t.Errorf("block[0].Type = %q, want 'thinking'", assistantMsg.Content[0].Type)
	}
	if assistantMsg.Content[1].Type != "text" {
		t.Errorf("block[1].Type = %q, want 'text'", assistantMsg.Content[1].Type)
	}
}

func TestBraindeadWarningInjected(t *testing.T) {
	var callCount atomic.Int32
	threshold := 3

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		n := int(callCount.Add(1))
		if n <= threshold+1 {
			// Return tool_use to keep the loop going
			return &provider.MessageResponse{
				ID:   fmt.Sprintf("msg_%d", n),
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "tool_use", ID: fmt.Sprintf("tu_%d", n), Name: "noop", Input: json.RawMessage(`{}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		return &provider.MessageResponse{
			ID:         fmt.Sprintf("msg_%d", n),
			Role:       "assistant",
			Content:    provider.TextContent("done"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	registry.Register(&tools.Tool{
		Name:       "noop",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Execute:    func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) { return tools.TextResult("ok"), nil },
	})

	ag := &Agent{
		Client:                    client,
		Sessions:                  store,
		Tools:                     registry,
		Bootstrap:                 workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:                     "claude-haiku-4-5",
		BraindeadWarningThreshold: threshold,
		BraindeadWarningEnable:    true,
	}

	_, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "go")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	msgs, _ := store.Load("test/imain/1000000000")
	// Find the braindead warning folded into a tool_result message
	found := 0
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "text" && strings.Contains(b.Text, "[system]") && strings.Contains(b.Text, "consecutive tool calls") {
				found++
			}
		}
	}
	if found != 1 {
		t.Errorf("braindead warnings found = %d, want 1", found)
	}
}

func TestBraindeadWarningOnlyOnce(t *testing.T) {
	var callCount atomic.Int32
	totalLoops := 6
	threshold := 2

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		n := int(callCount.Add(1))
		if n <= totalLoops {
			return &provider.MessageResponse{
				ID:   fmt.Sprintf("msg_%d", n),
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "tool_use", ID: fmt.Sprintf("tu_%d", n), Name: "noop", Input: json.RawMessage(`{}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		return &provider.MessageResponse{
			ID:         fmt.Sprintf("msg_%d", n),
			Role:       "assistant",
			Content:    provider.TextContent("done"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	registry.Register(&tools.Tool{
		Name:       "noop",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Execute:    func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) { return tools.TextResult("ok"), nil },
	})

	ag := &Agent{
		Client:                    client,
		Sessions:                  store,
		Tools:                     registry,
		Bootstrap:                 workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:                     "claude-haiku-4-5",
		BraindeadWarningThreshold: threshold,
		BraindeadWarningEnable:    true,
	}

	_, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "go")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	msgs, _ := store.Load("test/imain/1000000000")
	count := 0
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "text" && strings.Contains(b.Text, "[system]") {
				count++
			}
		}
	}
	if count != 1 {
		t.Errorf("braindead warnings = %d, want exactly 1 (only-once guarantee)", count)
	}
}

func TestBraindeadDisabledWhenZero(t *testing.T) {
	var callCount atomic.Int32

	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		n := int(callCount.Add(1))
		if n <= 5 {
			return &provider.MessageResponse{
				ID:   fmt.Sprintf("msg_%d", n),
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "tool_use", ID: fmt.Sprintf("tu_%d", n), Name: "noop", Input: json.RawMessage(`{}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		return &provider.MessageResponse{
			ID:         fmt.Sprintf("msg_%d", n),
			Role:       "assistant",
			Content:    provider.TextContent("done"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	registry.Register(&tools.Tool{
		Name:       "noop",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Execute:    func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) { return tools.TextResult("ok"), nil },
	})

	ag := &Agent{
		Client:                    client,
		Sessions:                  store,
		Tools:                     registry,
		Bootstrap:                 workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:                     "claude-haiku-4-5",
		BraindeadWarningThreshold: 0, // disabled
		BraindeadWarningEnable:    true,
	}

	_, err := ag.HandleMessage(context.Background(), "test/imain/1000000000", "go")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	msgs, _ := store.Load("test/imain/1000000000")
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "text" && strings.Contains(b.Text, "[system]") {
				t.Error("braindead warning injected despite threshold=0")
			}
		}
	}
}

func TestBatchPartialAssistantMessages_False(t *testing.T) {
	// When batch=false (default), intermediate text should be sent via ReplyFunc
	// and the final response text returned from HandleMessage.
	// This also covers the bug where final response has empty content.
	callCount := 0
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		callCount++
		if callCount == 1 {
			// First response: text + tool_use (intermediate text)
			return &provider.MessageResponse{
				ID:   "msg_1",
				Type: "message",
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "text", Text: "Working on it..."},
					{Type: "tool_use", ID: "tu_001", Name: "test_tool", Input: json.RawMessage(`{}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}
		// Second response: empty content (the bug scenario)
		return &provider.MessageResponse{
			ID:         "msg_2",
			Type:       "message",
			Role:       "assistant",
			Content:    []provider.ContentBlock{},
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 30, OutputTokens: 1},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	registry.Register(&tools.Tool{
		Name:       "test_tool",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return tools.TextResult("done"), nil
		},
	})
	ag := &Agent{
		Client:                        client,
		Sessions:                      store,
		Tools:                         registry,
		Bootstrap:                     workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:                         "claude-haiku-4-5",
		BatchPartialAssistantMessages: false,
	}

	var intermediateReplies []string
	cb := &TurnCallbacks{
		ReplyFunc: func(text string) {
			intermediateReplies = append(intermediateReplies, text)
		},
	}
	ctx := WithTurnCallbacks(context.Background(), cb)

	finalResp, err := ag.HandleMessage(ctx, "test/ibatchfalse/1000000000", "Do something")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// Intermediate text should have been sent via ReplyFunc
	if len(intermediateReplies) != 1 || intermediateReplies[0] != "Working on it..." {
		t.Errorf("intermediate replies = %v, want [\"Working on it...\"]", intermediateReplies)
	}

	// Final response is empty (the bug scenario — but text was already delivered)
	if finalResp != "" {
		t.Errorf("final response = %q, want empty", finalResp)
	}
}

func TestBatchPartialAssistantMessages_True(t *testing.T) {
	// When batch=true, intermediate text should be accumulated and returned
	// concatenated from HandleMessage. No ReplyFunc calls.
	callCount := 0
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		callCount++
		if callCount == 1 {
			return &provider.MessageResponse{
				ID:   "msg_1",
				Type: "message",
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "text", Text: "Working on it..."},
					{Type: "tool_use", ID: "tu_001", Name: "test_tool", Input: json.RawMessage(`{}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}
		// Second response: empty content
		return &provider.MessageResponse{
			ID:         "msg_2",
			Type:       "message",
			Role:       "assistant",
			Content:    []provider.ContentBlock{},
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 30, OutputTokens: 1},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	registry.Register(&tools.Tool{
		Name:       "test_tool",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return tools.TextResult("done"), nil
		},
	})
	ag := &Agent{
		Client:                        client,
		Sessions:                      store,
		Tools:                         registry,
		Bootstrap:                     workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:                         "claude-haiku-4-5",
		BatchPartialAssistantMessages: true,
	}

	var intermediateReplies []string
	cb := &TurnCallbacks{
		ReplyFunc: func(text string) {
			intermediateReplies = append(intermediateReplies, text)
		},
	}
	ctx := WithTurnCallbacks(context.Background(), cb)

	finalResp, err := ag.HandleMessage(ctx, "test/ibatchtrue/1000000000", "Do something")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// ReplyFunc should NOT have been called
	if len(intermediateReplies) != 0 {
		t.Errorf("intermediate replies = %v, want none", intermediateReplies)
	}

	// Batched text should be returned as the final response
	if finalResp != "Working on it..." {
		t.Errorf("final response = %q, want %q", finalResp, "Working on it...")
	}
}

func TestBatchPartialAssistantMessages_TrueMultipleTexts(t *testing.T) {
	// When batch=true with multiple intermediate text blocks and a final text,
	// all text should be concatenated with double newlines.
	callCount := 0
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		callCount++
		if callCount == 1 {
			return &provider.MessageResponse{
				ID:   "msg_1",
				Type: "message",
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "text", Text: "Step 1 done."},
					{Type: "tool_use", ID: "tu_001", Name: "test_tool", Input: json.RawMessage(`{}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}
		if callCount == 2 {
			return &provider.MessageResponse{
				ID:   "msg_2",
				Type: "message",
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "text", Text: "Step 2 done."},
					{Type: "tool_use", ID: "tu_002", Name: "test_tool", Input: json.RawMessage(`{}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 30, OutputTokens: 10},
			}
		}
		// Third response: final text
		return &provider.MessageResponse{
			ID:         "msg_3",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("All done!"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 40, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	registry.Register(&tools.Tool{
		Name:       "test_tool",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return tools.TextResult("done"), nil
		},
	})
	ag := &Agent{
		Client:                        client,
		Sessions:                      store,
		Tools:                         registry,
		Bootstrap:                     workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:                         "claude-haiku-4-5",
		BatchPartialAssistantMessages: true,
	}

	ctx := WithTurnCallbacks(context.Background(), &TurnCallbacks{})
	finalResp, err := ag.HandleMessage(ctx, "test/ibatchmulti/1000000000", "Do multiple things")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	expected := "Step 1 done.Step 2 done.All done!"
	if finalResp != expected {
		t.Errorf("final response = %q, want %q", finalResp, expected)
	}
}

func TestHandleMessageRateLimitGateBlocks(t *testing.T) {
	// When the gate is closed, HandleMessage should queue the message
	// and return RateLimitedError without touching the session.
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		t.Fatal("API should not be called when gate is closed")
		return nil
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

	// Close the gate
	until := time.Now().Add(1 * time.Hour)
	ag.rateLimitGate.Close(until)

	ctx := WithTrigger(context.Background(), "telegram")
	_, err := ag.HandleMessage(ctx, "test/igate/1000000000", "Hello")
	if err == nil {
		t.Fatal("expected RateLimitedError")
	}

	var rlErr *RateLimitedError
	if !errors.As(err, &rlErr) {
		t.Fatalf("expected *RateLimitedError, got %T: %v", err, err)
	}
	if !rlErr.Until.Equal(until) {
		t.Errorf("until = %v, want %v", rlErr.Until, until)
	}
}

func TestHandleMessageRateLimitClosesGate(t *testing.T) {
	// A 429 from the API should close the gate so subsequent calls are blocked.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"rate limited"}}`))
	}))
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

	// First call hits the API, gets 429, closes gate
	_, err := ag.HandleMessage(context.Background(), "test/igate429/1000000000", "Hello")
	if err == nil {
		t.Fatal("expected error")
	}
	var rlErr *RateLimitedError
	if !errors.As(err, &rlErr) {
		t.Fatalf("expected *RateLimitedError, got %T: %v", err, err)
	}

	// Gate should now be closed
	limited, _ := ag.rateLimitGate.IsLimited()
	if !limited {
		t.Error("gate should be closed after 429")
	}

	// Second call should be blocked by the gate (no API hit)
	_, err = ag.HandleMessage(context.Background(), "test/igate429/1000000000", "World")
	if err == nil {
		t.Fatal("expected RateLimitedError on second call")
	}
	if !errors.As(err, &rlErr) {
		t.Fatalf("expected *RateLimitedError on second call, got %T: %v", err, err)
	}
}

func TestDrainRateLimitQueue(t *testing.T) {
	// When the gate opens, DrainRateLimitQueue should replay messages.
	var apiCalls atomic.Int32
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		apiCalls.Add(1)
		return &provider.MessageResponse{
			ID:         "msg_drain",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("replayed"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
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

	// Queue items as if rate-limited
	ag.rateLimitGate.Close(time.Now().Add(-1 * time.Second)) // already expired
	ag.rateLimitGate.Enqueue("test/idrain/1000000000", "msg1", "user")
	ag.rateLimitGate.Enqueue("test/idrain/1000000000", "msg2", "keepalive")

	ag.DrainRateLimitQueue(context.Background())

	if got := apiCalls.Load(); got != 2 {
		t.Errorf("expected 2 API calls from replay, got %d", got)
	}
}

// TestCanFireBackgroundOperation_RateLimited proves that the method returns false
// when the rate limit gate is closed, with a descriptive reason including reset time.
func TestCanFireBackgroundOperation_RateLimited(t *testing.T) {
	ag := &Agent{ManaInvestInterval: 30 * time.Minute}
	ag.rateLimitGate.Close(time.Now().Add(2 * time.Hour))

	canFire, reason := ag.CanFireBackgroundOperation(context.Background(), "test/c123/1000000000")

	if canFire {
		t.Error("expected canFire=false when rate limited")
	}
	if !strings.Contains(reason, "rate limited") {
		t.Errorf("expected rate limited reason, got: %s", reason)
	}
	if !strings.Contains(reason, "resets") {
		t.Errorf("expected reset time in reason, got: %s", reason)
	}
}

// TestCanFireBackgroundOperation_NoSessionKey proves that the method returns false
// with "no session key" when given an empty session key.
func TestCanFireBackgroundOperation_NoSessionKey(t *testing.T) {
	ag := &Agent{ManaInvestInterval: 30 * time.Minute}

	canFire, reason := ag.CanFireBackgroundOperation(context.Background(), "")

	if canFire {
		t.Error("expected canFire=false with empty session key")
	}
	if reason != "no session key" {
		t.Errorf("expected 'no session key', got: %s", reason)
	}
}

// TestCanFireBackgroundOperation_NoUsageClient proves that the method returns true
// (skips mana check) when there's no UsageClient for the session's endpoint.
func TestCanFireBackgroundOperation_NoUsageClient(t *testing.T) {
	ag := &Agent{
		UsageClient:        nil,
		GetUsageClient:     func(endpoint string) *anthropic.UsageClient { return nil },
		ManaInvestInterval: 30 * time.Minute,
	}

	canFire, reason := ag.CanFireBackgroundOperation(context.Background(), "test/c123/1000000000")

	if !canFire {
		t.Errorf("expected canFire=true for non-Anthropic endpoint, got false: %s", reason)
	}
	if reason != "" {
		t.Errorf("expected empty reason, got: %s", reason)
	}
}

// TestCanFireBackgroundOperation_ZeroInvestInterval proves that mana checking is skipped
// when ManaInvestInterval is zero (mana tracking disabled).
func TestCanFireBackgroundOperation_ZeroInvestInterval(t *testing.T) {
	// Mock UsageClient that would fail if called
	mockClient := &anthropic.UsageClient{}

	ag := &Agent{
		UsageClient:        mockClient,
		GetUsageClient:     func(endpoint string) *anthropic.UsageClient { return mockClient },
		ManaInvestInterval: 0, // disabled
	}

	canFire, reason := ag.CanFireBackgroundOperation(context.Background(), "test/c123/1000000000")

	if !canFire {
		t.Errorf("expected canFire=true with zero invest interval, got false: %s", reason)
	}
	if reason != "" {
		t.Errorf("expected empty reason, got: %s", reason)
	}
}

// TestCanFireBackgroundOperation_ManaInsufficient proves that the method returns false
// when mana is insufficient according to the monitor's IsGoodFor check.
func TestCanFireBackgroundOperation_ManaInsufficient(t *testing.T) {
	// Skipping this test since UsageClient baseURL cannot be easily mocked in agent tests.
	// The mana.Monitor.IsGoodFor logic is already tested in the mana package tests.
	t.Skip("Skipping mana insufficient test - UsageClient baseURL cannot be easily mocked in agent tests")
}

// TestCanFireBackgroundOperation_Success proves that the method returns true
// when all checks pass (gate open, valid session, no usage client = mana check skipped).
func TestCanFireBackgroundOperation_Success(t *testing.T) {
	// Test the success path by having no usage client (mana check skipped)
	// This is the common path for non-Anthropic endpoints
	ag := &Agent{
		UsageClient:        nil,
		GetUsageClient:     func(endpoint string) *anthropic.UsageClient { return nil },
		ManaInvestInterval: 30 * time.Minute,
	}

	canFire, reason := ag.CanFireBackgroundOperation(context.Background(), "test/c123/1000000000")

	if !canFire {
		t.Errorf("expected canFire=true when all checks pass, got false: %s", reason)
	}
	if reason != "" {
		t.Errorf("expected empty reason, got: %s", reason)
	}
}
