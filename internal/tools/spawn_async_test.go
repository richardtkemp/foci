package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSpawnInheritSemaphore(t *testing.T) {
	t.Parallel()
	var concurrentCount int32
	var maxConcurrent int32

	mockSessions := &mockSessionBrancher{}

	// Use notifier to detect completion of background spawns.
	var completions int32
	allDone := make(chan struct{})
	notifier := NewAsyncNotifier(func(sk, msg string, replyTo string) {
		if c := atomic.AddInt32(&completions, 1); c == 4 {
			close(allDone)
		}
	})

	deps := SpawnDeps{
		Sessions:   mockSessions,
		AgentID:    "test",
		Model:      "anthropic/claude-haiku-4-5",
		MaxInherit: 2, // only allow 2 concurrent
		Notifier:   notifier,
	}

	// Agent that takes 50ms and tracks concurrency
	tool := NewSpawnTool(deps, func() SpawnAgent {
		return &concurrentAgent{
			concurrentCount: &concurrentCount,
			maxConcurrent:   &maxConcurrent,
		}
	})

	ctx := WithSessionKey(context.Background(), "test/imain/1000000000")

	// Launch 4 concurrent inherit calls (all return immediately with ack)
	for i := 0; i < 4; i++ {
		params, _ := json.Marshal(map[string]string{
			"prompt":  "task",
			"context": "clone",
		})
		result, err := tool.Execute(ctx, params)
		if err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
		if !strings.Contains(result.Text, "Spawn started in background") {
			t.Fatalf("spawn %d: expected async ack, got %q", i, result.Text)
		}
	}

	// Wait for all background goroutines to complete
	select {
	case <-allDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for background spawns")
	}

	// MaxConcurrent should never exceed 2
	if mc := atomic.LoadInt32(&maxConcurrent); mc > 2 {
		t.Errorf("max concurrent = %d, want <= 2", mc)
	}
}

func TestSpawnInheritAsyncDelivery(t *testing.T) {
	// Verify the notifier receives [SPAWN RESULT] with correct session and content.
	t.Parallel()
	delivered := make(chan struct{ sk, msg string }, 1)
	notifier := NewAsyncNotifier(func(sk, msg string, replyTo string) {
		delivered <- struct{ sk, msg string }{sk, msg}
	})

	mockAgent := &channelSpawnAgent{response: "Research complete.", called: make(chan struct{}, 1)}
	mockSessions := &mockSessionBrancher{}

	deps := SpawnDeps{
		Sessions:   mockSessions,
		AgentID:    "test",
		Model:      "anthropic/claude-haiku-4-5",
		MaxInherit: 3,
		Notifier:   notifier,
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	ctx := WithSessionKey(context.Background(), "test/imain/1000000000")
	params, _ := json.Marshal(map[string]string{
		"prompt":  "Do research",
		"context": "clone",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Spawn started in background") {
		t.Fatalf("expected async ack, got %q", result.Text)
	}

	select {
	case d := <-delivered:
		if d.sk != "test/imain/1000000000" {
			t.Errorf("notified session = %q, want agent:test:main", d.sk)
		}
		if !strings.Contains(d.msg, "[SPAWN RESULT]") {
			t.Errorf("expected [SPAWN RESULT] tag, got %q", d.msg)
		}
		if !strings.Contains(d.msg, "completed:") {
			t.Errorf("expected 'completed:' in msg, got %q", d.msg)
		}
		if !strings.Contains(d.msg, "Research complete.") {
			t.Errorf("expected agent result, got %q", d.msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notifier not called")
	}
}

func TestSpawnInheritAsyncError(t *testing.T) {
	// Verify errors are delivered via notifier with "failed:" tag.
	t.Parallel()
	delivered := make(chan string, 1)
	notifier := NewAsyncNotifier(func(sk, msg string, replyTo string) {
		delivered <- msg
	})

	mockAgent := &channelSpawnAgent{
		err:    fmt.Errorf("tool execution failed: timeout"),
		called: make(chan struct{}, 1),
	}
	mockSessions := &mockSessionBrancher{}

	deps := SpawnDeps{
		Sessions:   mockSessions,
		AgentID:    "test",
		Model:      "anthropic/claude-haiku-4-5",
		MaxInherit: 3,
		Notifier:   notifier,
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	ctx := WithSessionKey(context.Background(), "test/imain/1000000000")
	params, _ := json.Marshal(map[string]string{
		"prompt":  "Do task",
		"context": "clone",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Spawn started in background") {
		t.Fatalf("expected async ack, got %q", result.Text)
	}

	select {
	case msg := <-delivered:
		if !strings.Contains(msg, "[SPAWN RESULT]") {
			t.Errorf("expected [SPAWN RESULT] tag, got %q", msg)
		}
		if !strings.Contains(msg, "failed:") {
			t.Errorf("expected 'failed:' in msg, got %q", msg)
		}
		if !strings.Contains(msg, "tool execution failed: timeout") {
			t.Errorf("expected error message, got %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notifier not called")
	}
}

func TestSpawnInheritNilNotifierSync(t *testing.T) {
	// Nil notifier = synchronous fallback (existing behavior preserved).
	t.Parallel()
	mockAgent := &mockSpawnAgent{response: "Sync result."}
	mockSessions := &mockSessionBrancher{}

	deps := SpawnDeps{
		Sessions:   mockSessions,
		AgentID:    "test",
		Model:      "anthropic/claude-haiku-4-5",
		MaxInherit: 3,
		// Notifier intentionally nil
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	ctx := WithSessionKey(context.Background(), "test/imain/1000000000")
	params, _ := json.Marshal(map[string]string{
		"prompt":  "Do task",
		"context": "clone",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should return the actual result, not an async ack
	if result.Text != "Sync result." {
		t.Errorf("result = %q, want Sync result.", result.Text)
	}

	// Agent should have been called synchronously
	if mockAgent.message != "Do task" {
		t.Errorf("message = %q, want Do task", mockAgent.message)
	}
}
