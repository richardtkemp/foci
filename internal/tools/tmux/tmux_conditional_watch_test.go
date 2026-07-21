package tmux

import (
	"context"
	"encoding/json"
	"foci/internal/tools"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/session"
)

func TestTmuxConditionalWatchNoActivityNoFire(t *testing.T) {
	// Verifies that a conditional watch on an idle session never fires: if no
	// activity is detected, the watch remains in conditional state and does not
	// send an inactivity notification.
	t.Parallel()
	tmuxAvailable(t)

	var fired atomic.Int32
	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		fired.Add(1)
	})

	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	// autopilot=true, threshold=2s for fast test
	_, tool, _ := NewTmuxTool(300, 30, notifier, idx, "test-cond-nofire", true, 2, 0, "")

	name := "foci-test-cond-nofire"
	tmuxSetup(t, name)

	// Start with watch=false, then read to trigger conditional watch
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
		"watch":     false,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Read — should set conditional watch (autopilot=true)
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      name,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(result.Text, "Conditionally watching") {
		t.Errorf("expected conditional watch confirmation, got: %q", result.Text)
	}

	// Wait well past the threshold — should NOT fire because no activity
	// occurred. This is a genuine fixed-duration wait, not a latent race: it's
	// proving a negative (the callback did NOT fire), and polling can't prove
	// an absence faster than just waiting out the window the monitor would
	// need to have fired in (threshold 2s + its ~2s poll interval).
	time.Sleep(5 * time.Second)

	if n := fired.Load(); n != 0 {
		t.Errorf("conditional watch fired %d time(s) without activity, want 0", n)
	}

	// Cleanup
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	tool.Execute(context.Background(), params)
}

func TestTmuxConditionalWatchActivityThenFire(t *testing.T) {
	// Verifies that a conditional watch converts to a normal watch after
	// detecting activity, then fires the inactivity notification once the
	// session becomes idle again.
	t.Parallel()
	tmuxAvailable(t)

	var mu sync.Mutex
	var notifications []string
	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		mu.Lock()
		notifications = append(notifications, msg)
		mu.Unlock()
	})

	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	// autopilot=true, threshold=2s
	_, tool, _ := NewTmuxTool(300, 30, notifier, idx, "test-cond-fire", true, 2, 0, "")

	name := "foci-test-cond-fire"
	tmuxSetup(t, name)

	// Start with watch=false, use cat so we can produce output
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "cat",
		"watch":     false,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Read — sets conditional watch
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      name,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(result.Text, "Conditionally watching") {
		t.Fatalf("expected conditional watch, got: %q", result.Text)
	}

	// Produce activity: send some text to cat (which echoes it)
	exec.Command("tmux", "-S", tmuxSocketPath, "send-keys", "-t", name, "-l", "activity!").Run()
	exec.Command("tmux", "-S", tmuxSocketPath, "send-keys", "-t", name, "Enter").Run()

	// Wait for the monitor to detect activity (poll interval 2s) then
	// for the inactivity threshold (2s) to fire — total ~6s should be enough.
	deadline := time.After(10 * time.Second)
	for {
		mu.Lock()
		got := len(notifications)
		mu.Unlock()
		if got > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("conditional watch never fired after activity")
		case <-time.After(200 * time.Millisecond):
		}
	}

	mu.Lock()
	msg := notifications[0]
	mu.Unlock()

	if !strings.Contains(msg, name) {
		t.Errorf("notification = %q, want to contain %q", msg, name)
	}
}

func TestTmuxReadNoConditionalWatchWithoutAutopilot(t *testing.T) {
	// Verifies that read does NOT set a conditional watch when autopilot is
	// disabled, keeping watch management fully manual.
	t.Parallel()
	tmuxAvailable(t)

	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	// autopilot=false
	_, tool, _ := NewTmuxTool(300, 30, notifier, idx, "test-no-cond", false, 30, 0, "")

	name := "foci-test-no-cond"
	tmuxSetup(t, name)

	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
		"watch":     false,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Read — should NOT set conditional watch (autopilot=false)
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      name,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(result.Text, "Conditionally watching") || strings.Contains(result.Text, "Watching") {
		t.Errorf("should not watch when autopilot=false, got: %q", result.Text)
	}
}

func TestTmuxReadNoConditionalWatchIfAlreadyWatched(t *testing.T) {
	// Verifies that reading a session that is already watched does NOT add a
	// second (conditional) watch, even with autopilot enabled.
	t.Parallel()
	tmuxAvailable(t)

	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	// autopilot=true
	_, tool, _ := NewTmuxTool(300, 30, notifier, idx, "test-cond-dup", true, 30, 0, "")

	name := "foci-test-cond-dup"
	tmuxSetup(t, name)

	// Start with watch=true — auto-watches normally
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
		t.Fatalf("expected auto-watch on start, got: %q", result.Text)
	}

	// Read — should NOT add conditional watch (already watched)
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      name,
	})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(result.Text, "Conditionally watching") {
		t.Errorf("should not add conditional watch on already-watched session, got: %q", result.Text)
	}

	// Cleanup
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	tool.Execute(context.Background(), params)
}

func TestTmuxConditionalWatchPersistence(t *testing.T) {
	// Verifies that a conditional watch is persisted with the conditional flag
	// set, so it survives restarts in its conditional state.
	t.Parallel()
	tmuxAvailable(t)

	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	// autopilot=true
	_, tool, _ := NewTmuxTool(300, 30, notifier, idx, "test-cond-persist", true, 30, 0, "")

	name := "foci-test-cond-persist"
	tmuxSetup(t, name)

	// Start with watch=false
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
		"watch":     false,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Read — sets conditional watch
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      name,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(result.Text, "Conditionally watching") {
		t.Fatalf("expected conditional watch, got: %q", result.Text)
	}

	// Verify persisted watch has conditional flag
	rawW, errW := idx.GetAgentMetadata("test-cond-persist", "tmux_watches")
	if errW != nil || rawW == "" {
		t.Fatal("watches not persisted")
	}
	var watches []persistedWatch
	if err := json.Unmarshal([]byte(rawW), &watches); err != nil {
		t.Fatalf("unmarshal watches: %v", err)
	}
	if len(watches) != 1 {
		t.Fatalf("persisted watches = %d, want 1", len(watches))
	}
	if !watches[0].Conditional {
		t.Error("persisted watch should have Conditional=true")
	}

	// Cleanup
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	tool.Execute(context.Background(), params)
}
