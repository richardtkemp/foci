package tmux

import (
	"context"
	"encoding/json"
	"foci/internal/tools"
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
	sock := tmuxIsolatedSocket(t)

	var fired atomic.Int32
	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string, _ *tools.AgentOrigin) {
		fired.Add(1)
	})

	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	// autopilot=true, threshold=2s. The monitor polls and sees time pass only
	// when the driver says so.
	d := newWatchDriver(t)
	watchCount, tool := d.tool(notifier, idx, "test-cond-nofire", true, 2, sock)

	name := "foci-test-cond-nofire"

	// Start with watch=false, then read to trigger conditional watch
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   testSessionCmd,
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

	// Idle far past the threshold, polling before and after: the watch must
	// NOT fire because no activity ever occurred. Proving this absence needs
	// no real wait: the monitor only decides on a poll, and poll() returns
	// once that decision has been acted on.
	d.poll()
	d.advance(10 * 2 * time.Second)
	for range 2 {
		d.poll()
		if n := fired.Load(); n != 0 {
			t.Fatalf("conditional watch fired %d time(s) without activity, want 0", n)
		}
		// Still armed (an autopilot watch that fired would also have removed itself).
		if n := watchCount(); n != 1 {
			t.Fatalf("watch count = %d after idling, want 1 (conditional watch still waiting for activity)", n)
		}
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
	sock := tmuxIsolatedSocket(t)

	var mu sync.Mutex
	var notifications []string
	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string, _ *tools.AgentOrigin) {
		mu.Lock()
		notifications = append(notifications, msg)
		mu.Unlock()
	})

	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	// autopilot=true, threshold=2s, driven clock and polls.
	d := newWatchDriver(t)
	_, tool := d.tool(notifier, idx, "test-cond-fire", true, 2, sock)
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(notifications)
	}

	name := "foci-test-cond-fire"

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

	// While still conditional, idling past the threshold fires nothing.
	d.poll()
	d.advance(3 * time.Second)
	d.poll()
	if n := count(); n != 0 {
		t.Fatalf("conditional watch fired %d time(s) before any activity, want 0", n)
	}

	// Produce activity: send some text to cat. The pane then shows it twice
	// (the tty's echo of the typed line, then cat's copy); wait for both so
	// the pane is settled before the monitor looks, or a late redraw could
	// count as fresh activity after the clock has moved.
	if out, err := runTmuxWithSocket(context.Background(), sock, "send-keys", "-t", name, "-l", "activity!"); err != nil {
		t.Fatalf("send-keys: %v: %s", err, out)
	}
	if out, err := runTmuxWithSocket(context.Background(), sock, "send-keys", "-t", name, "Enter"); err != nil {
		t.Fatalf("send-keys Enter: %v: %s", err, out)
	}
	if !pollUntil(t, 10*time.Second, func() bool {
		out, err := runTmuxWithSocket(context.Background(), sock, "capture-pane", "-t", name, "-p")
		return err == nil && strings.Count(out, "activity!") >= 2
	}) {
		t.Fatal("cat never echoed the activity into the pane")
	}

	// The monitor sees the change (converting the watch to a normal one and
	// restarting the idle clock): no notification for activity itself.
	d.poll()
	if n := count(); n != 0 {
		t.Fatalf("watch fired %d time(s) on the activity poll, want 0", n)
	}

	// Idle past the threshold after the activity: now it fires.
	d.advance(3 * time.Second)
	d.poll()
	if n := count(); n != 1 {
		t.Fatalf("notifications = %d after activity then idle, want 1", n)
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
	sock := tmuxIsolatedSocket(t)

	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string, _ *tools.AgentOrigin) {})
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	// autopilot=false
	_, tool, _ := NewTmuxTool(300, 30, notifier, idx, "test-no-cond", false, 30, 0, sock)

	name := "foci-test-no-cond"

	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   testSessionCmd,
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
	sock := tmuxIsolatedSocket(t)

	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string, _ *tools.AgentOrigin) {})
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	// autopilot=true
	_, tool, _ := NewTmuxTool(300, 30, notifier, idx, "test-cond-dup", true, 30, 0, sock)

	name := "foci-test-cond-dup"

	// Start with watch=true — auto-watches normally
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   testSessionCmd,
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
	sock := tmuxIsolatedSocket(t)

	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string, _ *tools.AgentOrigin) {})
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	// autopilot=true
	_, tool, _ := NewTmuxTool(300, 30, notifier, idx, "test-cond-persist", true, 30, 0, sock)

	name := "foci-test-cond-persist"

	// Start with watch=false
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   testSessionCmd,
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
