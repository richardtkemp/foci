package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

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

