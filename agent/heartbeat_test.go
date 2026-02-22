package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"clod/anthropic"
	"clod/session"
	"clod/tools"
	"clod/workspace"
)

func TestHeartbeatFiresOnIdle(t *testing.T) {
	var handleCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(anthropic.MessageResponse{
			ID:         "msg_hb",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("heartbeat handled"),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 5},
		})
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	hb := NewHeartbeat(ag, "agent:test:hb", 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hb.Start(ctx)

	// Wait for at least one fire
	time.Sleep(200 * time.Millisecond)

	hb.Stop()

	if handleCount.Load() < 1 {
		t.Errorf("heartbeat fired %d times, want >= 1", handleCount.Load())
	}
}

func TestHeartbeatReset(t *testing.T) {
	var handleCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(anthropic.MessageResponse{
			ID:         "msg_hb",
			Content:    anthropic.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 5},
		})
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	hb := NewHeartbeat(ag, "agent:test:hb", 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hb.Start(ctx)

	// Keep resetting before the interval fires
	for i := 0; i < 5; i++ {
		time.Sleep(50 * time.Millisecond)
		hb.Reset()
	}

	hb.Stop()

	// Should not have fired because we kept resetting
	if handleCount.Load() > 0 {
		t.Errorf("heartbeat fired %d times, want 0 (kept resetting)", handleCount.Load())
	}
}

func TestHeartbeatStopPrevents(t *testing.T) {
	var handleCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(anthropic.MessageResponse{
			ID:         "msg_hb",
			Content:    anthropic.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 5},
		})
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-key")
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	hb := NewHeartbeat(ag, "agent:test:hb", 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hb.Start(ctx)
	hb.Stop()

	// Wait past when it would have fired
	time.Sleep(200 * time.Millisecond)

	if handleCount.Load() > 0 {
		t.Errorf("heartbeat fired %d times after Stop, want 0", handleCount.Load())
	}
}

func TestTruncateStr(t *testing.T) {
	tests := []struct {
		s    string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 5, "hello..."},
		{"", 5, ""},
		{"ab", 1, "a..."},
	}
	for _, tt := range tests {
		got := truncateStr(tt.s, tt.max)
		if got != tt.want {
			t.Errorf("truncateStr(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
		}
	}
}
