package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestCacheStrategyInRequest(t *testing.T) {
	// Verify that the agent sets CacheStrategy on the API request.
	var receivedReq *provider.MessageRequest

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	ag := &Agent{
		Client:        client,
		Sessions:      store,
		Tools:         registry,
		Bootstrap:     bootstrap,
		Model:         "claude-haiku-4-5",
		CacheStrategy: "explicit",
		ModelDefaultsFn: func(model string) config.ModelDefaults {
			return config.ModelDefaults{CacheTTL: "1h"}
		},
	}

	ag.HandleMessage(context.Background(), "test/icache/1000000000", "Hello")

	if receivedReq == nil {
		t.Fatal("no request received")
	}

	// CacheStrategy should be set on the request
	if receivedReq.CacheStrategy != "explicit" {
		t.Errorf("CacheStrategy = %q, want explicit", receivedReq.CacheStrategy)
	}
	if receivedReq.CacheTTL != "1h" {
		t.Errorf("CacheTTL = %q, want 1h", receivedReq.CacheTTL)
	}

	// Messages should be passed as-is (no deep copy, no markers)
	// — cache markers are applied at the translate boundary, not here.
}

func TestCacheBustDetection(t *testing.T) {
	// Proves that when cache_read_input_tokens drops from a high value to zero on
	// consecutive turns, the CacheBustAlert hook fires with the correct session and token counts.
	callCount := 0
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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
		CacheBustAlert: HookList[CacheBustFunc]{func(session string, prevRead, curRead int) {
			alerts = append(alerts, fmt.Sprintf("%s:%d→%d", session, prevRead, curRead))
		}},
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
	// Proves that if a session has been idle longer than CacheBustIdleThreshold,
	// a cache_read drop is not reported as a bust (cache expiry is expected, not a problem).
	callCount := 0
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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
		CacheBustAlert: HookList[CacheBustFunc]{func(session string, prevRead, curRead int) {
			alerts = append(alerts, fmt.Sprintf("%s:%d→%d", session, prevRead, curRead))
		}},
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
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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
		CacheBustAlert: HookList[CacheBustFunc]{func(session string, prevRead, curRead int) {
			alerts = append(alerts, fmt.Sprintf("%s:%d→%d", session, prevRead, curRead))
		}},
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
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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
		CacheBustAlert: HookList[CacheBustFunc]{func(session string, prevRead, curRead int) {
			alerts = append(alerts, fmt.Sprintf("%s:%d→%d", session, prevRead, curRead))
		}},
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

func TestCacheBustFiresForAllFormats(t *testing.T) {
	// Cache bust detection is format-agnostic: any provider that reports
	// CacheReadInputTokens should trigger alerts when cache reads drop.
	for _, format := range []string{"gemini", "openai", "anthropic"} {
		t.Run(format, func(t *testing.T) {
			callCount := 0
			client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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
			store := session.NewStore(t.TempDir())
			registry := tools.NewRegistry()
			bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

			var alerts []string
			ag := &Agent{
				Client:          client,
				Sessions:        store,
				Tools:           registry,
				Bootstrap:       bootstrap,
				Model:           "test-model",
				Format:          format,
				CacheBustDetect: true,
				CacheBustAlert: HookList[CacheBustFunc]{func(session string, prevRead, curRead int) {
					alerts = append(alerts, fmt.Sprintf("%s:%d→%d", session, prevRead, curRead))
				}},
			}

			ag.HandleMessage(context.Background(), "test/imain/1000000000", "msg1")
			ag.HandleMessage(context.Background(), "test/imain/1000000000", "msg2")

			if len(alerts) != 1 {
				t.Fatalf("expected 1 alert for %s format, got %d: %v", format, len(alerts), alerts)
			}
		})
	}
}

