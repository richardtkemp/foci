package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestHandleMessageEndTurn(t *testing.T) {
	// Proves the basic happy path: a single-turn exchange returns the assistant's
	// text and saves user+assistant messages to the session store.
	// Set up mock API server
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Hello from mock!"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
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

	resp, err := ag.hmTest(context.Background(), "test/imain/1000000000", "Hi there")
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
	// Proves that tool_use responses trigger tool execution and a follow-up API call,
	// and that all four messages (user, assistant-with-tool, tool-result, final-assistant)
	// are persisted in the session.
	var callCount atomic.Int32

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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

	resp, err := ag.hmTest(context.Background(), "test/imain/1000000000", "Run echo")
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
	// Proves that when the model calls a tool that isn't registered, the agent
	// sends an error tool_result back and the loop continues to a normal end_turn.
	var callCount atomic.Int32

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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

	resp, err := ag.hmTest(context.Background(), "test/imain/1000000000", "Use a fake tool")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if resp != "Sorry, that tool doesn't exist." {
		t.Errorf("response = %q", resp)
	}
}

func TestHandleMessageSessionContinuity(t *testing.T) {
	// Proves that session history is carried forward between turns: the second
	// message is sent to the API with the prior exchange already included.
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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
	resp, _ := ag.hmTest(context.Background(), "test/imain/1000000000", "First")
	if resp != "Received 1 messages" {
		t.Errorf("first response = %q", resp)
	}

	// Second message — should include history
	resp, _ = ag.hmTest(context.Background(), "test/imain/1000000000", "Second")
	if resp != "Received 3 messages" {
		t.Errorf("second response = %q, want 'Received 3 messages' (2 history + 1 new)", resp)
	}
}

func TestHandleMessageCancellation(t *testing.T) {
	// Verify that a cancelled context causes HandleMessage to return ctx.Err()
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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
		// Wait for the turn to start
		for !ag.IsTurnInFlight(session.SessionKeyBase("test/icancel/1000000000")) {
			// spin until the turn is in flight
		}
		cancel()
	}()

	_, err := ag.hmTest(ctx, "test/icancel/1000000000", "Do something slow")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

func TestTurnInFlight_ClearedAfterHandleMessage(t *testing.T) {
	// Proves the per-session in-flight flag is false both before and after a
	// HandleMessage call, confirming it's correctly cleared on completion.
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("done"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	const sk = "test/iproc/1000000000"
	base := session.SessionKeyBase(sk)
	if ag.IsTurnInFlight(base) {
		t.Error("should not be in flight before HandleMessage")
	}

	ag.hmTest(context.Background(), sk, "Hi")

	if ag.IsTurnInFlight(base) {
		t.Error("should not be in flight after HandleMessage returns")
	}
}

func TestProcessingDetails(t *testing.T) {
	// ProcessingDetails should capture session key, trigger, and timing
	started := make(chan struct{})
	var startedOnce sync.Once

	client := newTestClientWithError(func(_ context.Context, _ *provider.MessageRequest) (*provider.MessageResponse, error) {
		startedOnce.Do(func() { close(started) })
		time.Sleep(200 * time.Millisecond) // hold the turn open
		return &provider.MessageResponse{
			ID:         "msg_1",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}, nil
	})
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
	done := make(chan struct{})
	go func() {
		ctx := WithTrigger(context.Background(), "keepalive")
		ag.hmTest(ctx, "test/idetail/1000000000", "check")
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
	// Proves that WithTrigger stores the trigger string in context and
	// TriggerFromContext retrieves it, with an empty string as the default.
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

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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

	// Track intermediate replies via a context-scoped fnSink observer.
	var intermediateReplies []string
	recorder := fnSink(func(_ context.Context, ev turnevent.Event) {
		if tb, ok := ev.(turnevent.TextBlock); ok && tb.Phase == turnevent.PhaseIntermediate {
			intermediateReplies = append(intermediateReplies, tb.Text)
		}
	})
	ctx := turnevent.WithSink(context.Background(), recorder)

	finalResp, err := ag.hmTest(ctx, "test/ideferred/1000000000", "Complex question")
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

func TestNoResponseSentinelPassedThrough(t *testing.T) {
	// The agent layer no longer strips [[NO_RESPONSE]] — that's handled
	// downstream by platform.IsSilent (bot SendText/SendTextToChat and Finalize).
	// HandleMessage returns the sentinel as-is.
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("[[NO_RESPONSE]]"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	resp, err := ag.hmTest(context.Background(), "test/imain/1000000000", "ping")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if resp != "[[NO_RESPONSE]]" {
		t.Errorf("expected sentinel to pass through, got %q", resp)
	}
}

// newTestClientWithBase creates a test client with a custom base URL.
