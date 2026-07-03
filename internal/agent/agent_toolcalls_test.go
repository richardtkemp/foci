package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"foci/internal/agent/turnevent"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestRepairInterruptedToolCalls(t *testing.T) {
	// Proves that repairInterruptedToolCalls returns nil for benign cases
	// (empty, last message is user, no tool_use) and produces a
	// user(tool_result) + assistant(ack) pair for interrupted tool calls.
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
		if len(got) != 2 {
			t.Fatalf("expected 2 repair messages (tool_result + ack), got %d", len(got))
		}
		// First message: user with tool_result
		if got[0].Role != "user" {
			t.Errorf("repair[0] role = %q, want user", got[0].Role)
		}
		if len(got[0].Content) != 1 {
			t.Fatalf("expected 1 tool_result block, got %d", len(got[0].Content))
		}
		if got[0].Content[0].Type != "tool_result" {
			t.Errorf("block type = %q, want tool_result", got[0].Content[0].Type)
		}
		if got[0].Content[0].ToolUseID != "tu_123" {
			t.Errorf("tool_use_id = %q, want tu_123", got[0].Content[0].ToolUseID)
		}
		if !got[0].Content[0].IsError {
			t.Error("expected is_error = true")
		}
		// Second message: assistant ack
		if got[1].Role != "assistant" {
			t.Errorf("repair[1] role = %q, want assistant", got[1].Role)
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
		if len(got) != 2 {
			t.Fatalf("expected 2 repair messages, got %d", len(got))
		}
		if len(got[0].Content) != 2 {
			t.Fatalf("expected 2 tool_result blocks, got %d", len(got[0].Content))
		}
		if got[0].Content[0].ToolUseID != "tu_a" {
			t.Errorf("block[0].tool_use_id = %q, want tu_a", got[0].Content[0].ToolUseID)
		}
		if got[0].Content[1].ToolUseID != "tu_b" {
			t.Errorf("block[1].tool_use_id = %q, want tu_b", got[0].Content[1].ToolUseID)
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
	sessionKey := "test/irepair"
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

	_, err := ag.hmTest(context.Background(), sessionKey, "continue")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// The API request should include the repair tool_result before the new user message
	if receivedReq == nil {
		t.Fatal("no request received")
	}

	// Messages: user("do something"), assistant(tool_use), user(tool_result repair),
	// assistant(ack), user("continue"), assistant("Recovered.")
	// The repair includes an assistant ack to maintain role alternation.
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
	// Should have: user, assistant(tool_use), user(tool_result repair), assistant(ack), user(continue), assistant(Recovered.)
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
	// Verify the agent emits the intermediate TextBlock before the ToolCall
	// event when the API response contains both text and tool_use blocks.
	// This ordering is critical for platform message display: thinking text
	// must appear above tool call notifications in the chat.
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

	// Record event order so we can assert TextBlock-before-ToolCall.
	var mu sync.Mutex
	var order []string

	recorder := fnSink(func(_ context.Context, ev turnevent.Event) {
		switch e := ev.(type) {
		case turnevent.TextBlock:
			if e.Phase == turnevent.PhaseIntermediate {
				mu.Lock()
				order = append(order, "reply:"+e.Text)
				mu.Unlock()
			}
		case turnevent.ToolCall:
			mu.Lock()
			order = append(order, "tool:"+e.Name)
			mu.Unlock()
		}
	})
	ctx := turnevent.WithSink(context.Background(), recorder)

	_, err := ag.hmTest(ctx, "test/iorder", "Check something")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(order) < 2 {
		t.Fatalf("expected at least 2 events, got %d: %v", len(order), order)
	}
	if order[0] != "reply:Let me check..." {
		t.Errorf("order[0] = %q, want intermediate text first", order[0])
	}
	if order[1] != "tool:test_tool" {
		t.Errorf("order[1] = %q, want tool call second", order[1])
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

	_, err := ag.hmTest(context.Background(), "test/iredact", "Leak the secret")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// Verify the saved session has redacted tool result
	msgs, _ := store.Load("test/iredact")
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

	_, err := ag.hmTest(context.Background(), "test/iredacterr", "Try the tool")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// Verify the saved session has redacted error message
	msgs, _ := store.Load("test/iredacterr")
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
