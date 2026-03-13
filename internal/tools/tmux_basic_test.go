package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTmuxStartAndList(t *testing.T) {
	// Verifies that sessions can be started and listed correctly, confirming the session name appears in list output with proper ownership markers.
	t.Parallel()
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
	if !strings.Contains(result.Text, name) {
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
	if !strings.Contains(result.Text, name) {
		t.Errorf("list result = %q, want %q", result.Text, name)
	}
	// Should have header line
	if !strings.Contains(result.Text, "SESSION") {
		t.Errorf("list result missing header: %q", result.Text)
	}
	// Owned session should show owner (not "-")
	if !strings.Contains(result.Text, "self") {
		t.Errorf("list result missing owner: %q", result.Text)
	}
}

func TestTmuxSendAndRead(t *testing.T) {
	// Verifies that text sent to a session appears in the read output, confirming the send→read round-trip works correctly.
	t.Parallel()
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
	if !strings.Contains(result.Text, "hello tmux") {
		t.Errorf("read result = %q, want 'hello tmux'", result.Text)
	}
}

func TestTmuxReadDefault(t *testing.T) {
	// Verifies that read succeeds with no explicit line count, confirming the default parameter works without error.
	t.Parallel()
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
	// Verifies that killing a session removes it from the list, confirming kill actually terminates and deregisters the session.
	t.Parallel()
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
	if !strings.Contains(result.Text, name) {
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
	if strings.Contains(result.Text, name) {
		t.Errorf("session %q still in list after kill", name)
	}
}

func TestTmuxStartWithWorkdir(t *testing.T) {
	// Verifies that a session started with a workdir parameter actually runs in that directory, confirmed by reading pwd output.
	t.Parallel()
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
	if !strings.Contains(result.Text, name) {
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
