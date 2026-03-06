package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestMaxTokensWarning(t *testing.T) {
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("This response was cut off bec"),
			StopReason: "max_tokens",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 8192},
		}
	})
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
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Normal response."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 50},
		}
	})
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

func TestBraindeadWarningInjected(t *testing.T) {
	var callCount atomic.Int32
	threshold := 3

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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

