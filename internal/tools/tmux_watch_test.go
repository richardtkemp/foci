package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTmuxWatchUnwatch verifies basic watch and unwatch operations.
func TestTmuxWatchUnwatch(t *testing.T) {
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

	name := "foci-test-watch"
	tmuxSetup(t, name)

	// Start a session
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Watch
	params, _ = json.Marshal(map[string]interface{}{
		"operation":         "watch",
		"name":              name,
		"threshold_seconds": 5,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if !strings.Contains(result.Text, "Watching") {
		t.Errorf("watch result = %q", result.Text)
	}

	// Unwatch
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unwatch: %v", err)
	}
	if !strings.Contains(result.Text, "Stopped watching") {
		t.Errorf("unwatch result = %q", result.Text)
	}
}

// TestTmuxWatchAlreadyWatched verifies that watching an already-watched session fails.
func TestTmuxWatchAlreadyWatched(t *testing.T) {
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

	name := "foci-test-watch-dup"
	tmuxSetup(t, name)

	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Watch once
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "watch",
		"name":      name,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("first watch: %v", err)
	}

	// Watch again — should error
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for duplicate watch")
	}
	if !strings.Contains(err.Error(), "already being watched") {
		t.Errorf("error = %q", err.Error())
	}

	// Cleanup watch
	unwatchParams, _ := json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	tool.Execute(context.Background(), unwatchParams)
}

// TestTmuxUnwatchNotWatched verifies that unwatching a non-watched session fails.
func TestTmuxUnwatchNotWatched(t *testing.T) {
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

	params, _ := json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      "nonexistent-session",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for unwatching non-watched session")
	}
	if !strings.Contains(err.Error(), "not being watched") {
		t.Errorf("error = %q", err.Error())
	}
}

// TestTmuxWatchWakeCallback verifies that the wake callback fires on inactivity.
func TestTmuxWatchWakeCallback(t *testing.T) {
	t.Parallel()
	tmuxAvailable(t)

	var wakeCalled atomic.Int32
	var wakeMsg string
	notifier := NewAsyncNotifier(func(sk, msg string, replyTo string) {
		wakeCalled.Add(1)
		wakeMsg = msg
	})

	_, tool, _ := NewTmuxTool(300, 30, notifier, nil, "", false, 30, 0)

	name := "foci-test-wake"
	tmuxSetup(t, name)

	// Start a session that does nothing (sleep) — watch=false to control watch params below
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
		"watch":     false,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Watch with very short threshold (3 seconds)
	params, _ = json.Marshal(map[string]interface{}{
		"operation":         "watch",
		"name":              name,
		"threshold_seconds": 3,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("watch: %v", err)
	}

	// Wait for the wake callback to fire (threshold 3s + poll interval 2s)
	deadline := time.After(10 * time.Second)
	for wakeCalled.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("wake callback not called within timeout")
		case <-time.After(200 * time.Millisecond):
		}
	}

	if !strings.Contains(wakeMsg, name) {
		t.Errorf("wake message = %q, want to contain session name %q", wakeMsg, name)
	}
	if !strings.Contains(wakeMsg, "TMUX WATCH") {
		t.Errorf("wake message = %q, want to contain TMUX WATCH", wakeMsg)
	}
	if !strings.Contains(wakeMsg, "SYSTEM INJECTION") {
		t.Errorf("wake message = %q, want to contain context note", wakeMsg)
	}

	// Cleanup
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	tool.Execute(context.Background(), params)
}

// TestTmuxWatchDeadSession verifies that dead sessions are cleaned up from watch state.
func TestTmuxWatchDeadSession(t *testing.T) {
	t.Parallel()
	tmuxAvailable(t)

	var msgs []string
	var mu sync.Mutex
	notifier := NewAsyncNotifier(func(sk, msg string, replyTo string) {
		mu.Lock()
		msgs = append(msgs, msg)
		mu.Unlock()
	})

	_, tool, _ := NewTmuxTool(300, 30, notifier, nil, "", false, 30, 0)

	name := "foci-test-dead"
	tmuxSetup(t, name)

	// Start a session — watch=false to control watch params below
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
		"watch":     false,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Watch it
	params, _ = json.Marshal(map[string]interface{}{
		"operation":         "watch",
		"name":              name,
		"threshold_seconds": 60, // long threshold — we're testing death, not inactivity
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("watch: %v", err)
	}

	// Kill the tmux session externally
	exec.Command("tmux", "-S", tmuxSocketPath, "kill-session", "-t", name).Run()
	time.Sleep(100 * time.Millisecond)

	// Give the monitor time to detect the dead session (poll interval is 2s)
	time.Sleep(2500 * time.Millisecond)

	// The watch entry should have been cleaned up — unwatching should fail
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Error("expected error unwatching already-cleaned-up session")
	}
	if !strings.Contains(err.Error(), "not being watched") {
		t.Errorf("error = %q, want 'not being watched'", err.Error())
	}

	// Verify that no "no longer exists" notification was sent
	mu.Lock()
	for _, msg := range msgs {
		if strings.Contains(msg, "no longer exists") {
			mu.Unlock()
			t.Errorf("unexpected notification: %q", msg)
			return
		}
	}
	mu.Unlock()
}

// TestTmuxWatchMissingName verifies that watch/unwatch without name are rejected.
func TestTmuxWatchMissingName(t *testing.T) {
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

	params, _ := json.Marshal(map[string]interface{}{
		"operation": "watch",
	})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for watch missing name")
	}

	params, _ = json.Marshal(map[string]interface{}{
		"operation": "unwatch",
	})
	_, err = tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for unwatch missing name")
	}
}
