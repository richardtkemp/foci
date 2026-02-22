package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	tool := NewTmuxTool(300, 30, nil)

	name := "clod-test-start"
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
}

func TestTmuxSendAndRead(t *testing.T) {
	tmuxAvailable(t)
	tool := NewTmuxTool(300, 30, nil)

	name := "clod-test-sendread"
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
	tool := NewTmuxTool(300, 30, nil)

	name := "clod-test-readdefault"
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
	tool := NewTmuxTool(300, 30, nil)

	name := "clod-test-kill"
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
	tool := NewTmuxTool(300, 30, nil)

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
	tool := NewTmuxTool(300, 30, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"command":   "sleep 60",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(result, "clod-") {
		t.Errorf("result = %q, want auto-generated clod-N name", result)
	}

	// Extract name and clean up
	name := strings.TrimPrefix(result, "Session started: ")
	defer tmuxCleanup(t, name)
}

func TestTmuxSendNoEnter(t *testing.T) {
	tmuxAvailable(t)
	tool := NewTmuxTool(300, 30, nil)

	name := "clod-test-noenter"
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
	tool := NewTmuxTool(300, 30, nil)

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
	tool := NewTmuxTool(300, 30, nil)

	name := "clod-test-workdir"
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
	tool := NewTmuxTool(300, 30, nil)

	name := "clod-test-watch"
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
	tool := NewTmuxTool(300, 30, nil)

	name := "clod-test-watch-dup"
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
	tool := NewTmuxTool(300, 30, nil)

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
	var wakeSession string
	var wakeWindow int
	wakeFn := func(session string, window int, threshold time.Duration) {
		wakeCalled.Add(1)
		wakeSession = session
		wakeWindow = window
	}

	tool := NewTmuxTool(300, 30, wakeFn)

	name := "clod-test-wake"
	defer tmuxCleanup(t, name)

	// Start a session that does nothing (sleep)
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
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

	if wakeSession != name {
		t.Errorf("wake session = %q, want %q", wakeSession, name)
	}
	if wakeWindow != 0 {
		t.Errorf("wake window = %d, want 0", wakeWindow)
	}

	// Cleanup
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	tool.Execute(context.Background(), params)
}

func TestTmuxWatchMissingName(t *testing.T) {
	tool := NewTmuxTool(300, 30, nil)

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

	toolA := NewTmuxTool(300, 30, nil)
	toolB := NewTmuxTool(300, 30, nil)

	nameA := "clod-test-iso-a"
	nameB := "clod-test-iso-b"
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

	// Agent A's list should only show A's session
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
	if strings.Contains(result, nameB) {
		t.Errorf("agent A list shows agent B's session: %q", result)
	}

	// Agent B's list should only show B's session
	result, err = toolB.Execute(context.Background(), listParams)
	if err != nil {
		t.Fatalf("agent B list: %v", err)
	}
	if !strings.Contains(result, nameB) {
		t.Errorf("agent B list missing own session: %q", result)
	}
	if strings.Contains(result, nameA) {
		t.Errorf("agent B list shows agent A's session: %q", result)
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
	toolA := NewTmuxTool(300, 30, func(session string, window int, threshold time.Duration) {
		wakeA.Add(1)
	})
	toolB := NewTmuxTool(300, 30, func(session string, window int, threshold time.Duration) {
		wakeB.Add(1)
	})

	nameA := "clod-test-wakeroute-a"
	nameB := "clod-test-wakeroute-b"
	defer tmuxCleanup(t, nameA)
	defer tmuxCleanup(t, nameB)

	// Agent A starts and watches a session
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      nameA,
		"command":   "sleep 60",
	})
	if _, err := toolA.Execute(context.Background(), params); err != nil {
		t.Fatalf("agent A start: %v", err)
	}

	// Agent B starts and watches a session
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      nameB,
		"command":   "sleep 60",
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

	toolA := NewTmuxTool(300, 30, nil)
	toolB := NewTmuxTool(300, 30, nil)

	name := "clod-test-watchiso"
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

func TestNormalizePaneContent_TokenCounts(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Context: 88,447 tokens used", "Context:  used"},
		{"Used 1500 tokens", "Used "},
		{"12 tokens left", " left"},
	}
	for _, tt := range tests {
		got := normalizePaneContent(tt.input)
		if got != tt.want {
			t.Errorf("normalizePaneContent(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizePaneContent_Percentages(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"44% used", " used"},
		{"Context: 88.5% full", "Context:  full"},
		{"Progress: 100%", "Progress: "},
	}
	for _, tt := range tests {
		got := normalizePaneContent(tt.input)
		if got != tt.want {
			t.Errorf("normalizePaneContent(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizePaneContent_Costs(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Cost: $0.0430", "Cost: "},
		{"Total $12.50 spent", "Total  spent"},
	}
	for _, tt := range tests {
		got := normalizePaneContent(tt.input)
		if got != tt.want {
			t.Errorf("normalizePaneContent(%q) = %q, want %q", tt.input, got, tt.want)
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

func TestNormalizePaneContent_Spinners(t *testing.T) {
	// Braille spinners used by many TUI apps
	input1 := "⠋ Loading..."
	input2 := "⠙ Loading..."
	norm1 := normalizePaneContent(input1)
	norm2 := normalizePaneContent(input2)
	if norm1 != norm2 {
		t.Errorf("spinner frames should normalize to same: %q vs %q", norm1, norm2)
	}
}

func TestNormalizePaneContent_PreservesContent(t *testing.T) {
	// Meaningful content should be preserved
	tests := []string{
		"$ ls -la",
		"error: file not found",
		"Build succeeded",
		"PASS ok clod/tools 0.004s",  // "0.004s" gets stripped but that's fine
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
	// All dynamic parts should be stripped; static text remains
	if strings.Contains(got, "44%") {
		t.Errorf("percentage not stripped: %q", got)
	}
	if strings.Contains(got, "12,543 tokens") {
		t.Errorf("token count not stripped: %q", got)
	}
	if strings.Contains(got, "2m 30s") {
		t.Errorf("elapsed timer not stripped: %q", got)
	}
	if strings.Contains(got, "$0.0430") {
		t.Errorf("cost not stripped: %q", got)
	}
}

func TestNormalizePaneContent_StableHash(t *testing.T) {
	// Two snapshots that differ only in TUI noise should normalize
	// to identical strings (and thus hash the same).
	snap1 := `$ opencode
OpenCode v0.1 | claude-3-5-sonnet
⠋ Thinking... | 44% context | 12,543 tokens | 1m 3s | $0.0200
> How do I fix the bug?`

	snap2 := `$ opencode
OpenCode v0.1 | claude-3-5-sonnet
⠹ Thinking... | 48% context | 14,221 tokens | 2m 54s | $0.0430
> How do I fix the bug?`

	norm1 := normalizePaneContent(snap1)
	norm2 := normalizePaneContent(snap2)

	if norm1 != norm2 {
		t.Errorf("snapshots with only TUI noise differences should normalize equally:\n  snap1: %q\n  snap2: %q", norm1, norm2)
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
