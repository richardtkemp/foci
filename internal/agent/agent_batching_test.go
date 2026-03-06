package agent

import (
	"context"
	"encoding/json"
	"testing"

	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

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

	client := newTestClientWithBase(server.URL)
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

	client := newTestClientWithBase(server.URL)
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

	client := newTestClientWithBase(server.URL)
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

	client := newTestClientWithBase(server.URL)
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

	client := newTestClientWithBase(server.URL)
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

	client := newTestClientWithBase(server.URL)
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

