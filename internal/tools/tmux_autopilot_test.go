package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/session"
)

func TestTmuxStartAutoWatch(t *testing.T) {
	// Verifies that starting a session with the default watch setting automatically activates watch monitoring and persists the watch entry to the state store.
	t.Parallel()
	tmuxAvailable(t)

	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	_, tool, _, _ := NewTmuxTool(300, 30, notifier, idx, "test-autowatch", false, 30, 0)

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
	if !strings.Contains(result.Text, "Session started") {
		t.Errorf("result missing 'Session started': %q", result.Text)
	}
	if !strings.Contains(result.Text, "Watching") {
		t.Errorf("result missing watch confirmation: %q", result.Text)
	}

	// Verify watch was persisted
	rawW, errW := idx.GetAgentMetadata("test-autowatch", "tmux_watches")
	if errW != nil || rawW == "" {
		t.Fatal("watches not persisted after auto-watch")
	}
	var watches []persistedWatch
	if err := json.Unmarshal([]byte(rawW), &watches); err != nil {
		t.Fatalf("unmarshal watches: %v", err)
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
	// Verifies that explicitly setting watch=false on start prevents auto-watch and leaves no watch entries in the state store.
	t.Parallel()
	tmuxAvailable(t)

	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	_, tool, _, _ := NewTmuxTool(300, 30, notifier, idx, "test-nowatch", false, 30, 0)

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
	if !strings.Contains(result.Text, "Session started") {
		t.Errorf("result missing 'Session started': %q", result.Text)
	}
	if strings.Contains(result.Text, "Watching") {
		t.Errorf("result should NOT contain watch confirmation when watch=false: %q", result.Text)
	}

	// Verify no watches persisted
	rawW, _ := idx.GetAgentMetadata("test-nowatch", "tmux_watches")
	if rawW != "" {
		var watches []persistedWatch
		if err := json.Unmarshal([]byte(rawW), &watches); err == nil && len(watches) != 0 {
			t.Errorf("persisted watches = %d, want 0 when watch=false", len(watches))
		}
	}
}

func TestTmuxStartAutoWatchNoNotifier(t *testing.T) {
	// Verifies that auto-watch is silently skipped when no notifier is configured, so the session starts successfully without watch-related output.
	t.Parallel()
	tmuxAvailable(t)
	_, tool, _, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

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
	if !strings.Contains(result.Text, "Session started") {
		t.Errorf("result missing 'Session started': %q", result.Text)
	}
	// Should not contain watch info since no notifier
	if strings.Contains(result.Text, "Watching") {
		t.Errorf("result should NOT contain watch when no notifier: %q", result.Text)
	}
}

func TestTmuxAutopilotAutoUnwatch(t *testing.T) {
	// Verifies that in autopilot mode, a session watch is automatically removed after the inactivity threshold fires, so the agent is not re-woken repeatedly.
	t.Parallel()
	tmuxAvailable(t)

	var mu sync.Mutex
	var notifications []string
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		mu.Lock()
		notifications = append(notifications, msg)
		mu.Unlock()
	})

	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	// autopilot=true, threshold=2s for fast test
	_, tool, _, _ := NewTmuxTool(300, 30, notifier, idx, "test-autopilot-unwatch", true, 2, 0)

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
	if !strings.Contains(result.Text, "Watching") {
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
	if strings.Contains(result.Text, "watched") {
		t.Errorf("expected watch to be auto-removed after inactivity (autopilot), got: %q", result.Text)
	}
}

func TestTmuxAutopilotAutoWatchOnSend(t *testing.T) {
	// Verifies that in autopilot mode, sending keys to an unwatched session automatically starts watching it, and subsequent sends don't register a duplicate watch.
	t.Parallel()
	tmuxAvailable(t)

	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	// autopilot=true
	_, tool, _, _ := NewTmuxTool(300, 30, notifier, idx, "test-autopilot-send", true, 30, 0)

	name := "foci-test-ap-send"
	tmuxSetup(t, name)

	// Start with watch=false so session is unwatched
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "cat",
		"watch":     false,
	})
	_, err = tool.Execute(context.Background(), params)
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
	if !strings.Contains(result.Text, "Watching") {
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
	if strings.Contains(result.Text, "Watching") {
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
	// Verifies that when autopilot is disabled, sending to an unwatched session does not trigger auto-watch, leaving watch management entirely manual.
	t.Parallel()
	tmuxAvailable(t)

	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	// autopilot=false
	_, tool, _, _ := NewTmuxTool(300, 30, notifier, idx, "test-no-autopilot", false, 30, 0)

	name := "foci-test-no-ap"
	tmuxSetup(t, name)

	// Start with watch=false
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "cat",
		"watch":     false,
	})
	_, err = tool.Execute(context.Background(), params)
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
	if strings.Contains(result.Text, "Watching") {
		t.Errorf("should not auto-watch when autopilot=false, got: %q", result.Text)
	}
}
