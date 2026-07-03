package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"foci/internal/agent/turnevent"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestSteerCheckFromCtx_NilCallbacks(t *testing.T) {
	// Proves that steerBlocks returns nil and does not panic when there is
	// no Steerer attached to the context.
	if got := steerBlocks(context.Background()); got != nil {
		t.Errorf("expected nil from bare context, got %v", got)
	}
}

func TestSteerCheckFromCtx_NilFunc(t *testing.T) {
	// Proves that steerBlocks returns nil when a Steerer returns an empty
	// slice — no [user] blocks should be generated.
	ctx := turnevent.WithSteerer(context.Background(), turnevent.SteererFunc(func() []string { return nil }))
	if got := steerBlocks(ctx); got != nil {
		t.Errorf("expected nil with empty Steerer, got %v", got)
	}
}

func TestSteerCheckFromCtx_ReturnsText(t *testing.T) {
	// Proves that steerBlocks pulls from the ctx-attached Steerer and wraps
	// each pending steer in a `[user] ...` content block.
	ctx := turnevent.WithSteerer(context.Background(), turnevent.SteererFunc(func() []string {
		return []string{"change direction"}
	}))
	got := steerBlocks(ctx)
	if len(got) != 1 || got[0].Text != "[user] change direction" {
		t.Errorf("got %v, want [user] change direction", got)
	}
}

func TestExecuteToolCalls_SteerSkipsRemainingTools(t *testing.T) {
	// Proves that when SteerCheckFunc returns a non-empty string after the first tool executes, all subsequent tools in the batch are skipped with error results and the steer text is injected as a [user] text block.
	registry := tools.NewRegistry()
	var toolCalls int
	registry.Register(&tools.Tool{
		Name:        "slow_tool",
		Description: "a tool that increments a counter",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			toolCalls++
			return tools.TextResult("ok"), nil
		},
	})

	ag := &Agent{Tools: registry}

	// Steer fires on the second check (after first tool executes)
	var checkCount int
	ctx := turnevent.WithSteerer(context.Background(), turnevent.SteererFunc(func() []string {
		checkCount++
		if checkCount >= 2 {
			return []string{"stop and do something else"}
		}
		return nil
	}))

	blocks := []provider.ContentBlock{
		{Type: "tool_use", ID: "tu_1", Name: "slow_tool", Input: json.RawMessage(`{}`)},
		{Type: "tool_use", ID: "tu_2", Name: "slow_tool", Input: json.RawMessage(`{}`)},
		{Type: "tool_use", ID: "tu_3", Name: "slow_tool", Input: json.RawMessage(`{}`)},
	}

	td := &TurnDetail{SessionKey: "test/steer"}
	results, err := ag.executeToolCalls(ctx, td, nil, "test/steer", "", blocks, nil)
	if err != nil {
		t.Fatalf("executeToolCalls: %v", err)
	}

	// First tool should have executed
	if toolCalls != 1 {
		t.Errorf("tool executions = %d, want 1", toolCalls)
	}

	// Results: tool_result for tu_1 (success) + steer text block.
	// No synthetic "Skipped" results — the caller strips unexecuted tool_use
	// blocks from the assistant message instead.
	if len(results) != 2 {
		t.Fatalf("got %d result blocks, want 2", len(results))
	}

	// First result: successful execution of tu_1
	if results[0].ToolUseID != "tu_1" || results[0].IsError {
		t.Errorf("result[0]: expected successful tu_1, got toolUseID=%q isError=%v", results[0].ToolUseID, results[0].IsError)
	}

	// Second result: steer text block
	if results[1].Type != "text" || results[1].Text != "[user] stop and do something else" {
		t.Errorf("result[1]: expected steer text block, got type=%q text=%q", results[1].Type, results[1].Text)
	}
}

func TestExecuteToolCalls_NoSteer(t *testing.T) {
	// Proves that when SteerCheckFunc always returns empty, executeToolCalls runs all tools to completion and returns only tool_result blocks with no injected text.
	registry := tools.NewRegistry()
	var toolCalls int
	registry.Register(&tools.Tool{
		Name:        "counter",
		Description: "increments counter",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			toolCalls++
			return tools.TextResult(fmt.Sprintf("call %d", toolCalls)), nil
		},
	})

	ag := &Agent{Tools: registry}

	ctx := turnevent.WithSteerer(context.Background(), turnevent.SteererFunc(func() []string { return nil }))

	blocks := []provider.ContentBlock{
		{Type: "tool_use", ID: "tu_1", Name: "counter", Input: json.RawMessage(`{}`)},
		{Type: "tool_use", ID: "tu_2", Name: "counter", Input: json.RawMessage(`{}`)},
	}

	td := &TurnDetail{SessionKey: "test/nosteer"}
	results, err := ag.executeToolCalls(ctx, td, nil, "test/nosteer", "", blocks, nil)
	if err != nil {
		t.Fatalf("executeToolCalls: %v", err)
	}

	if toolCalls != 2 {
		t.Errorf("tool executions = %d, want 2", toolCalls)
	}

	// Should have 2 tool results, no steer text blocks
	if len(results) != 2 {
		t.Fatalf("got %d result blocks, want 2", len(results))
	}
	for _, r := range results {
		if r.Type == "text" {
			t.Errorf("unexpected text block: %q", r.Text)
		}
	}
}

func TestExecuteToolCalls_SteerBeforeFirstTool(t *testing.T) {
	// Proves that when SteerCheckFunc returns a steer message before the first tool runs, zero tools execute and every tool in the batch gets a synthetic skip result.
	registry := tools.NewRegistry()
	var toolCalls int
	registry.Register(&tools.Tool{
		Name:        "noop",
		Description: "should not run",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			toolCalls++
			return tools.TextResult("ok"), nil
		},
	})

	ag := &Agent{Tools: registry}

	// Steer fires immediately
	ctx := turnevent.WithSteerer(context.Background(), turnevent.SteererFunc(func() []string { return []string{"abort"} }))

	blocks := []provider.ContentBlock{
		{Type: "tool_use", ID: "tu_1", Name: "noop", Input: json.RawMessage(`{}`)},
		{Type: "tool_use", ID: "tu_2", Name: "noop", Input: json.RawMessage(`{}`)},
	}

	td := &TurnDetail{SessionKey: "test/early-steer"}
	results, err := ag.executeToolCalls(ctx, td, nil, "test/early-steer", "", blocks, nil)
	if err != nil {
		t.Fatalf("executeToolCalls: %v", err)
	}

	if toolCalls != 0 {
		t.Errorf("tool executions = %d, want 0", toolCalls)
	}

	// Just the steer text block — no synthetic "Skipped" results.
	if len(results) != 1 {
		t.Fatalf("got %d result blocks, want 1", len(results))
	}

	if results[0].Type != "text" || results[0].Text != "[user] abort" {
		t.Errorf("steer block: type=%q text=%q", results[0].Type, results[0].Text)
	}
}

func TestSteerInjectedAfterToolBatch(t *testing.T) {
	// Proves that a steer message polled after the tool batch completes (not between individual tools) is injected as a [user] text block in the next API request, allowing the model to honour the redirect.
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
						Name:  "echo_tool",
						Input: json.RawMessage(`{"text":"hello"}`),
					},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}

		// Second call: verify steer message was injected
		// Find [user] block in the messages
		for _, msg := range req.Messages {
			for _, block := range msg.Content {
				if block.Type == "text" && block.Text == "[user] new direction please" {
					return &provider.MessageResponse{
						ID:         "msg_2",
						Type:       "message",
						Role:       "assistant",
						Content:    provider.TextContent("Got your redirect."),
						StopReason: "end_turn",
						Usage:      provider.Usage{InputTokens: 30, OutputTokens: 15},
					}
				}
			}
		}

		return &provider.MessageResponse{
			ID:         "msg_2",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("No steer found."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 30, OutputTokens: 15},
		}
	})
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
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

	// Steer arrives after tool batch completes (not between individual tools).
	// SteerCheckFunc fires "" during executeToolCalls, then "new direction please"
	// at the post-batch check.
	var checkCount int
	ctx := turnevent.WithSteerer(context.Background(), turnevent.SteererFunc(func() []string {
		checkCount++
		// The first check is inside executeToolCalls (before the tool).
		// The second check is the post-batch check in agent.go.
		if checkCount == 2 {
			return []string{"new direction please"}
		}
		return nil
	}))

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	resp, err := ag.hmTestAttachments(ctx, "test/imain", []string{"Do something"}, nil)
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if resp != "Got your redirect." {
		t.Errorf("response = %q, want %q", resp, "Got your redirect.")
	}
}

func TestSteerMidBatch_AssistantMessageRewritten(t *testing.T) {
	// Proves that when a steer fires mid-batch, the assistant message sent
	// in the next API call has the unexecuted tool_use blocks stripped and
	// no "Skipped" tool_results appear anywhere.
	var callCount atomic.Int32

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		n := callCount.Add(1)

		if n == 1 {
			// First call: model requests 3 tool calls
			return &provider.MessageResponse{
				ID:   "msg_1",
				Type: "message",
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "text", Text: "Let me run these tools"},
					{Type: "tool_use", ID: "tu_A", Name: "echo_tool", Input: json.RawMessage(`{"text":"first"}`)},
					{Type: "tool_use", ID: "tu_B", Name: "echo_tool", Input: json.RawMessage(`{"text":"second"}`)},
					{Type: "tool_use", ID: "tu_C", Name: "echo_tool", Input: json.RawMessage(`{"text":"third"}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}

		// Second call: verify the assistant message was rewritten.
		// It should only contain the text block and tu_A (executed),
		// not tu_B or tu_C (skipped by steer).
		for _, msg := range req.Messages {
			if msg.Role == "assistant" {
				for _, block := range msg.Content {
					if block.Type == "tool_use" && (block.ID == "tu_B" || block.ID == "tu_C") {
						return &provider.MessageResponse{
							ID:         "msg_2",
							Type:       "message",
							Role:       "assistant",
							Content:    provider.TextContent("FAIL: unexecuted tool_use blocks present"),
							StopReason: "end_turn",
							Usage:      provider.Usage{InputTokens: 30, OutputTokens: 15},
						}
					}
				}
			}
			// Also verify no "Skipped" results
			for _, block := range msg.Content {
				if block.Type == "tool_result" && block.Content == "Skipped: user redirected the conversation" {
					return &provider.MessageResponse{
						ID:         "msg_2",
						Type:       "message",
						Role:       "assistant",
						Content:    provider.TextContent("FAIL: Skipped tool_result present"),
						StopReason: "end_turn",
						Usage:      provider.Usage{InputTokens: 30, OutputTokens: 15},
					}
				}
			}
		}

		return &provider.MessageResponse{
			ID:         "msg_2",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Clean rewrite confirmed."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 30, OutputTokens: 15},
		}
	})

	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
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

	// Steer fires after the first tool executes (second steer check).
	var checkCount int
	ctx := turnevent.WithSteerer(context.Background(), turnevent.SteererFunc(func() []string {
		checkCount++
		if checkCount == 2 {
			return []string{"stop everything"}
		}
		return nil
	}))

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	resp, err := ag.hmTestAttachments(ctx, "test/imain", []string{"Run three tools"}, nil)
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if resp != "Clean rewrite confirmed." {
		t.Errorf("response = %q, want %q", resp, "Clean rewrite confirmed.")
	}
}
