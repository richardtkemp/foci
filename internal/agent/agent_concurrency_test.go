package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestConcurrentTurnSerialization(t *testing.T) {
	// Verify that concurrent HandleMessage calls on the same session
	// are serialized — messages never interleave. This is critical for
	// Anthropic's prefix-matched prompt cache: the conversation history
	// sent to the API must be a strict append-only extension of the
	// previous request.

	var mu sync.Mutex
	var apiCallOrder []string // tracks which turn's messages were seen by API

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		// Identify which turn this is by looking at the last user message
		lastMsg := req.Messages[len(req.Messages)-1]
		text := provider.TextOf(lastMsg.Content)

		mu.Lock()
		apiCallOrder = append(apiCallOrder, text)
		mu.Unlock()

		// Slow down the first turn so concurrent callers pile up
		if strings.Contains(text, "Turn A") {
			time.Sleep(100 * time.Millisecond)
		}

		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Reply to: " + text),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	sessionKey := "test/iconcurrent/1000000000"

	// Launch two concurrent turns on the same session
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		ag.HandleMessage(context.Background(), sessionKey, "Turn A")
	}()

	// Small delay to ensure Turn A acquires the lock first
	time.Sleep(10 * time.Millisecond)

	go func() {
		defer wg.Done()
		ag.HandleMessage(context.Background(), sessionKey, "Turn B")
	}()

	wg.Wait()

	// Verify session has 4 messages in order: user A, assistant A, user B, assistant B
	msgs, err := store.Load(sessionKey)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4", len(msgs))
	}

	// Messages must be strictly ordered: Turn A's pair first, then Turn B's pair
	textA := provider.TextOf(msgs[0].Content)
	if !strings.Contains(textA, "Turn A") {
		t.Errorf("msgs[0] should be Turn A's user message, got: %s", textA)
	}
	if msgs[0].Role != "user" {
		t.Errorf("msgs[0].Role = %q, want user", msgs[0].Role)
	}

	replyA := provider.TextOf(msgs[1].Content)
	if !strings.Contains(replyA, "Reply to") {
		t.Errorf("msgs[1] should be Turn A's assistant reply, got: %s", replyA)
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msgs[1].Role = %q, want assistant", msgs[1].Role)
	}

	textB := provider.TextOf(msgs[2].Content)
	if !strings.Contains(textB, "Turn B") {
		t.Errorf("msgs[2] should be Turn B's user message, got: %s", textB)
	}
	if msgs[2].Role != "user" {
		t.Errorf("msgs[2].Role = %q, want user", msgs[2].Role)
	}

	replyB := provider.TextOf(msgs[3].Content)
	if !strings.Contains(replyB, "Reply to") {
		t.Errorf("msgs[3] should be Turn B's assistant reply, got: %s", replyB)
	}
	if msgs[3].Role != "assistant" {
		t.Errorf("msgs[3].Role = %q, want assistant", msgs[3].Role)
	}

	// Verify Turn B's API request included Turn A's messages (prefix stability)
	mu.Lock()
	defer mu.Unlock()
	if len(apiCallOrder) != 2 {
		t.Fatalf("expected 2 API calls, got %d", len(apiCallOrder))
	}
	// Turn A should have been processed first
	if !strings.Contains(apiCallOrder[0], "Turn A") {
		t.Errorf("first API call should be Turn A, got: %s", apiCallOrder[0])
	}
	if !strings.Contains(apiCallOrder[1], "Turn B") {
		t.Errorf("second API call should be Turn B, got: %s", apiCallOrder[1])
	}
}

func TestConcurrentTurnsDifferentSessions(t *testing.T) {
	// Verify that turns on DIFFERENT sessions can run concurrently
	// (the per-session lock should not serialize across sessions).
	var mu sync.Mutex
	var activeConcurrent int32
	var maxConcurrent int32

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		cur := atomic.AddInt32(&activeConcurrent, 1)
		defer atomic.AddInt32(&activeConcurrent, -1)

		mu.Lock()
		if cur > maxConcurrent {
			maxConcurrent = cur
		}
		mu.Unlock()

		time.Sleep(50 * time.Millisecond) // ensure overlap

		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("OK"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		ag.HandleMessage(context.Background(), "test/isessionA/1000000000", "Hello A")
	}()

	go func() {
		defer wg.Done()
		ag.HandleMessage(context.Background(), "test/isessionB/1000000000", "Hello B")
	}()

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if maxConcurrent < 2 {
		t.Errorf("different sessions should run concurrently, max concurrent = %d", maxConcurrent)
	}
}

func TestConcurrentTurnCancellation(t *testing.T) {
	// Verify that a cancelled context while waiting for the turn lock
	// returns immediately without processing.
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		time.Sleep(200 * time.Millisecond) // slow turn
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Done"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	sessionKey := "test/icancelq/1000000000"

	// Start a slow turn
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ag.HandleMessage(context.Background(), sessionKey, "Slow turn")
	}()

	time.Sleep(20 * time.Millisecond) // let the slow turn start

	// Start a second turn with a context that we'll cancel
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := ag.HandleMessage(ctx, sessionKey, "Should not process")
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got: %v", err)
	}

	wg.Wait()

	// Only the first turn's messages should be in the session
	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (only the slow turn)", len(msgs))
	}
}

