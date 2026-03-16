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

func TestTmuxWatchUnwatch(t *testing.T) {
	// Verifies that watch registers monitoring for a session and unwatch stops it, both returning appropriate confirmation messages.
	t.Parallel()
	tmuxAvailable(t)
	_, tool, _, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

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

func TestTmuxWatchAlreadyWatched(t *testing.T) {
	// Verifies that a second watch call on an already-watched session returns a clear error, preventing duplicate watch registrations.
	t.Parallel()
	tmuxAvailable(t)
	_, tool, _, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

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

func TestTmuxUnwatchNotWatched(t *testing.T) {
	// Verifies that unwatching a session that was never watched returns a clear error, guarding against invalid state transitions.
	t.Parallel()
	_, tool, _, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

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

func TestTmuxWatchWakeCallback(t *testing.T) {
	// Verifies that when a watched session is inactive longer than the threshold, the wake notifier fires with a message containing the session name and TMUX WATCH context.
	t.Parallel()
	tmuxAvailable(t)

	var wakeMsg atomic.Value
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		wakeMsg.Store(msg)
	})

	_, tool, _, _ := NewTmuxTool(300, 30, notifier, nil, "", false, 30, 0)

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
	for wakeMsg.Load() == nil {
		select {
		case <-deadline:
			t.Fatal("wake callback not called within timeout")
		case <-time.After(200 * time.Millisecond):
		}
	}

	msg := wakeMsg.Load().(string)
	if !strings.Contains(msg, name) {
		t.Errorf("wake message = %q, want to contain session name %q", msg, name)
	}
	if !strings.Contains(msg, "TMUX WATCH") {
		t.Errorf("wake message = %q, want to contain TMUX WATCH", msg)
	}
	if !strings.Contains(msg, "SYSTEM INJECTION") {
		t.Errorf("wake message = %q, want to contain context note", msg)
	}

	// Cleanup
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	tool.Execute(context.Background(), params)
}

func TestTmuxWatchDeadSession(t *testing.T) {
	// Verifies that when a watched session is externally killed, the watch state is cleaned up automatically without sending a spurious notification.
	t.Parallel()
	tmuxAvailable(t)

	var msgs []string
	var mu sync.Mutex
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		mu.Lock()
		msgs = append(msgs, msg)
		mu.Unlock()
	})

	_, tool, _, _ := NewTmuxTool(300, 30, notifier, nil, "", false, 30, 0)

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

func TestTmuxWatchMissingName(t *testing.T) {
	// Verifies that both watch and unwatch fail with an error when no session name is provided, enforcing required-parameter validation.
	t.Parallel()
	_, tool, _, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

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
