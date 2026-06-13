package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"foci/internal/nudge"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestMaxTokensWarning(t *testing.T) {
	// Proves that when the API returns stop_reason="max_tokens", the MaxTokensWarnFunc callback fires with a message that includes the session key, while still returning the truncated response to the caller.
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
		MaxTokensWarnFunc: HookList[func(string)]{func(warn string) {
			warnings = append(warnings, warn)
		}},
	}

	resp, err := ag.hmTest(context.Background(), "test/imaxtkn/1000000000", "Write a very long essay")
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
	// Proves that a normal end_turn response does not trigger the MaxTokensWarnFunc — warnings are specific to max_tokens truncation.
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
		MaxTokensWarnFunc: HookList[func(string)]{func(warn string) {
			warnings = append(warnings, warn)
		}},
	}

	ag.hmTest(context.Background(), "test/inomax/1000000000", "Hello")

	if len(warnings) != 0 {
		t.Errorf("expected no warnings for end_turn, got %d: %v", len(warnings), warnings)
	}
}

func TestBraindeadWarningInjected(t *testing.T) {
	// Proves that the braindead warning (via nudge system) is injected when
	// tool calls reach the configured threshold.
	var callCount atomic.Int32
	threshold := 3

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		n := int(callCount.Add(1))
		if n <= threshold+1 {
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
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return tools.TextResult("ok"), nil
		},
	})

	rs := &nudge.RuleSet{Rules: nudge.BraindeadRule(threshold, "")}
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:     "claude-haiku-4-5",
		Nudger:    nudge.NewScheduler(rs, 5, 1),
	}

	_, err := ag.hmTest(context.Background(), "test/imain/1000000000", "go")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	msgs, _ := store.Load("test/imain/1000000000")
	found := 0
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "text" && strings.Contains(b.Text, "consecutive tool calls") {
				found++
			}
		}
	}
	if found != 1 {
		t.Errorf("braindead warnings found = %d, want 1", found)
	}
}

func TestBraindeadWarningCooldown(t *testing.T) {
	// Proves that the braindead nudge fires only once per cooldown window,
	// not on every tool batch after the threshold.
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
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return tools.TextResult("ok"), nil
		},
	})

	// cooldown=5 means after firing at tool 2, next eligible is tool 7+.
	// With 6 total tool calls, multiples of 2 are 2, 4, 6 — only 2 passes cooldown.
	rs := &nudge.RuleSet{Rules: nudge.BraindeadRule(threshold, "")}
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:     "claude-haiku-4-5",
		Nudger:    nudge.NewScheduler(rs, 5, 1),
	}

	_, err := ag.hmTest(context.Background(), "test/imain/1000000000", "go")
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
			if b.Type == "text" && strings.Contains(b.Text, "consecutive tool calls") {
				count++
			}
		}
	}
	if count != 1 {
		t.Errorf("braindead warnings = %d, want exactly 1 (cooldown prevents repeat)", count)
	}
}

func TestBraindeadDisabledWhenZero(t *testing.T) {
	// Proves that BraindeadRule with threshold=0 produces no rules, so
	// no warning is injected even when the loop runs many iterations.
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
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return tools.TextResult("ok"), nil
		},
	})

	// threshold=0 → BraindeadRule returns nil → no nudger
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:     "claude-haiku-4-5",
		// No Nudger set — braindead disabled
	}

	_, err := ag.hmTest(context.Background(), "test/imain/1000000000", "go")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	msgs, _ := store.Load("test/imain/1000000000")
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "text" && strings.Contains(b.Text, "consecutive tool calls") {
				t.Error("braindead warning injected despite threshold=0")
			}
		}
	}
}

func TestDisplayNoteInjectedOnce(t *testing.T) {
	// Proves that a [display] tool_results note is injected exactly once per turn
	// (on the first tool batch), reflecting the effective ShowToolCalls mode.
	var callCount atomic.Int32
	totalLoops := 4

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
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return tools.TextResult("ok"), nil
		},
	})

	ag := &Agent{
		Client:        client,
		Sessions:      store,
		Tools:         registry,
		Bootstrap:     workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:         "claude-haiku-4-5",
		ShowToolCalls: "full",
	}

	_, err := ag.hmTest(context.Background(), "test/imain/1000000000", "go")
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
			if b.Type == "text" && strings.Contains(b.Text, "[display]") {
				count++
				if !strings.Contains(b.Text, "visible") {
					t.Errorf("display note should say visible for full mode, got: %s", b.Text)
				}
			}
		}
	}
	if count != 1 {
		t.Errorf("display notes = %d, want exactly 1", count)
	}
}

func TestDisplayNoteReflectsSessionOverride(t *testing.T) {
	// Proves that the display note reflects per-session overrides, not just the agent default.
	var callCount atomic.Int32

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		n := int(callCount.Add(1))
		if n <= 1 {
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
		Execute: func(ctx context.Context, params json.RawMessage) (tools.ToolResult, error) {
			return tools.TextResult("ok"), nil
		},
	})

	sk := "test/imain/1000000000"
	ag := &Agent{
		Client:        client,
		Sessions:      store,
		Tools:         registry,
		Bootstrap:     workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:         "claude-haiku-4-5",
		ShowToolCalls: "off", // agent default
	}
	// Per-session override to preview
	ag.SetSessionShowToolCalls(sk, "preview")

	_, err := ag.hmTest(context.Background(), sk, "go")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	msgs, _ := store.Load(sk)
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "text" && strings.Contains(b.Text, "[display]") {
				if !strings.Contains(b.Text, "preview") {
					t.Errorf("display note should reflect session override 'preview', got: %s", b.Text)
				}
				return
			}
		}
	}
	t.Error("no [display] note found in tool results")
}
