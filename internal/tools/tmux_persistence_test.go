package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/state"
)

// TestTmuxPersistOwnedSessions verifies that owned sessions are persisted.
func TestTmuxPersistOwnedSessions(t *testing.T) {
	t.Parallel()
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

// TestTmuxRestoreOwnedSessions verifies that owned sessions are restored.
func TestTmuxRestoreOwnedSessions(t *testing.T) {
	t.Parallel()
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)

	// Pre-populate state with an owned session
	if err := store.Set("tmux:test-agent", map[string]string{"foci-test-restore": ""}); err != nil {
		t.Fatalf("set state: %v", err)
	}

	// Create the tmux session (simulating it still exists from before restart)
	tmuxSetup(t, "foci-test-restore")
	if err := exec.Command("tmux", "-S", tmuxSocketPath, "new-session", "-d", "-s", "foci-test-restore", "sleep", "60").Run(); err != nil {
		t.Fatalf("create tmux session: %v", err)
	}

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

// TestTmuxPersistOnKill verifies that sessions are removed from state when killed.
func TestTmuxPersistOnKill(t *testing.T) {
	t.Parallel()
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	_, tool, _ := NewTmuxTool(300, 30, nil, store, "tmux:test-agent", false, 30, 0)

	name := "foci-test-persistkill"
	tmuxSetupWithSentinel(t, name)

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

// TestTmuxPersistClearedOnStaleSessions verifies stale sessions are cleaned from state.
func TestTmuxPersistClearedOnStaleSessions(t *testing.T) {
	// NOT parallel: lists sessions on the shared tmux server which
	// other parallel tests may create/destroy concurrently.
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

// TestTmuxNoStateStore verifies that tool works without state store.
func TestTmuxNoStateStore(t *testing.T) {
	t.Parallel()
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
	if !strings.Contains(result.Text, name) {
		t.Errorf("list result = %q, want to contain %s", result.Text, name)
	}
}

// TestTmuxStateFileRoundTrip verifies state persistence and restoration end-to-end.
func TestTmuxStateFileRoundTrip(t *testing.T) {
	t.Parallel()
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

// TestTmuxPersistWatches verifies that watches are persisted to state.
func TestTmuxPersistWatches(t *testing.T) {
	t.Parallel()
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
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

// TestTmuxRestoreWatches verifies that watches are restored from state.
func TestTmuxRestoreWatches(t *testing.T) {
	t.Parallel()
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)

	name := "foci-test-restore-watch"
	tmuxSetup(t, name)

	// Create the tmux session (simulating it still exists from before restart)
	if err := exec.Command("tmux", "-S", tmuxSocketPath, "new-session", "-d", "-s", name, "sleep", "60").Run(); err != nil {
		t.Fatalf("create tmux session: %v", err)
	}

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

	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
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

// TestTmuxRestoreWatchesStaleSessions verifies stale watched sessions are cleaned.
func TestTmuxRestoreWatchesStaleSessions(t *testing.T) {
	t.Parallel()
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

	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
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

// TestTmuxUnwatchPersists verifies that unwatch updates persisted state.
func TestTmuxUnwatchPersists(t *testing.T) {
	t.Parallel()
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
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

// TestTmuxClearAllPersistsWatches verifies that ClearAll removes watches from state.
func TestTmuxClearAllPersistsWatches(t *testing.T) {
	t.Parallel()
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
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

// TestTmuxUnwatchNotRestoredOnRestart verifies unwatched sessions don't restore.
func TestTmuxUnwatchNotRestoredOnRestart(t *testing.T) {
	t.Parallel()
	tmuxAvailable(t)

	stateFile := filepath.Join(t.TempDir(), "state.json")
	store := state.New(stateFile)
	if err := store.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
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
