package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/state"
)

func tmuxAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
}

func tmuxCleanup(t *testing.T, name string) {
	t.Helper()
	exec.Command("tmux", "kill-session", "-t", name).Run()
}

func TestTmuxStartAndList(t *testing.T) {
	tmuxAvailable(t)
	tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)

	name := "foci-test-start"
	defer tmuxCleanup(t, name)

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
	if !strings.Contains(result, name) {
		t.Errorf("start result = %q, want session name", result)
	}

	// List
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "list",
	})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(result, name) {
		t.Errorf("list result = %q, want %q", result, name)
	}
	// Should have header line
	if !strings.Contains(result, "SESSION") {
		t.Errorf("list result missing header: %q", result)
	}
	// Owned session should show "owned" status
	if !strings.Contains(result, "owned") {
		t.Errorf("list result missing 'owned' status: %q", result)
	}
}

func TestTmuxSendAndRead(t *testing.T) {
	tmuxAvailable(t)
	tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)


	name := "foci-test-sendread"
	defer tmuxCleanup(t, name)

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
	if !strings.Contains(result, "hello tmux") {
		t.Errorf("read result = %q, want 'hello tmux'", result)
	}
}

func TestTmuxReadDefault(t *testing.T) {
	tmuxAvailable(t)
	tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)


	name := "foci-test-readdefault"
	defer tmuxCleanup(t, name)

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
	tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)


	name := "foci-test-kill"
	defer tmuxCleanup(t, name)

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
	if !strings.Contains(result, name) {
		t.Errorf("kill result = %q", result)
	}

	// Verify gone from list
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "list",
	})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(result, name) {
		t.Errorf("session %q still in list after kill", name)
	}
}

func TestTmuxInvalidOperation(t *testing.T) {
	tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)


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
	tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)


	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"command":   "sleep 60",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(result, "foci-") {
		t.Errorf("result = %q, want auto-generated foci-N name", result)
	}

	// Extract name and clean up
	name := strings.TrimPrefix(result, "Session started: ")
	defer tmuxCleanup(t, name)
}

func TestTmuxSendNoEnter(t *testing.T) {
	tmuxAvailable(t)
	tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)


	name := "foci-test-noenter"
	defer tmuxCleanup(t, name)

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
	if result != "Keys sent." {
		t.Errorf("result = %q", result)
	}
}

func TestTmuxMissingName(t *testing.T) {
	tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)


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
	tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)


	name := "foci-test-workdir"
	defer tmuxCleanup(t, name)

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
	if !strings.Contains(result, name) {
		t.Errorf("result = %q", result)
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
	if !strings.Contains(output, dir) && !strings.Contains(output, resolvedDir) {
		t.Errorf("output = %q, want workdir %q or %q", output, dir, resolvedDir)
	}
}

func TestTmuxWatchUnwatch(t *testing.T) {
	tmuxAvailable(t)
	tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)


	name := "foci-test-watch"
	defer tmuxCleanup(t, name)

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
	if !strings.Contains(result, "Watching") {
		t.Errorf("watch result = %q", result)
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
	if !strings.Contains(result, "Stopped watching") {
		t.Errorf("unwatch result = %q", result)
	}
}

func TestTmuxWatchAlreadyWatched(t *testing.T) {
	tmuxAvailable(t)
	tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)


	name := "foci-test-watch-dup"
	defer tmuxCleanup(t, name)

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
	tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)


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
	tmuxAvailable(t)

	var wakeCalled atomic.Int32
	var wakeMsg string
	notifier := NewAsyncNotifier(func(sk, msg string) {
		wakeCalled.Add(1)
		wakeMsg = msg
	})

	tool, _ := NewTmuxTool(300, 30, notifier, nil, "", false, 30)

	name := "foci-test-wake"
	defer tmuxCleanup(t, name)

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
	if !strings.Contains(wakeMsg, "[TMUX WATCH]") {
		t.Errorf("wake message = %q, want to contain [TMUX WATCH]", wakeMsg)
	}

	// Cleanup
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	tool.Execute(context.Background(), params)
}

func TestTmuxWatchDeadSession(t *testing.T) {
	tmuxAvailable(t)

	var msgs []string
	var mu sync.Mutex
	notifier := NewAsyncNotifier(func(sk, msg string) {
		mu.Lock()
		msgs = append(msgs, msg)
		mu.Unlock()
	})

	tool, _ := NewTmuxTool(300, 30, notifier, nil, "", false, 30)

	name := "foci-test-dead"
	defer tmuxCleanup(t, name)

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
	exec.Command("tmux", "kill-session", "-t", name).Run()
	time.Sleep(100 * time.Millisecond)

	// Wait for the monitor to detect the dead session (poll interval is 2s)
	deadline := time.After(10 * time.Second)
	for {
		mu.Lock()
		got := len(msgs)
		mu.Unlock()
		if got > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no notification received for dead session within timeout")
		case <-time.After(200 * time.Millisecond):
		}
	}

	mu.Lock()
	msg := msgs[0]
	mu.Unlock()

	if !strings.Contains(msg, "no longer exists") {
		t.Errorf("message = %q, want to contain 'no longer exists'", msg)
	}
	if !strings.Contains(msg, name) {
		t.Errorf("message = %q, want to contain session name %q", msg, name)
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
}

func TestTmuxWatchMissingName(t *testing.T) {
	tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)


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

	toolA, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)
	toolB, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)

	nameA := "foci-test-iso-a"
	nameB := "foci-test-iso-b"
	defer tmuxCleanup(t, nameA)
	defer tmuxCleanup(t, nameB)

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
	if !strings.Contains(result, nameA) {
		t.Errorf("agent A list missing own session: %q", result)
	}
	// A should see its own session as "owned" and B's as "idle"
	for _, line := range strings.Split(result, "\n") {
		if strings.Contains(line, nameA) && !strings.Contains(line, "owned") {
			t.Errorf("agent A list should show %q as owned: %q", nameA, line)
		}
		if strings.Contains(line, nameB) && strings.Contains(line, "owned") {
			t.Errorf("agent A list should not show %q as owned: %q", nameB, line)
		}
	}

	result, err = toolB.Execute(context.Background(), listParams)
	if err != nil {
		t.Fatalf("agent B list: %v", err)
	}
	if !strings.Contains(result, nameB) {
		t.Errorf("agent B list missing own session: %q", result)
	}
	// B should see its own session as "owned" and A's as "idle"
	for _, line := range strings.Split(result, "\n") {
		if strings.Contains(line, nameB) && !strings.Contains(line, "owned") {
			t.Errorf("agent B list should show %q as owned: %q", nameB, line)
		}
		if strings.Contains(line, nameA) && strings.Contains(line, "owned") {
			t.Errorf("agent B list should not show %q as owned: %q", nameA, line)
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
	tmuxAvailable(t)

	var wakeA, wakeB atomic.Int32
	toolA, _ := NewTmuxTool(300, 30, NewAsyncNotifier(func(sk, msg string) {
		wakeA.Add(1)
	}), nil, "", false, 30)
	toolB, _ := NewTmuxTool(300, 30, NewAsyncNotifier(func(sk, msg string) {
		wakeB.Add(1)
	}), nil, "", false, 30)

	nameA := "foci-test-wakeroute-a"
	nameB := "foci-test-wakeroute-b"
	exec.Command("tmux", "kill-session", "-t", nameA).Run()
	exec.Command("tmux", "kill-session", "-t", nameB).Run()
	defer tmuxCleanup(t, nameA)
	defer tmuxCleanup(t, nameB)

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

	toolA, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)

	toolB, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)


	name := "foci-test-watchiso"
	defer tmuxCleanup(t, name)

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
	tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)


	name := "foci-test-readraw"
	defer tmuxCleanup(t, name)

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
	if !strings.Contains(result, "Claude Code") {
		t.Errorf("raw read should preserve all content, got:\n%s", result)
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

	tool, _ := NewTmuxTool(300, 30, nil, store, "tmux:test-agent", false, 30)

	name := "foci-test-persist"
	defer tmuxCleanup(t, name)

	// Start a session
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Verify state was persisted
	var owned []string
	if !store.Get("tmux:test-agent", &owned) {
		t.Fatal("owned sessions not persisted")
	}
	if len(owned) != 1 || owned[0] != name {
		t.Errorf("persisted sessions = %v, want [%s]", owned, name)
	}
}

func TestTmuxRestoreOwnedSessions(t *testing.T) {
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)

	// Pre-populate state with an owned session
	if err := store.Set("tmux:test-agent", []string{"foci-test-restore"}); err != nil {
		t.Fatalf("set state: %v", err)
	}

	// Create the tmux session (simulating it still exists from before restart)
	exec.Command("tmux", "new-session", "-d", "-s", "foci-test-restore", "sleep", "60").Run()
	defer tmuxCleanup(t, "foci-test-restore")

	// Load state
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	// Create tool with state store - should restore owned sessions
	tool, _ := NewTmuxTool(300, 30, nil, store, "tmux:test-agent", false, 30)

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

	tool, _ := NewTmuxTool(300, 30, nil, store, "tmux:test-agent", false, 30)

	name := "foci-test-persistkill"
	defer tmuxCleanup(t, name)

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
	var owned []string
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
	if !store.Get("tmux:test-agent", &owned) {
		t.Fatal("owned sessions key should still exist")
	}
	if len(owned) != 0 {
		t.Errorf("persisted sessions after kill = %v, want empty", owned)
	}
}

func TestTmuxPersistClearedOnStaleSessions(t *testing.T) {
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)

	// Pre-populate state with sessions that no longer exist
	if err := store.Set("tmux:test-agent", []string{"foci-test-stale1", "foci-test-stale2"}); err != nil {
		t.Fatalf("set state: %v", err)
	}

	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	tool, _ := NewTmuxTool(300, 30, nil, store, "tmux:test-agent", false, 30)

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
	var owned []string
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
	tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)


	name := "foci-test-nostate"
	defer tmuxCleanup(t, name)

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
	if !strings.Contains(result, name) {
		t.Errorf("list result = %q, want to contain %s", result, name)
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

	tool1, _ := NewTmuxTool(300, 30, nil, store1, "tmux:test-agent", false, 30)

	name := "foci-test-roundtrip"
	defer tmuxCleanup(t, name)

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

	tool2, _ := NewTmuxTool(300, 30, nil, store2, "tmux:test-agent", false, 30)

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

	notifier := NewAsyncNotifier(func(sk, msg string) {})
	tool, _ := NewTmuxTool(300, 30, notifier, store, "tmux:test-agent", false, 30)

	name := "foci-test-persist-watch"
	defer tmuxCleanup(t, name)

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
	defer tmuxCleanup(t, name)

	// Create the tmux session (simulating it still exists from before restart)
	exec.Command("tmux", "new-session", "-d", "-s", name, "sleep", "60").Run()

	// Pre-populate state with owned session and watch
	if err := store.Set("tmux:test-agent", []string{name}); err != nil {
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

	notifier := NewAsyncNotifier(func(sk, msg string) {})
	_, cleanup := NewTmuxTool(300, 30, notifier, store, "tmux:test-agent", false, 30)

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

	notifier := NewAsyncNotifier(func(sk, msg string) {})
	NewTmuxTool(300, 30, notifier, store, "tmux:test-agent", false, 30)

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

	notifier := NewAsyncNotifier(func(sk, msg string) {})
	tool, _ := NewTmuxTool(300, 30, notifier, store, "tmux:test-agent", false, 30)

	name := "foci-test-unwatch-persist"
	defer tmuxCleanup(t, name)

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

	notifier := NewAsyncNotifier(func(sk, msg string) {})
	tool, cleanup := NewTmuxTool(300, 30, notifier, store, "tmux:test-agent", false, 30)

	name := "foci-test-clearall-watch"
	defer tmuxCleanup(t, name)

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

	notifier := NewAsyncNotifier(func(sk, msg string) {})
	tool1, cleanup1 := NewTmuxTool(300, 30, notifier, store, "tmux:test-agent", false, 30)
	defer cleanup1()

	name := "foci-test-unwatch-restart"
	defer tmuxCleanup(t, name)

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

	tool2, cleanup2 := NewTmuxTool(300, 30, notifier, store2, "tmux:test-agent", false, 30)
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

	notifier := NewAsyncNotifier(func(sk, msg string) {})
	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	tool, _ := NewTmuxTool(300, 30, notifier, store, "tmux:test-autowatch", false, 30)

	name := "foci-test-autowatch"
	defer tmuxCleanup(t, name)

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
	if !strings.Contains(result, "Session started") {
		t.Errorf("result missing 'Session started': %q", result)
	}
	if !strings.Contains(result, "Watching") {
		t.Errorf("result missing watch confirmation: %q", result)
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

	notifier := NewAsyncNotifier(func(sk, msg string) {})
	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	tool, _ := NewTmuxTool(300, 30, notifier, store, "tmux:test-nowatch", false, 30)

	name := "foci-test-nowatch"
	defer tmuxCleanup(t, name)

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
	if !strings.Contains(result, "Session started") {
		t.Errorf("result missing 'Session started': %q", result)
	}
	if strings.Contains(result, "Watching") {
		t.Errorf("result should NOT contain watch confirmation when watch=false: %q", result)
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
	tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30)

	name := "foci-test-autowatch-nonotif"
	defer tmuxCleanup(t, name)

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
	if !strings.Contains(result, "Session started") {
		t.Errorf("result missing 'Session started': %q", result)
	}
	// Should not contain watch info since no notifier
	if strings.Contains(result, "Watching") {
		t.Errorf("result should NOT contain watch when no notifier: %q", result)
	}
}

func TestTmuxAutopilotAutoUnwatch(t *testing.T) {
	tmuxAvailable(t)

	var mu sync.Mutex
	var notifications []string
	notifier := NewAsyncNotifier(func(sk, msg string) {
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
	tool, _ := NewTmuxTool(300, 30, notifier, store, "tmux:test-autopilot-unwatch", true, 2)

	name := "foci-test-ap-unwatch"
	tmuxCleanup(t, name) // clean up stale sessions from prior crashed runs
	defer tmuxCleanup(t, name)

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
	if !strings.Contains(result, "Watching") {
		t.Fatalf("expected auto-watch on start: %q", result)
	}

	// Wait for inactivity notification (threshold=2s, monitor polls every 2s)
	time.Sleep(6 * time.Second)

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
	if strings.Contains(result, "watched") {
		t.Errorf("expected watch to be auto-removed after inactivity (autopilot), got: %q", result)
	}
}

func TestTmuxAutopilotAutoWatchOnSend(t *testing.T) {
	tmuxAvailable(t)

	notifier := NewAsyncNotifier(func(sk, msg string) {})
	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	// autopilot=true
	tool, _ := NewTmuxTool(300, 30, notifier, store, "tmux:test-autopilot-send", true, 30)

	name := "foci-test-ap-send"
	defer tmuxCleanup(t, name)

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
	if !strings.Contains(result, "Watching") {
		t.Errorf("expected auto-watch on send (autopilot), got: %q", result)
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
	if strings.Contains(result, "Watching") {
		t.Errorf("should not re-watch already watched session, got: %q", result)
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

	notifier := NewAsyncNotifier(func(sk, msg string) {})
	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	// autopilot=false
	tool, _ := NewTmuxTool(300, 30, notifier, store, "tmux:test-no-autopilot", false, 30)

	name := "foci-test-no-ap"
	defer tmuxCleanup(t, name)

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
	if strings.Contains(result, "Watching") {
		t.Errorf("should not auto-watch when autopilot=false, got: %q", result)
	}
}
