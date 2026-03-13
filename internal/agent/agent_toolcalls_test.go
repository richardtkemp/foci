package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestRepairInterruptedToolCalls(t *testing.T) {
	// Proves that repairInterruptedToolCalls returns nil for benign cases (empty, last message is user, no tool_use) and produces a synthetic error tool_result for every unmatched tool_use block when an assistant message ends mid-call.
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

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	// Pre-populate session with an interrupted tool call
	sessionKey := "test/irepair/1000000000"
	store.TestAppend(sessionKey, provider.Message{
		Role: "user", Content: provider.TextContent("do something"),
	})
	store.TestAppend(sessionKey, provider.Message{
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

func TestIntermediateTextBeforeToolCalls(t *testing.T) {
	// Verify the agent calls sendIntermediate before notifyToolCall when the
	// API response contains both text and tool_use blocks. This ordering is
	// critical for platform message display: thinking text must appear above
	// tool call notifications in the chat.
	var callCount atomic.Int32

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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

func TestToolResultRedaction(t *testing.T) {
	// Proves that the Redact function is applied to tool output before it is saved to the session store, preventing secrets from being persisted in plaintext.
	var callCount atomic.Int32

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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
	// Proves that the Redact function is applied to tool error messages as well as successful results, so that secrets in error text are not stored in the session.
	var callCount atomic.Int32

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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

