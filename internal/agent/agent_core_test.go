package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

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


