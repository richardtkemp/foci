package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// TestSteerCheckFromCtx_NilCallbacks verifies that steerCheckFromCtx returns ""
// when the context has no TurnCallbacks, preventing panics in the default path.
func TestSteerCheckFromCtx_NilCallbacks(t *testing.T) {
	got := steerCheckFromCtx(context.Background())
	if got != "" {
		t.Errorf("expected empty string from bare context, got %q", got)
	}
}

// TestSteerCheckFromCtx_NilFunc verifies that steerCheckFromCtx returns ""
// when TurnCallbacks exists but SteerCheckFunc is nil.
func TestSteerCheckFromCtx_NilFunc(t *testing.T) {
	ctx := WithTurnCallbacks(context.Background(), &TurnCallbacks{})
	got := steerCheckFromCtx(ctx)
	if got != "" {
		t.Errorf("expected empty string with nil SteerCheckFunc, got %q", got)
	}
}

// TestSteerCheckFromCtx_ReturnsText verifies that steerCheckFromCtx returns
// the text from SteerCheckFunc when set.
func TestSteerCheckFromCtx_ReturnsText(t *testing.T) {
	ctx := WithTurnCallbacks(context.Background(), &TurnCallbacks{
		SteerCheckFunc: func() string { return "change direction" },
	})
	got := steerCheckFromCtx(ctx)
	if got != "change direction" {
		t.Errorf("got %q, want %q", got, "change direction")
	}
}

// TestExecuteToolCalls_SteerSkipsRemainingTools verifies that when a steer
// message arrives between tool calls, the remaining tools are skipped with
// synthetic error results and the steer text is appended as a [user] block.
func TestExecuteToolCalls_SteerSkipsRemainingTools(t *testing.T) {
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
	ctx := WithTurnCallbacks(context.Background(), &TurnCallbacks{
		SteerCheckFunc: func() string {
			checkCount++
			if checkCount >= 2 {
				return "stop and do something else"
			}
			return ""
		},
	})

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

	// Results should contain: tool_result for tu_1 (success), tool_result for
	// tu_2 and tu_3 (skipped), and a text block with [user] prefix.
	if len(results) != 4 {
		t.Fatalf("got %d result blocks, want 4", len(results))
	}

	// First result: successful execution of tu_1
	if results[0].ToolUseID != "tu_1" || results[0].IsError {
		t.Errorf("result[0]: expected successful tu_1, got toolUseID=%q isError=%v", results[0].ToolUseID, results[0].IsError)
	}

	// Second result: skipped tu_2
	if results[1].ToolUseID != "tu_2" || !results[1].IsError {
		t.Errorf("result[1]: expected skipped tu_2, got toolUseID=%q isError=%v", results[1].ToolUseID, results[1].IsError)
	}
	if results[1].Content != "Skipped: user redirected the conversation" {
		t.Errorf("result[1] content = %q", results[1].Content)
	}

	// Third result: skipped tu_3
	if results[2].ToolUseID != "tu_3" || !results[2].IsError {
		t.Errorf("result[2]: expected skipped tu_3, got toolUseID=%q isError=%v", results[2].ToolUseID, results[2].IsError)
	}

	// Fourth result: steer text block
	if results[3].Type != "text" || results[3].Text != "[user] stop and do something else" {
		t.Errorf("result[3]: expected steer text block, got type=%q text=%q", results[3].Type, results[3].Text)
	}
}

// TestExecuteToolCalls_NoSteer verifies that when SteerCheckFunc always
// returns "", all tools execute normally and no steer text is injected.
func TestExecuteToolCalls_NoSteer(t *testing.T) {
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

	ctx := WithTurnCallbacks(context.Background(), &TurnCallbacks{
		SteerCheckFunc: func() string { return "" },
	})

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

// TestExecuteToolCalls_SteerBeforeFirstTool verifies that if steer arrives
// before the first tool executes, all tools are skipped.
func TestExecuteToolCalls_SteerBeforeFirstTool(t *testing.T) {
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
	ctx := WithTurnCallbacks(context.Background(), &TurnCallbacks{
		SteerCheckFunc: func() string { return "abort" },
	})

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

	// 2 skipped tool results + 1 steer text = 3
	if len(results) != 3 {
		t.Fatalf("got %d result blocks, want 3", len(results))
	}

	if results[2].Type != "text" || results[2].Text != "[user] abort" {
		t.Errorf("steer block: type=%q text=%q", results[2].Type, results[2].Text)
	}
}

// TestSteerInjectedAfterToolBatch verifies that steer messages arriving after
// all tools complete are injected into tool results before the next API call.
// Uses a full HandleMessage flow with a mock server.
func TestSteerInjectedAfterToolBatch(t *testing.T) {
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
	ctx := WithTurnCallbacks(context.Background(), &TurnCallbacks{
		SteerCheckFunc: func() string {
			checkCount++
			// The first check is inside executeToolCalls (before the tool).
			// The second check is the post-batch check in agent.go.
			if checkCount == 2 {
				return "new direction please"
			}
			return ""
		},
	})

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	resp, err := ag.HandleMessageWithAttachments(ctx, "test/imain/1000000000", "Do something", nil)
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if resp != "Got your redirect." {
		t.Errorf("response = %q, want %q", resp, "Got your redirect.")
	}
}
