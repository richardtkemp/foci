package tmux

import (
	"context"
	"encoding/json"
	"foci/internal/tools"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTmuxWatchUnwatch(t *testing.T) {
	// Verifies that watch registers monitoring for a session and unwatch stops it, both returning appropriate confirmation messages.
	t.Parallel()
	sock := tmuxIsolatedSocket(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, sock)

	name := "foci-test-watch"

	// Start a session
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   testSessionCmd,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

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
	sock := tmuxIsolatedSocket(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, sock)

	name := "foci-test-watch-dup"

	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   testSessionCmd,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

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
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, "")

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
	sock := tmuxIsolatedSocket(t)

	var mu sync.Mutex
	var wakes []string
	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string, _ *tools.AgentOrigin) {
		mu.Lock()
		wakes = append(wakes, msg)
		mu.Unlock()
	})
	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), wakes...)
	}

	// The monitor polls and sees time pass only when the driver says so.
	d := newWatchDriver(t)
	_, tool := d.tool(notifier, nil, "", false, 30, sock)

	name := "foci-test-wake"

	// Start a session that does nothing (sleep) — watch=false to control watch params below
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   testSessionCmd,
		"watch":     false,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	const threshold = 3 * time.Second
	params, _ = json.Marshal(map[string]interface{}{
		"operation":         "watch",
		"name":              name,
		"threshold_seconds": int(threshold / time.Second),
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("watch: %v", err)
	}

	// Exactly at the threshold the session has not been idle LONGER than it:
	// no wake yet. (A monitor that fires on every poll fails here.)
	d.poll()
	d.advance(threshold)
	d.poll()
	if got := snapshot(); len(got) != 0 {
		t.Fatalf("wake fired at exactly the threshold: %q", got)
	}

	// Past the threshold the wake fires, once.
	d.advance(time.Millisecond)
	d.poll()
	got := snapshot()
	if len(got) != 1 {
		t.Fatalf("wake callback calls = %d after idling past the threshold, want 1: %q", len(got), got)
	}

	msg := got[0]
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
	sock := tmuxIsolatedSocket(t)

	var msgs []string
	var mu sync.Mutex
	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string, _ *tools.AgentOrigin) {
		mu.Lock()
		msgs = append(msgs, msg)
		mu.Unlock()
	})

	d := newWatchDriver(t)
	watchCount, tool := d.tool(notifier, nil, "", false, 30, sock)

	name := "foci-test-dead"

	// Start a session — watch=false to control watch params below
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   testSessionCmd,
		"watch":     false,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Watch it
	params, _ = json.Marshal(map[string]interface{}{
		"operation":         "watch",
		"name":              name,
		"threshold_seconds": 60, // long threshold — we're testing death, not inactivity
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("watch: %v", err)
	}

	// A poll of a live session keeps the watch.
	d.poll()
	if n := watchCount(); n != 1 {
		t.Fatalf("watch count = %d after polling a live session, want 1", n)
	}

	// Kill the tmux session externally
	if out, err := runTmuxWithSocket(context.Background(), sock, "kill-session", "-t", name); err != nil {
		t.Fatalf("kill-session: %v: %s", err, out)
	}

	// The next poll's capture-pane fails, and the monitor's death path
	// (pollWatch's capture-pane error branch) deletes the watch and exits
	// WITHOUT ever calling the notifier. poll() returns once that is done.
	d.poll()
	if n := watchCount(); n != 0 {
		t.Fatalf("watch count = %d after polling a dead session, want 0 (watch not cleaned up)", n)
	}

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
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, "")

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
