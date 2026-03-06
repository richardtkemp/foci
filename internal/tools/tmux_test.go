package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"foci/internal/state"
)

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "foci-tmux-test-*")
	tmuxSocketPath = filepath.Join(dir, "tmux.sock")
	code := m.Run()
	exec.Command("tmux", "-S", tmuxSocketPath, "kill-server").Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func tmuxAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
}

// tmuxSetup pre-cleans named sessions (from prior crashed runs) and registers
// t.Cleanup to kill them when the test finishes. All operations use the
// test-isolated tmux socket.
func tmuxSetup(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		exec.Command("tmux", "-S", tmuxSocketPath, "kill-session", "-t", name).Run()
		t.Cleanup(func() {
			exec.Command("tmux", "-S", tmuxSocketPath, "kill-session", "-t", name).Run()
		})
	}
}

func TestTmuxStartAndList(t *testing.T) {
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

	name := "foci-test-start"
	tmuxSetup(t, name)

	// Start
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(result.Text,name) {
		t.Errorf("start result = %q, want session name", result.Text)
	}

	// List
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "list",
	})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(result.Text,name) {
		t.Errorf("list result = %q, want %q", result.Text,name)
	}
	// Should have header line
	if !strings.Contains(result.Text,"SESSION") {
		t.Errorf("list result missing header: %q", result.Text)
	}
	// Owned session should show owner (not "-")
	if !strings.Contains(result.Text,"self") {
		t.Errorf("list result missing owner: %q", result.Text)
	}
}

func TestTmuxSendAndRead(t *testing.T) {
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)


	name := "foci-test-sendread"
	tmuxSetup(t, name)

	// Start a session with cat (echoes input)
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "cat",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Send text
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      name,
		"keys":      "hello tmux",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("send: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Read output
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      name,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(result.Text,"hello tmux") {
		t.Errorf("read result = %q, want 'hello tmux'", result.Text)
	}
}

func TestTmuxReadDefault(t *testing.T) {
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)


	name := "foci-test-readdefault"
	tmuxSetup(t, name)

	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Read with default lines (50)
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      name,
	})
	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
}

func TestTmuxKill(t *testing.T) {
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)


	name := "foci-test-kill"
	tmuxSetup(t, name)

	// Start
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Kill
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "kill",
		"name":      name,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("kill: %v", err)
	}
	if !strings.Contains(result.Text,name) {
		t.Errorf("kill result = %q", result.Text)
	}

	// Verify gone from list
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "list",
	})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(result.Text,name) {
		t.Errorf("session %q still in list after kill", name)
	}
}

func TestTmuxInvalidOperation(t *testing.T) {
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)


	params, _ := json.Marshal(map[string]interface{}{
		"operation": "restart",
	})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for invalid operation")
	}
	if !strings.Contains(err.Error(), "unknown operation") {
		t.Errorf("error = %q, want 'unknown operation'", err.Error())
	}
}

func TestTmuxStartNoName(t *testing.T) {
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)


	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"command":   "sleep 60",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(result.Text,"foci-") {
		t.Errorf("result = %q, want auto-generated foci-N name", result.Text)
	}

	// Extract name and clean up
	name := strings.TrimPrefix(result.Text, "Session started: ")
	tmuxSetup(t, name)
}

func TestTmuxSendNoEnter(t *testing.T) {
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)


	name := "foci-test-noenter"
	tmuxSetup(t, name)

	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Send without enter
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      name,
		"keys":      "partial",
		"enter":     false,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if result.Text != "Keys sent." {
		t.Errorf("result = %q", result.Text)
	}
}

func TestTmuxSendBareEnter(t *testing.T) {
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

	name := "foci-test-bareenter"
	tmuxSetup(t, name)

	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Send bare Enter (no keys, enter=true)
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      name,
		"enter":     true,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("bare enter send should succeed: %v", err)
	}
	if result.Text != "Keys sent." {
		t.Errorf("result = %q, want %q", result.Text,"Keys sent.")
	}

	// Verify: no keys + no enter should fail
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      name,
		"enter":     false,
	})
	_, err = tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for empty keys with enter=false")
	}
}

func TestTmuxMissingName(t *testing.T) {
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)


	for _, op := range []string{"send", "read", "kill"} {
		params, _ := json.Marshal(map[string]interface{}{
			"operation": op,
		})
		_, err := tool.Execute(context.Background(), params)
		if err == nil {
			t.Errorf("%s: expected error for missing name", op)
		}
	}
}

func TestTmuxStartWithWorkdir(t *testing.T) {
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)


	name := "foci-test-workdir"
	tmuxSetup(t, name)

	dir := t.TempDir()
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"workdir":   dir,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("start with workdir: %v", err)
	}
	if !strings.Contains(result.Text,name) {
		t.Errorf("result = %q", result.Text)
	}

	time.Sleep(200 * time.Millisecond)

	// Send pwd and read output
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      name,
		"keys":      "pwd",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("send: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Read output — should show the working directory
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      name,
		"lines":     100,
	})
	output, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Resolve any symlinks (e.g. /tmp -> /private/tmp on macOS)
	resolvedDir, _ := filepath.EvalSymlinks(dir)
	if !strings.Contains(output.Text, dir) && !strings.Contains(output.Text, resolvedDir) {
		t.Errorf("output = %q, want workdir %q or %q", output.Text, dir, resolvedDir)
	}
}

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
	if !strings.Contains(result.Text,"Watching") {
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
	if !strings.Contains(result.Text,"Stopped watching") {
		t.Errorf("unwatch result = %q", result.Text)
	}
}

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

func TestTmuxInstanceIsolation(t *testing.T) {
	tmuxAvailable(t)

	_, toolA, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)
	_, toolB, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

	nameA := "foci-test-iso-a"
	nameB := "foci-test-iso-b"
	tmuxSetup(t, nameA, nameB)

	// Agent A starts a session
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      nameA,
		"command":   "sleep 60",
	})
	if _, err := toolA.Execute(context.Background(), params); err != nil {
		t.Fatalf("agent A start: %v", err)
	}

	// Agent B starts a session
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      nameB,
		"command":   "sleep 60",
	})
	if _, err := toolB.Execute(context.Background(), params); err != nil {
		t.Fatalf("agent B start: %v", err)
	}

	// List now shows ALL sessions — but ownership status differs per instance
	listParams, _ := json.Marshal(map[string]interface{}{
		"operation": "list",
	})
	result, err := toolA.Execute(context.Background(), listParams)
	if err != nil {
		t.Fatalf("agent A list: %v", err)
	}
	if !strings.Contains(result.Text,nameA) {
		t.Errorf("agent A list missing own session: %q", result.Text)
	}
	// A should see its own session with an owner and B's without
	for _, line := range strings.Split(result.Text, "\n") {
		if strings.Contains(line, nameA) && !strings.Contains(line, "self") {
			t.Errorf("agent A list should show %q with owner: %q", nameA, line)
		}
	}

	result, err = toolB.Execute(context.Background(), listParams)
	if err != nil {
		t.Fatalf("agent B list: %v", err)
	}
	if !strings.Contains(result.Text,nameB) {
		t.Errorf("agent B list missing own session: %q", result.Text)
	}
	// B should see its own session with an owner
	for _, line := range strings.Split(result.Text, "\n") {
		if strings.Contains(line, nameB) && !strings.Contains(line, "self") {
			t.Errorf("agent B list should show %q with owner: %q", nameB, line)
		}
	}

	// Agent B cannot read agent A's session
	readParams, _ := json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      nameA,
	})
	_, err = toolB.Execute(context.Background(), readParams)
	if err == nil {
		t.Fatal("agent B should not be able to read agent A's session")
	}
	if !strings.Contains(err.Error(), "not owned") {
		t.Errorf("error = %q, want 'not owned'", err.Error())
	}

	// Agent B cannot send to agent A's session
	sendParams, _ := json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      nameA,
		"keys":      "hello",
	})
	_, err = toolB.Execute(context.Background(), sendParams)
	if err == nil {
		t.Fatal("agent B should not be able to send to agent A's session")
	}
	if !strings.Contains(err.Error(), "not owned") {
		t.Errorf("error = %q, want 'not owned'", err.Error())
	}

	// Agent B cannot kill agent A's session
	killParams, _ := json.Marshal(map[string]interface{}{
		"operation": "kill",
		"name":      nameA,
	})
	_, err = toolB.Execute(context.Background(), killParams)
	if err == nil {
		t.Fatal("agent B should not be able to kill agent A's session")
	}
	if !strings.Contains(err.Error(), "not owned") {
		t.Errorf("error = %q, want 'not owned'", err.Error())
	}
}

func TestTmuxWakeRoutesToCorrectAgent(t *testing.T) {
	t.Parallel()
	tmuxAvailable(t)

	var wakeA, wakeB atomic.Int32
	_, toolA, _ := NewTmuxTool(300, 30, NewAsyncNotifier(func(sk, msg string, replyTo string) {
		wakeA.Add(1)
	}), nil, "", false, 30, 0)
	_, toolB, _ := NewTmuxTool(300, 30, NewAsyncNotifier(func(sk, msg string, replyTo string) {
		wakeB.Add(1)
	}), nil, "", false, 30, 0)

	nameA := "foci-test-wakeroute-a"
	nameB := "foci-test-wakeroute-b"
	tmuxSetup(t, nameA, nameB)

	// Agent A starts a session — watch=false to control watch params below
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      nameA,
		"command":   "sleep 60",
		"watch":     false,
	})
	if _, err := toolA.Execute(context.Background(), params); err != nil {
		t.Fatalf("agent A start: %v", err)
	}

	// Agent B starts a session — watch=false to control watch params below
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      nameB,
		"command":   "sleep 60",
		"watch":     false,
	})
	if _, err := toolB.Execute(context.Background(), params); err != nil {
		t.Fatalf("agent B start: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Agent A watches with short threshold
	params, _ = json.Marshal(map[string]interface{}{
		"operation":         "watch",
		"name":              nameA,
		"threshold_seconds": 3,
	})
	if _, err := toolA.Execute(context.Background(), params); err != nil {
		t.Fatalf("agent A watch: %v", err)
	}

	// Agent B watches with short threshold
	params, _ = json.Marshal(map[string]interface{}{
		"operation":         "watch",
		"name":              nameB,
		"threshold_seconds": 3,
	})
	if _, err := toolB.Execute(context.Background(), params); err != nil {
		t.Fatalf("agent B watch: %v", err)
	}

	// Wait for both wake callbacks to fire
	deadline := time.After(10 * time.Second)
	for wakeA.Load() == 0 || wakeB.Load() == 0 {
		select {
		case <-deadline:
			t.Fatalf("wake callbacks not called: A=%d B=%d", wakeA.Load(), wakeB.Load())
		case <-time.After(200 * time.Millisecond):
		}
	}

	// Each agent's wake function should have been called independently
	if wakeA.Load() < 1 {
		t.Errorf("agent A wake not called")
	}
	if wakeB.Load() < 1 {
		t.Errorf("agent B wake not called")
	}

	// Cleanup watches
	unwatchA, _ := json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      nameA,
	})
	toolA.Execute(context.Background(), unwatchA)

	unwatchB, _ := json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      nameB,
	})
	toolB.Execute(context.Background(), unwatchB)
}

func TestTmuxWatchIsolation(t *testing.T) {
	tmuxAvailable(t)

	_, toolA, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

	_, toolB, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)


	name := "foci-test-watchiso"
	tmuxSetup(t, name)

	// Agent A starts and watches
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
	})
	if _, err := toolA.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	watchParams, _ := json.Marshal(map[string]interface{}{
		"operation": "watch",
		"name":      name,
	})
	if _, err := toolA.Execute(context.Background(), watchParams); err != nil {
		t.Fatalf("agent A watch: %v", err)
	}

	// Agent B should be able to watch the same session name (separate instance)
	// This exercises that watched maps are independent
	if _, err := toolB.Execute(context.Background(), watchParams); err != nil {
		t.Fatalf("agent B watch should succeed on separate instance: %v", err)
	}

	// Agent A unwatch should not affect agent B
	unwatchParams, _ := json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	if _, err := toolA.Execute(context.Background(), unwatchParams); err != nil {
		t.Fatalf("agent A unwatch: %v", err)
	}

	// Agent B unwatch should still work (not already removed by A)
	if _, err := toolB.Execute(context.Background(), unwatchParams); err != nil {
		t.Fatalf("agent B unwatch should still work: %v", err)
	}
}

// --- TUI noise filter ---

func TestNormalizePaneContent_ElapsedTimers(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Running 1m 3s", "Running "},
		{"Elapsed: 2h 30m", "Elapsed: "},
		{"Time: 0m 5s remaining", "Time:  remaining"},
		{"took 45m 12s total", "took  total"},
	}
	for _, tt := range tests {
		got := normalizePaneContent(tt.input)
		if got != tt.want {
			t.Errorf("normalizePaneContent(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizePaneContent_Clocks(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Status bar 14:30 ready", "Status bar  ready"},
		{"Clock: 2:30:00 PM end", "Clock:  end"},
		{"Time 9:05 AM left", "Time  left"},
		{"[23:59:59]", "[]"},
	}
	for _, tt := range tests {
		got := normalizePaneContent(tt.input)
		if got != tt.want {
			t.Errorf("normalizePaneContent(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizePaneContent_TokenCountsPreserved(t *testing.T) {
	// Token counts indicate active work — should NOT be stripped
	tests := []string{
		"Context: 88,447 tokens used",
		"Used 1500 tokens",
		"12 tokens left",
	}
	for _, tt := range tests {
		got := normalizePaneContent(tt)
		if got != tt {
			t.Errorf("normalizePaneContent(%q) = %q, want unchanged (tokens indicate activity)", tt, got)
		}
	}
}

func TestNormalizePaneContent_PercentagesPreserved(t *testing.T) {
	// Percentages indicate active work — should NOT be stripped
	tests := []string{
		"44% used",
		"Context: 88.5% full",
		"Progress: 100%",
	}
	for _, tt := range tests {
		got := normalizePaneContent(tt)
		if got != tt {
			t.Errorf("normalizePaneContent(%q) = %q, want unchanged (percentages indicate activity)", tt, got)
		}
	}
}

func TestNormalizePaneContent_CostsPreserved(t *testing.T) {
	// Cost changes indicate active work — should NOT be stripped
	tests := []string{
		"Cost: $0.0430",
		"Total $12.50 spent",
	}
	for _, tt := range tests {
		got := normalizePaneContent(tt)
		if got != tt {
			t.Errorf("normalizePaneContent(%q) = %q, want unchanged (costs indicate activity)", tt, got)
		}
	}
}

func TestNormalizePaneContent_Durations(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Took 3.2s", "Took "},
		{"Response in 0.5s", "Response in "},
	}
	for _, tt := range tests {
		got := normalizePaneContent(tt.input)
		if got != tt.want {
			t.Errorf("normalizePaneContent(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizePaneContent_SpinnersPreserved(t *testing.T) {
	// Spinners indicate active work — should NOT be stripped
	input1 := "⠋ Loading..."
	input2 := "⠙ Loading..."
	norm1 := normalizePaneContent(input1)
	norm2 := normalizePaneContent(input2)
	if norm1 == norm2 {
		t.Errorf("spinner frames should be preserved (different frames = activity): %q vs %q", norm1, norm2)
	}
}

func TestNormalizePaneContent_PreservesContent(t *testing.T) {
	// Meaningful content should be preserved
	tests := []string{
		"$ ls -la",
		"error: file not found",
		"Build succeeded",
		"PASS ok foci/tools 0.004s", // "0.004s" gets stripped but that's fine
		"func TestFoo(t *testing.T)",
	}
	for _, input := range tests {
		got := normalizePaneContent(input)
		// Should not be empty (meaningful content preserved)
		if strings.TrimSpace(got) == "" {
			t.Errorf("normalizePaneContent(%q) = %q, should preserve content", input, got)
		}
	}
}

func TestNormalizePaneContent_MixedLine(t *testing.T) {
	// A realistic TUI status bar line
	input := "⠙ Thinking  Claude 3.5 | 44% context | 12,543 tokens | 2m 30s | $0.0430"
	got := normalizePaneContent(input)
	// Only clocks/timers should be stripped; spinners, tokens, percentages, costs preserved
	if !strings.Contains(got, "44%") {
		t.Errorf("percentage should be preserved (indicates activity): %q", got)
	}
	if !strings.Contains(got, "12,543 tokens") {
		t.Errorf("token count should be preserved (indicates activity): %q", got)
	}
	if strings.Contains(got, "2m 30s") {
		t.Errorf("elapsed timer should be stripped: %q", got)
	}
	if !strings.Contains(got, "$0.0430") {
		t.Errorf("cost should be preserved (indicates activity): %q", got)
	}
}

func TestNormalizePaneContent_StableHash(t *testing.T) {
	// Two snapshots that differ only in clocks/timers should normalize
	// to identical strings. Spinners/tokens/percentages are NOT noise.
	snap1 := `$ opencode
OpenCode v0.1 | claude-3-5-sonnet
Thinking... | 1m 3s
> How do I fix the bug?`

	snap2 := `$ opencode
OpenCode v0.1 | claude-3-5-sonnet
Thinking... | 2m 54s
> How do I fix the bug?`

	norm1 := normalizePaneContent(snap1)
	norm2 := normalizePaneContent(snap2)

	if norm1 != norm2 {
		t.Errorf("snapshots differing only in timers should normalize equally:\n  snap1: %q\n  snap2: %q", norm1, norm2)
	}
}

func TestNormalizePaneContent_DifferentContent(t *testing.T) {
	// Two snapshots with genuinely different content should NOT normalize
	// to the same string.
	snap1 := `$ opencode
⠋ Thinking... | 44% context
> How do I fix the bug?`

	snap2 := `$ opencode
Here's the fix for the bug:
  change line 42 to use foo() instead of bar()`

	norm1 := normalizePaneContent(snap1)
	norm2 := normalizePaneContent(snap2)

	if norm1 == norm2 {
		t.Error("snapshots with different content should NOT normalize equally")
	}
}

// --- TUI detection and cleanup ---

func TestDetectTUIAgent_CC(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"Claude Code marker", "some output\nClaude Code v1.2.3\nprompt here"},
		{"bypass marker", "⏵⏵ bypass\nsome command output"},
		{"Cooked for", "Cooked for 3.2s\nresult here"},
		{"Crunched for", "Crunched for 1.5s\nresult here"},
		{"Baked for", "Baked for 0.8s\nresult here"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectTUIAgent(tt.content)
			if got != "cc" {
				t.Errorf("detectTUIAgent() = %q, want %q", got, "cc")
			}
		})
	}
}

func TestDetectTUIAgent_OC(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"OpenCode marker", "OpenCode v0.1\nclaude-3-5-sonnet\nprompt"},
		{"GLM marker", "GLM\nsome output here"},
		{"Build marker", "Build\nrunning tests"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectTUIAgent(tt.content)
			if got != "oc" {
				t.Errorf("detectTUIAgent() = %q, want %q", got, "oc")
			}
		})
	}
}

func TestDetectTUIAgent_None(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"plain shell", "$ ls -la\ntotal 42\ndrwxr-xr-x 5 user user 4096 file.go"},
		{"empty", ""},
		{"command output", "go test ./...\nPASS\nok  foci/tools 0.004s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectTUIAgent(tt.content)
			if got != "" {
				t.Errorf("detectTUIAgent() = %q, want empty", got)
			}
		})
	}
}

func TestCleanTUIOutput_CC(t *testing.T) {
	input := strings.Join([]string{
		"Claude Code v1.2.3",
		"╭──────────────────────────╮",
		"│ Here is my response      │",
		"│ with important content   │",
		"╰──────────────────────────╯",
		"─────────────────────────",
		"✻",
		"▟█▙",
		"⏵⏵ bypass",
		"shift+tab to accept",
		"actual content line",
		"",
		"",
		"",
		"",
		"another content line",
	}, "\n")

	got := cleanTUIOutput(input, "cc")

	// Should preserve meaningful content
	if !strings.Contains(got, "Here is my response") {
		t.Errorf("should preserve content line, got:\n%s", got)
	}
	if !strings.Contains(got, "with important content") {
		t.Errorf("should preserve content line, got:\n%s", got)
	}
	if !strings.Contains(got, "actual content line") {
		t.Errorf("should preserve actual content, got:\n%s", got)
	}
	if !strings.Contains(got, "another content line") {
		t.Errorf("should preserve another content line, got:\n%s", got)
	}

	// Should strip chrome
	if strings.Contains(got, "Claude Code v1.2.3") {
		t.Errorf("should strip version line, got:\n%s", got)
	}
	if strings.Contains(got, "╭") || strings.Contains(got, "╰") {
		t.Errorf("should strip box-drawing lines, got:\n%s", got)
	}
	if strings.Contains(got, "─────") {
		t.Errorf("should strip horizontal rules, got:\n%s", got)
	}
	if strings.Contains(got, "✻") {
		t.Errorf("should strip decorative symbols, got:\n%s", got)
	}
	if strings.Contains(got, "▟█▙") {
		t.Errorf("should strip logo blocks, got:\n%s", got)
	}
	if strings.Contains(got, "⏵⏵ bypass") {
		t.Errorf("should strip mode indicator, got:\n%s", got)
	}
	if strings.Contains(got, "shift+tab") {
		t.Errorf("should strip status hints, got:\n%s", got)
	}

	// Should collapse consecutive blank lines
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("should collapse consecutive blank lines, got:\n%s", got)
	}
}

func TestCleanTUIOutput_OC(t *testing.T) {
	input := strings.Join([]string{
		"OpenCode v0.1",
		"┃",
		"━━━━━━━━━━━━━━━━━━━━━━━━━",
		"MCP│server status",
		"LSP│go initialized",
		"Build│running...",
		"Here is the actual response",
		"with multiple lines",
		"Modified Files",
		"3 files changed",
		"ctrl+a to select all",
		"╹",
	}, "\n")

	got := cleanTUIOutput(input, "oc")

	// Should preserve meaningful content
	if !strings.Contains(got, "Here is the actual response") {
		t.Errorf("should preserve content, got:\n%s", got)
	}
	if !strings.Contains(got, "with multiple lines") {
		t.Errorf("should preserve content, got:\n%s", got)
	}

	// Should strip chrome
	if strings.Contains(got, "OpenCode v0.1") {
		t.Errorf("should strip version line, got:\n%s", got)
	}
	if strings.Contains(got, "━━━") {
		t.Errorf("should strip box-drawing, got:\n%s", got)
	}
	if strings.Contains(got, "MCP│") {
		t.Errorf("should strip MCP sidebar, got:\n%s", got)
	}
	if strings.Contains(got, "LSP│") {
		t.Errorf("should strip LSP sidebar, got:\n%s", got)
	}
	if strings.Contains(got, "Build│") {
		t.Errorf("should strip build line, got:\n%s", got)
	}
	if strings.Contains(got, "Modified Files") {
		t.Errorf("should strip section header, got:\n%s", got)
	}
	if strings.Contains(got, "3 files changed") {
		t.Errorf("should strip diff summary, got:\n%s", got)
	}
	if strings.Contains(got, "ctrl+a") {
		t.Errorf("should strip status hints, got:\n%s", got)
	}
}

func TestCleanTUIOutput_NoAgent(t *testing.T) {
	input := "$ ls -la\ntotal 42\ndrwxr-xr-x 5 user user 4096 file.go"
	got := cleanTUIOutput(input, "")
	if got != input {
		t.Errorf("empty agent type should return content unchanged\ngot:  %q\nwant: %q", got, input)
	}
}

func TestTmuxReadRaw(t *testing.T) {
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)


	name := "foci-test-readraw"
	tmuxSetup(t, name)

	// Start a session that echoes CC-like content
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Send some text that would trigger CC detection
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      name,
		"keys":      "echo Claude Code v1.0",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("send: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Read with raw=true — should contain the marker
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      name,
		"raw":       true,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if !strings.Contains(result.Text,"Claude Code") {
		t.Errorf("raw read should preserve all content, got:\n%s", result.Text)
	}

	// Read with raw=false (default) — CC version line should be stripped
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      name,
	})
	_, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("read cleaned: %v", err)
	}
	// The "Claude Code v1.0" in `echo` output will be detected and the
	// version-line pattern may strip lines matching "Claude Code ...".
	// The echo command itself (containing "Claude Code") triggers detection,
	// and the output line "Claude Code v1.0" matches the version-line regex.
	// We just verify it doesn't error — exact content depends on shell prompt.
}

// --- State persistence ---

func TestTmuxPersistOwnedSessions(t *testing.T) {
	tmuxAvailable(t)

	// Create a temp file for state persistence
	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	_, tool, _ := NewTmuxTool(300, 30, nil, store, "tmux:test-agent", false, 30, 0)

	name := "foci-test-persist"
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

	// Verify state was persisted (new format: map[string]string)
	var owned map[string]string
	if !store.Get("tmux:test-agent", &owned) {
		t.Fatal("owned sessions not persisted")
	}
	if len(owned) != 1 {
		t.Errorf("persisted sessions = %v, want 1 entry", owned)
	}
	if _, ok := owned[name]; !ok {
		t.Errorf("persisted sessions = %v, want key %s", owned, name)
	}
}

func TestTmuxRestoreOwnedSessions(t *testing.T) {
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)

	// Pre-populate state with an owned session
	if err := store.Set("tmux:test-agent", map[string]string{"foci-test-restore": ""}); err != nil {
		t.Fatalf("set state: %v", err)
	}

	// Create the tmux session (simulating it still exists from before restart)
	tmuxSetup(t, "foci-test-restore")
	exec.Command("tmux", "-S", tmuxSocketPath, "new-session", "-d", "-s", "foci-test-restore", "sleep", "60").Run()

	// Load state
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	// Create tool with state store - should restore owned sessions
	_, tool, _ := NewTmuxTool(300, 30, nil, store, "tmux:test-agent", false, 30, 0)

	// Read should succeed because the session is in the restored owned set
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      "foci-test-restore",
	})
	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Errorf("read on restored session should succeed, got: %v", err)
	}
}

func TestTmuxPersistOnKill(t *testing.T) {
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	_, tool, _ := NewTmuxTool(300, 30, nil, store, "tmux:test-agent", false, 30, 0)

	name := "foci-test-persistkill"
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

	// Verify persisted
	var owned map[string]string
	if !store.Get("tmux:test-agent", &owned) {
		t.Fatal("owned sessions not persisted after start")
	}
	if len(owned) != 1 {
		t.Errorf("persisted sessions = %v, want 1 session", owned)
	}

	// Kill the session
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "kill",
		"name":      name,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("kill: %v", err)
	}

	// Verify removed from persisted state
	var ownedAfter map[string]string
	if !store.Get("tmux:test-agent", &ownedAfter) {
		t.Fatal("owned sessions key should still exist")
	}
	if len(ownedAfter) != 0 {
		t.Errorf("persisted sessions after kill = %v, want empty", ownedAfter)
	}
}

func TestTmuxPersistClearedOnStaleSessions(t *testing.T) {
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)

	// Pre-populate state with sessions that no longer exist
	if err := store.Set("tmux:test-agent", map[string]string{"foci-test-stale1": "", "foci-test-stale2": ""}); err != nil {
		t.Fatalf("set state: %v", err)
	}

	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	_, tool, _ := NewTmuxTool(300, 30, nil, store, "tmux:test-agent", false, 30, 0)

	// List should detect stale sessions and clear persisted state
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "list",
	})
	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// The stale sessions should not appear as "owned" in the output
	// (the main check is that persisted state is cleared, below)

	// Verify persisted state was cleared
	var owned map[string]string
	if !store.Get("tmux:test-agent", &owned) {
		t.Fatal("owned sessions key should still exist")
	}
	if len(owned) != 0 {
		t.Errorf("persisted sessions after list = %v, want empty", owned)
	}
}

func TestTmuxNoStateStore(t *testing.T) {
	tmuxAvailable(t)

	// Create tool without state store (nil)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)


	name := "foci-test-nostate"
	tmuxSetup(t, name)

	// Start should still work
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// List should work
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "list",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(result.Text,name) {
		t.Errorf("list result = %q, want to contain %s", result.Text, name)
	}
}

func TestTmuxStateFileRoundTrip(t *testing.T) {
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")

	// First instance: start session and persist
	store1 := state.New(stateFile)
	if err := store1.Load(); err != nil {
		t.Fatalf("load state1: %v", err)
	}

	_, tool1, _ := NewTmuxTool(300, 30, nil, store1, "tmux:test-agent", false, 30, 0)

	name := "foci-test-roundtrip"
	tmuxSetup(t, name)

	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
	})
	if _, err := tool1.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Read the persisted file directly to verify it was written
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if !strings.Contains(string(data), "tmux:test-agent") {
		t.Errorf("state file does not contain key 'tmux:test-agent': %s", string(data))
	}
	if !strings.Contains(string(data), name) {
		t.Errorf("state file does not contain session name %q: %s", name, string(data))
	}

	// Second instance: reload state and verify session is accessible
	store2 := state.New(stateFile)
	if err := store2.Load(); err != nil {
		t.Fatalf("load state2: %v", err)
	}

	_, tool2, _ := NewTmuxTool(300, 30, nil, store2, "tmux:test-agent", false, 30, 0)

	// Read should work because session was restored from state
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      name,
	})
	_, err = tool2.Execute(context.Background(), params)
	if err != nil {
		t.Errorf("read on restored session should succeed, got: %v", err)
	}
}

func TestTmuxPersistWatches(t *testing.T) {
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	notifier := NewAsyncNotifier(func(sk, msg string, replyTo string) {})
	_, tool, _ := NewTmuxTool(300, 30, notifier, store, "tmux:test-agent", false, 30, 0)

	name := "foci-test-persist-watch"
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
		"threshold_seconds": 45,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("watch: %v", err)
	}

	// Verify watches were persisted
	var watches []persistedWatch
	if !store.Get("tmux:test-agent:watches", &watches) {
		t.Fatal("watches not persisted")
	}
	if len(watches) != 1 {
		t.Fatalf("persisted watches = %d, want 1", len(watches))
	}
	if watches[0].Session != name {
		t.Errorf("persisted watch session = %q, want %q", watches[0].Session, name)
	}
	if watches[0].ThresholdSecs != 45 {
		t.Errorf("persisted watch threshold = %d, want 45", watches[0].ThresholdSecs)
	}

	// Cleanup
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	tool.Execute(context.Background(), params)
}

func TestTmuxRestoreWatches(t *testing.T) {
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)

	name := "foci-test-restore-watch"
	tmuxSetup(t, name)

	// Create the tmux session (simulating it still exists from before restart)
	exec.Command("tmux", "-S", tmuxSocketPath, "new-session", "-d", "-s", name, "sleep", "60").Run()

	// Pre-populate state with owned session and watch
	if err := store.Set("tmux:test-agent", map[string]string{name: ""}); err != nil {
		t.Fatalf("set owned state: %v", err)
	}
	if err := store.Set("tmux:test-agent:watches", []persistedWatch{
		{Session: name, Window: 0, ThresholdSecs: 30, AgentSessionKey: "test-session"},
	}); err != nil {
		t.Fatalf("set watch state: %v", err)
	}

	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	notifier := NewAsyncNotifier(func(sk, msg string, replyTo string) {})
	_, _, cleanup := NewTmuxTool(300, 30, notifier, store, "tmux:test-agent", false, 30, 0)

	// Verify the watch was restored by checking the state is still persisted
	// (if the session was alive, it stays in the map; if stale, it gets cleaned)
	var watches []persistedWatch
	if !store.Get("tmux:test-agent:watches", &watches) {
		t.Fatal("watches should still be in state")
	}
	if len(watches) != 1 {
		t.Fatalf("restored watches = %d, want 1", len(watches))
	}
	if watches[0].Session != name {
		t.Errorf("restored watch session = %q, want %q", watches[0].Session, name)
	}

	// Cleanup watches via ClearAll
	cleanup()
}

func TestTmuxRestoreWatchesStaleSessions(t *testing.T) {
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)

	// Pre-populate with a watch for a non-existent session
	if err := store.Set("tmux:test-agent:watches", []persistedWatch{
		{Session: "foci-test-stale-watch-xyz", Window: 0, ThresholdSecs: 30, AgentSessionKey: "test-session"},
	}); err != nil {
		t.Fatalf("set watch state: %v", err)
	}

	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	notifier := NewAsyncNotifier(func(sk, msg string, replyTo string) {})
	NewTmuxTool(300, 30, notifier, store, "tmux:test-agent", false, 30, 0)

	// Stale watch should have been cleaned from state
	var watches []persistedWatch
	if !store.Get("tmux:test-agent:watches", &watches) {
		t.Fatal("watches key should still exist")
	}
	if len(watches) != 0 {
		t.Errorf("persisted watches after stale cleanup = %d, want 0", len(watches))
	}
}

func TestTmuxUnwatchPersists(t *testing.T) {
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	notifier := NewAsyncNotifier(func(sk, msg string, replyTo string) {})
	_, tool, _ := NewTmuxTool(300, 30, notifier, store, "tmux:test-agent", false, 30, 0)

	name := "foci-test-unwatch-persist"
	tmuxSetup(t, name)

	// Start — watch=false to control watch params below
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

	params, _ = json.Marshal(map[string]interface{}{
		"operation":         "watch",
		"name":              name,
		"threshold_seconds": 30,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("watch: %v", err)
	}

	// Verify watch is persisted
	var watches []persistedWatch
	if !store.Get("tmux:test-agent:watches", &watches) || len(watches) != 1 {
		t.Fatal("watch should be persisted")
	}

	// Unwatch
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("unwatch: %v", err)
	}

	// Verify watches state is now empty
	if !store.Get("tmux:test-agent:watches", &watches) {
		t.Fatal("watches key should still exist")
	}
	if len(watches) != 0 {
		t.Errorf("persisted watches after unwatch = %d, want 0", len(watches))
	}
}

func TestTmuxClearAllPersistsWatches(t *testing.T) {
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	notifier := NewAsyncNotifier(func(sk, msg string, replyTo string) {})
	_, tool, cleanup := NewTmuxTool(300, 30, notifier, store, "tmux:test-agent", false, 30, 0)

	name := "foci-test-clearall-watch"
	tmuxSetup(t, name)

	// Start — watch=false to control watch params below
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

	params, _ = json.Marshal(map[string]interface{}{
		"operation":         "watch",
		"name":              name,
		"threshold_seconds": 30,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("watch: %v", err)
	}

	// Verify watch is persisted
	var watches []persistedWatch
	if !store.Get("tmux:test-agent:watches", &watches) || len(watches) != 1 {
		t.Fatal("watch should be persisted before ClearAll")
	}

	// ClearAll
	cleanup()

	// Verify watches state is cleared
	if !store.Get("tmux:test-agent:watches", &watches) {
		t.Fatal("watches key should still exist")
	}
	if len(watches) != 0 {
		t.Errorf("persisted watches after ClearAll = %d, want 0", len(watches))
	}
}

func TestTmuxUnwatchNotRestoredOnRestart(t *testing.T) {
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	notifier := NewAsyncNotifier(func(sk, msg string, replyTo string) {})
	_, tool1, cleanup1 := NewTmuxTool(300, 30, notifier, store, "tmux:test-agent", false, 30, 0)
	defer cleanup1()

	name := "foci-test-unwatch-restart"
	tmuxSetup(t, name)

	// Start session — watch=false to control watch params below
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
		"watch":     false,
	})
	if _, err := tool1.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Watch the session
	params, _ = json.Marshal(map[string]interface{}{
		"operation":         "watch",
		"name":              name,
		"threshold_seconds": 30,
	})
	if _, err := tool1.Execute(context.Background(), params); err != nil {
		t.Fatalf("watch: %v", err)
	}

	// Verify watch is persisted
	var watches []persistedWatch
	if !store.Get("tmux:test-agent:watches", &watches) || len(watches) != 1 {
		t.Fatal("watch should be persisted")
	}

	// Unwatch
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	if _, err := tool1.Execute(context.Background(), params); err != nil {
		t.Fatalf("unwatch: %v", err)
	}

	// Simulate restart: create a new tool instance from the same state store
	// (reload state from disk to mimic fresh process start)
	store2 := state.New(stateFile)
	if err := store2.Load(); err != nil {
		t.Fatalf("reload state: %v", err)
	}

	_, tool2, cleanup2 := NewTmuxTool(300, 30, notifier, store2, "tmux:test-agent", false, 30, 0)
	defer cleanup2()

	// The unwatched session should NOT be restored — verify by trying to unwatch
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	_, err := tool2.Execute(context.Background(), params)
	if err == nil {
		t.Error("expected error unwatching session that should not have been restored")
	}

	// Verify no watches in persisted state
	var watches2 []persistedWatch
	if store2.Get("tmux:test-agent:watches", &watches2) && len(watches2) != 0 {
		t.Errorf("watches in state after restart = %d, want 0", len(watches2))
	}
}

func TestTmuxStartAutoWatch(t *testing.T) {
	tmuxAvailable(t)

	notifier := NewAsyncNotifier(func(sk, msg string, replyTo string) {})
	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	_, tool, _ := NewTmuxTool(300, 30, notifier, store, "tmux:test-autowatch", false, 30, 0)

	name := "foci-test-autowatch"
	tmuxSetup(t, name)

	// Start with default watch=true (omitted from params)
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(result.Text,"Session started") {
		t.Errorf("result missing 'Session started': %q", result.Text)
	}
	if !strings.Contains(result.Text,"Watching") {
		t.Errorf("result missing watch confirmation: %q", result.Text)
	}

	// Verify watch was persisted
	var watches []persistedWatch
	if !store.Get("tmux:test-autowatch:watches", &watches) {
		t.Fatal("watches not persisted after auto-watch")
	}
	if len(watches) != 1 {
		t.Fatalf("persisted watches = %d, want 1", len(watches))
	}
	if watches[0].Session != name {
		t.Errorf("watch session = %q, want %q", watches[0].Session, name)
	}
	if watches[0].ThresholdSecs != 30 {
		t.Errorf("watch threshold = %d, want 30 (default)", watches[0].ThresholdSecs)
	}

	// Cleanup watch
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	tool.Execute(context.Background(), params)
}

func TestTmuxStartWatchFalse(t *testing.T) {
	tmuxAvailable(t)

	notifier := NewAsyncNotifier(func(sk, msg string, replyTo string) {})
	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	_, tool, _ := NewTmuxTool(300, 30, notifier, store, "tmux:test-nowatch", false, 30, 0)

	name := "foci-test-nowatch"
	tmuxSetup(t, name)

	// Start with watch=false
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
		"watch":     false,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(result.Text,"Session started") {
		t.Errorf("result missing 'Session started': %q", result.Text)
	}
	if strings.Contains(result.Text,"Watching") {
		t.Errorf("result should NOT contain watch confirmation when watch=false: %q", result.Text)
	}

	// Verify no watches persisted
	var watches []persistedWatch
	if store.Get("tmux:test-nowatch:watches", &watches) && len(watches) != 0 {
		t.Errorf("persisted watches = %d, want 0 when watch=false", len(watches))
	}
}

func TestTmuxStartAutoWatchNoNotifier(t *testing.T) {
	tmuxAvailable(t)

	// No notifier — auto-watch should be silently skipped
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

	name := "foci-test-autowatch-nonotif"
	tmuxSetup(t, name)

	// Start with default watch=true but no notifier
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(result.Text,"Session started") {
		t.Errorf("result missing 'Session started': %q", result.Text)
	}
	// Should not contain watch info since no notifier
	if strings.Contains(result.Text,"Watching") {
		t.Errorf("result should NOT contain watch when no notifier: %q", result.Text)
	}
}

func TestTmuxAutopilotAutoUnwatch(t *testing.T) {
	t.Parallel()
	tmuxAvailable(t)

	var mu sync.Mutex
	var notifications []string
	notifier := NewAsyncNotifier(func(sk, msg string, replyTo string) {
		mu.Lock()
		notifications = append(notifications, msg)
		mu.Unlock()
	})

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	// autopilot=true, threshold=2s for fast test
	_, tool, _ := NewTmuxTool(300, 30, notifier, store, "tmux:test-autopilot-unwatch", true, 2, 0)

	name := "foci-test-ap-unwatch"
	tmuxSetup(t, name)

	// Start session (auto-watches with 2s threshold due to autopilot)
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(result.Text,"Watching") {
		t.Fatalf("expected auto-watch on start: %q", result.Text)
	}

	// Wait for inactivity notification (threshold=2s, monitor polls every 2s)
	time.Sleep(3500 * time.Millisecond)

	mu.Lock()
	gotNotification := len(notifications) > 0
	mu.Unlock()

	if !gotNotification {
		t.Fatal("expected inactivity notification")
	}

	// Verify watch was auto-removed (autopilot)
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "list",
	})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(result.Text,"watched") {
		t.Errorf("expected watch to be auto-removed after inactivity (autopilot), got: %q", result.Text)
	}
}

func TestTmuxAutopilotAutoWatchOnSend(t *testing.T) {
	tmuxAvailable(t)

	notifier := NewAsyncNotifier(func(sk, msg string, replyTo string) {})
	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	// autopilot=true
	_, tool, _ := NewTmuxTool(300, 30, notifier, store, "tmux:test-autopilot-send", true, 30, 0)

	name := "foci-test-ap-send"
	tmuxSetup(t, name)

	// Start with watch=false so session is unwatched
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "cat",
		"watch":     false,
	})
	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Send keys — should auto-watch
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      name,
		"keys":      "hello",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(result.Text,"Watching") {
		t.Errorf("expected auto-watch on send (autopilot), got: %q", result.Text)
	}

	// Second send should NOT add another watch
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      name,
		"keys":      "world",
	})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if strings.Contains(result.Text,"Watching") {
		t.Errorf("should not re-watch already watched session, got: %q", result.Text)
	}

	// Cleanup
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	tool.Execute(context.Background(), params)
}

func TestTmuxAutopilotDisabled(t *testing.T) {
	tmuxAvailable(t)

	notifier := NewAsyncNotifier(func(sk, msg string, replyTo string) {})
	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	// autopilot=false
	_, tool, _ := NewTmuxTool(300, 30, notifier, store, "tmux:test-no-autopilot", false, 30, 0)

	name := "foci-test-no-ap"
	tmuxSetup(t, name)

	// Start with watch=false
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "cat",
		"watch":     false,
	})
	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Send keys — should NOT auto-watch when autopilot=false
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      name,
		"keys":      "hello",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if strings.Contains(result.Text,"Watching") {
		t.Errorf("should not auto-watch when autopilot=false, got: %q", result.Text)
	}
}

func TestTmuxKillCleansUpChildProcesses(t *testing.T) {
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

	name := "foci-test-killproc"
	tmuxSetup(t, name)

	// Start a session that spawns a child process
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 300",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Give the process a moment to start
	time.Sleep(200 * time.Millisecond)

	// Get the pane PID before killing
	pids := tmuxSessionPIDs(name)
	if len(pids) == 0 {
		t.Fatal("no pane PIDs found before kill")
	}

	// Collect descendants (the sleep process is a child of the shell)
	children := collectDescendants(pids)
	allPIDs := append(pids, children...)

	// Kill the session via the tool
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "kill",
		"name":      name,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("kill: %v", err)
	}
	if !strings.Contains(result.Text, name) {
		t.Errorf("kill result = %q, want session name", result.Text)
	}

	// Wait for processes to actually die
	time.Sleep(500 * time.Millisecond)

	// Verify all processes are gone
	for _, pid := range allPIDs {
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue // process doesn't exist, good
		}
		if err := proc.Signal(syscall.Signal(0)); err == nil {
			t.Errorf("process %d still alive after kill", pid)
		}
	}
}

func TestCollectDescendants(t *testing.T) {
	// Spawn a process with a known child
	cmd := exec.Command("bash", "-c", "sleep 300 & echo $!; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		cmd.Process.Signal(syscall.SIGTERM)
		cmd.Wait()
	}()

	// Give it a moment to spawn the child
	time.Sleep(200 * time.Millisecond)

	parentPID := cmd.Process.Pid
	descendants := collectDescendants([]int{parentPID})

	if len(descendants) == 0 {
		t.Error("expected at least 1 descendant (the sleep process)")
	}

	// Verify descendants are real PIDs
	for _, pid := range descendants {
		if pid <= 1 {
			t.Errorf("invalid descendant PID: %d", pid)
		}
	}

	// Clean up
	for _, pid := range descendants {
		if proc, err := os.FindProcess(pid); err == nil {
			proc.Signal(syscall.SIGKILL)
		}
	}
}

func TestTerminateProcesses(t *testing.T) {
	// Spawn a process that ignores SIGHUP and SIGTERM (like OpenCode does).
	// terminateProcesses should escalate to SIGKILL.
	cmd := exec.Command("bash", "-c", "trap '' HUP TERM; sleep 300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid

	// Give trap time to install
	time.Sleep(100 * time.Millisecond)

	// Verify process is alive
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("process not alive after start: %v", err)
	}

	// terminateProcesses should SIGTERM, wait, then SIGKILL
	killed := terminateProcesses([]int{pid})
	if killed == 0 {
		t.Error("expected terminateProcesses to signal at least 1 process")
	}

	// Wait for SIGKILL to take effect
	cmd.Wait()

	// Verify dead (Signal(0) should fail)
	if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
		t.Errorf("process %d still alive after terminateProcesses", pid)
		cmd.Process.Kill()
	}
}

func TestMaybeKillTmuxServer_WithSessions(t *testing.T) {
	tmuxAvailable(t)

	name := "foci-test-maybekill"
	tmuxSetup(t, name)

	// Start a session so the server has at least one.
	_, err := runTmux(context.Background(), "new-session", "-d", "-s", name, "sleep 300")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// maybeKillTmuxServer should NOT kill because sessions exist.
	if maybeKillTmuxServer(context.Background()) {
		t.Error("maybeKillTmuxServer killed server while sessions exist")
	}

	// Verify the session is still there.
	out, err := runTmux(context.Background(), "list-sessions", "-F", "#{session_name}")
	if err != nil {
		t.Fatalf("list-sessions after maybeKillTmuxServer: %v", err)
	}
	if !strings.Contains(out, name) {
		t.Errorf("session %q disappeared after maybeKillTmuxServer", name)
	}
}

func TestMaybeKillTmuxServer_NoSessions(t *testing.T) {
	tmuxAvailable(t)

	// Start a session and immediately kill it so the server has no sessions.
	name := "foci-test-maybekill-empty"
	_, err := runTmux(context.Background(), "new-session", "-d", "-s", name, "sleep 1")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	_, err = runTmux(context.Background(), "kill-session", "-t", name)
	if err != nil {
		t.Fatalf("kill session: %v", err)
	}

	// Server may have exited already (exit-empty on), or it may linger.
	// maybeKillTmuxServer should handle both cases gracefully.
	maybeKillTmuxServer(context.Background())

	// After this, the server should not be running. Verify by listing.
	out, err := runTmux(context.Background(), "list-sessions", "-F", "#{session_name}")
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if strings.TrimSpace(line) != "" {
				t.Errorf("unexpected session %q after server cleanup", line)
			}
		}
	}
	// err != nil is expected ("no server running") — that's the success case.
}

func TestTmuxKillCleansUpServer(t *testing.T) {
	tmuxAvailable(t)

	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)
	name := "foci-test-killserver"
	tmuxSetup(t, name)

	// Start a single session
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Kill it — should also clean up the server
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "kill",
		"name":      name,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("kill: %v", err)
	}

	// Give a moment for cleanup
	time.Sleep(100 * time.Millisecond)

	// Verify no tmux server is running
	out, err := runTmux(context.Background(), "list-sessions", "-F", "#{session_name}")
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if strings.TrimSpace(line) != "" {
				t.Errorf("session %q still exists after kill (server should be gone)", line)
			}
		}
	}
	// err != nil ("no server running") is the expected success case.
}

func TestTmuxSessionPIDs(t *testing.T) {
	tmuxAvailable(t)

	name := "foci-test-pids"
	tmuxSetup(t, name)

	// Create a session
	_, err := runTmux(context.Background(), "new-session", "-d", "-s", name, "sleep 300")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	pids := tmuxSessionPIDs(name)
	if len(pids) == 0 {
		t.Error("expected at least 1 pane PID")
	}
	for _, pid := range pids {
		if pid <= 1 {
			t.Errorf("invalid pane PID: %d", pid)
		}
		// Verify PID exists
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); os.IsNotExist(err) {
			t.Errorf("pane PID %d does not exist in /proc", pid)
		}
	}
}

func TestTmuxSendRateLimit(t *testing.T) {
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

	name := "foci-test-ratelimit"
	tmuxSetup(t, name)

	// Start a session
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "cat",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// First send should be fast
	sendParams, _ := json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      name,
		"keys":      "first",
		"enter":     false,
	})
	t0 := time.Now()
	if _, err := tool.Execute(context.Background(), sendParams); err != nil {
		t.Fatalf("first send: %v", err)
	}
	d1 := time.Since(t0)

	// Second send should be delayed ~2s by rate limiter
	sendParams, _ = json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      name,
		"keys":      "second",
		"enter":     false,
	})
	t1 := time.Now()
	if _, err := tool.Execute(context.Background(), sendParams); err != nil {
		t.Fatalf("second send: %v", err)
	}
	d2 := time.Since(t1)

	if d1 > 1*time.Second {
		t.Errorf("first send took %v, expected < 1s", d1)
	}
	if d2 < 1500*time.Millisecond {
		t.Errorf("second send took %v, expected >= 1.5s (rate limited)", d2)
	}
}

func TestTmuxSessionKeyIsolation(t *testing.T) {
	tmuxAvailable(t)

	// Single tool instance, two different session keys
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

	nameA := "foci-test-skiso-a"
	nameB := "foci-test-skiso-b"
	tmuxSetup(t, nameA, nameB)

	ctxA := WithSessionKey(context.Background(), "agent:test:chat:111")
	ctxB := WithSessionKey(context.Background(), "agent:test:chat:222")

	// Session A starts
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      nameA,
		"command":   "sleep 60",
	})
	if _, err := tool.Execute(ctxA, params); err != nil {
		t.Fatalf("session A start: %v", err)
	}

	// Session B starts
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      nameB,
		"command":   "sleep 60",
	})
	if _, err := tool.Execute(ctxB, params); err != nil {
		t.Fatalf("session B start: %v", err)
	}

	// Session B cannot read session A's tmux session
	readParams, _ := json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      nameA,
	})
	_, err := tool.Execute(ctxB, readParams)
	if err == nil {
		t.Fatal("session B should not be able to read session A's tmux session")
	}
	if !strings.Contains(err.Error(), "not owned") {
		t.Errorf("error = %q, want 'not owned'", err.Error())
	}

	// Session A can read its own tmux session
	_, err = tool.Execute(ctxA, readParams)
	if err != nil {
		t.Fatalf("session A should be able to read its own session: %v", err)
	}

	// Session B cannot send to session A's tmux session
	sendParams, _ := json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      nameA,
		"keys":      "hello",
	})
	_, err = tool.Execute(ctxB, sendParams)
	if err == nil {
		t.Fatal("session B should not be able to send to session A's session")
	}

	// Session B cannot kill session A's tmux session
	killParams, _ := json.Marshal(map[string]interface{}{
		"operation": "kill",
		"name":      nameA,
	})
	_, err = tool.Execute(ctxB, killParams)
	if err == nil {
		t.Fatal("session B should not be able to kill session A's session")
	}

	// List from session A: should show owner "test" for its own, "-" for B's
	listParams, _ := json.Marshal(map[string]interface{}{
		"operation": "list",
	})
	result, err := tool.Execute(ctxA, listParams)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, line := range strings.Split(result.Text, "\n") {
		if strings.Contains(line, nameA) && !strings.Contains(line, "test") {
			t.Errorf("session A should show %q with owner 'test': %q", nameA, line)
		}
	}
}

// TestTmuxReapExpiredSessions tests the reaper logic directly.
func TestTmuxReapExpiredSessions(t *testing.T) {
	tmuxAvailable(t)

	name := "foci-test-reap"
	tmuxSetup(t, name)

	inst := &tmuxInstance{
		watched:           make(map[string]*watchedSession),
		owned:             make(map[string]string),
		lastSend:          make(map[string]time.Time),
		lastAccess:        make(map[string]time.Time),
		sessionTTL:        100 * time.Millisecond,
	}

	// Create a real tmux session
	_, err := runTmux(context.Background(), "new-session", "-d", "-s", name, "sleep", "60")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Register it as owned with an old lastAccess time
	inst.owned[name] = ""
	inst.lastAccess[name] = time.Now().Add(-1 * time.Second) // well past TTL

	// Reap
	inst.reapExpiredSessions()

	// Verify session was removed from owned
	if _, ok := inst.owned[name]; ok {
		t.Error("session should have been removed from owned map")
	}
	if _, ok := inst.lastAccess[name]; ok {
		t.Error("session should have been removed from lastAccess map")
	}

	// Verify tmux session was killed
	_, err = runTmux(context.Background(), "has-session", "-t", name)
	if err == nil {
		t.Error("tmux session should have been killed by reaper")
	}
}

// TestTmuxReapPreservesActiveSession verifies the reaper doesn't kill
// sessions that have been accessed recently.
func TestTmuxReapPreservesActiveSession(t *testing.T) {
	tmuxAvailable(t)

	name := "foci-test-reap-active"
	tmuxSetup(t, name)

	inst := &tmuxInstance{
		watched:           make(map[string]*watchedSession),
		owned:             make(map[string]string),
		lastSend:          make(map[string]time.Time),
		lastAccess:        make(map[string]time.Time),
		sessionTTL:        1 * time.Hour,
	}

	// Create a real tmux session
	_, err := runTmux(context.Background(), "new-session", "-d", "-s", name, "sleep", "60")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Register with recent access
	inst.owned[name] = ""
	inst.lastAccess[name] = time.Now()

	// Reap — should not kill
	inst.reapExpiredSessions()

	// Verify session is still owned
	if _, ok := inst.owned[name]; !ok {
		t.Error("active session should not have been reaped")
	}

	// Verify tmux session still exists
	_, err = runTmux(context.Background(), "has-session", "-t", name)
	if err != nil {
		t.Error("active tmux session should still exist after reap")
	}
}
