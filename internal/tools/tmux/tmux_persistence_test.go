package tmux

import (
	"context"
	"encoding/json"
	"foci/internal/tools"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/session"
)

func TestTmuxPersistOwnedSessions(t *testing.T) {
	// Verifies that starting a session writes the session name to the session index under the agent key, so ownership survives process restart.
	t.Parallel()
	sock := tmuxIsolatedSocket(t)

	// Create a temp DB for state persistence
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	_, tool, _ := NewTmuxTool(300, 30, nil, idx, "test-agent", false, 30, 0, sock)

	name := "foci-test-persist"

	// Start a session
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Verify state was persisted (new format: map[string]string as JSON)
	raw, err := idx.GetAgentMetadata("test-agent", "tmux_owned")
	if err != nil || raw == "" {
		t.Fatal("owned sessions not persisted")
	}
	var owned map[string]string
	if err := json.Unmarshal([]byte(raw), &owned); err != nil {
		t.Fatalf("unmarshal owned: %v", err)
	}
	if len(owned) != 1 {
		t.Errorf("persisted sessions = %v, want 1 entry", owned)
	}
	if _, ok := owned[name]; !ok {
		t.Errorf("persisted sessions = %v, want key %s", owned, name)
	}
}

func TestTmuxRestoreOwnedSessions(t *testing.T) {
	// Verifies that a tool instance initialized with pre-populated state can read sessions that were persisted by a previous instance.
	t.Parallel()
	sock := tmuxIsolatedSocket(t)

	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Pre-populate state with an owned session
	ownedJSON, _ := json.Marshal(map[string]string{"foci-test-restore": ""})
	if err := idx.SetAgentMetadata("test-agent", "tmux_owned", string(ownedJSON)); err != nil {
		t.Fatalf("set state: %v", err)
	}

	// Create the tmux session (simulating it still exists from before restart)
	if err := exec.Command("tmux", "-S", sock, "new-session", "-d", "-s", "foci-test-restore", "sleep", "60").Run(); err != nil {
		t.Fatalf("create tmux session: %v", err)
	}

	// Create tool with session index - should restore owned sessions
	_, tool, _ := NewTmuxTool(300, 30, nil, idx, "test-agent", false, 30, 0, sock)

	// Read should succeed because the session is in the restored owned set
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      "foci-test-restore",
	})
	_, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Errorf("read on restored session should succeed, got: %v", err)
	}
}

func TestTmuxPersistOnKill(t *testing.T) {
	// Verifies that killing a session removes it from the persisted state, ensuring the session index stays in sync with actual session existence.
	sock := tmuxIsolatedSocket(t)

	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	_, tool, _ := NewTmuxTool(300, 30, nil, idx, "test-agent", false, 30, 0, sock)
	t.Parallel()

	name := "foci-test-persistkill"

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
	raw, err := idx.GetAgentMetadata("test-agent", "tmux_owned")
	if err != nil || raw == "" {
		t.Fatal("owned sessions not persisted after start")
	}
	var owned map[string]string
	if err := json.Unmarshal([]byte(raw), &owned); err != nil {
		t.Fatalf("unmarshal owned: %v", err)
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
	raw, err = idx.GetAgentMetadata("test-agent", "tmux_owned")
	if err != nil || raw == "" {
		t.Fatal("owned sessions key should still exist")
	}
	var ownedAfter map[string]string
	if err := json.Unmarshal([]byte(raw), &ownedAfter); err != nil {
		t.Fatalf("unmarshal owned after: %v", err)
	}
	if len(ownedAfter) != 0 {
		t.Errorf("persisted sessions after kill = %v, want empty", ownedAfter)
	}
}

func TestTmuxPersistClearedOnStaleSessions(t *testing.T) {
	// Verifies that listing sessions clears stale entries from persisted state when the corresponding tmux sessions no longer exist.
	sock := tmuxIsolatedSocket(t)

	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Pre-populate state with sessions that no longer exist
	ownedJSON, _ := json.Marshal(map[string]string{"foci-test-stale1": "", "foci-test-stale2": ""})
	if err := idx.SetAgentMetadata("test-agent", "tmux_owned", string(ownedJSON)); err != nil {
		t.Fatalf("set state: %v", err)
	}

	_, tool, _ := NewTmuxTool(300, 30, nil, idx, "test-agent", false, 30, 0, sock)
	t.Parallel()

	// List should detect stale sessions and clear persisted state
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "list",
	})
	_, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	// Verify persisted state was cleared
	raw, err := idx.GetAgentMetadata("test-agent", "tmux_owned")
	if err != nil || raw == "" {
		t.Fatal("owned sessions key should still exist")
	}
	var owned map[string]string
	if err := json.Unmarshal([]byte(raw), &owned); err != nil {
		t.Fatalf("unmarshal owned: %v", err)
	}
	if len(owned) != 0 {
		t.Errorf("persisted sessions after list = %v, want empty", owned)
	}
}

func TestTmuxNoSessionIndex(t *testing.T) {
	// Verifies that the tool operates correctly when no session index is configured, allowing stateless use without persistence.
	t.Parallel()
	sock := tmuxIsolatedSocket(t)

	// Create tool without session index (nil)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, sock)

	name := "foci-test-nostate"

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

func TestTmuxStateFileRoundTrip(t *testing.T) {
	// Verifies end-to-end persistence: a session started by one instance is accessible from a new instance that reads from the same DB.
	t.Parallel()
	sock := tmuxIsolatedSocket(t)

	dbPath := filepath.Join(t.TempDir(), "state.db")

	// First instance: start session and persist
	idx1, err := session.NewSessionIndex(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	_, tool1, _ := NewTmuxTool(300, 30, nil, idx1, "test-agent", false, 30, 0, sock)

	name := "foci-test-roundtrip"

	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
	})
	if _, err := tool1.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Verify metadata was written
	raw, err := idx1.GetAgentMetadata("test-agent", "tmux_owned")
	if err != nil || raw == "" {
		t.Fatal("session index does not contain tmux_owned metadata")
	}
	if !strings.Contains(raw, name) {
		t.Errorf("metadata does not contain session name %q: %s", name, raw)
	}

	// Second instance: open same DB and verify session is accessible
	idx2, err := session.NewSessionIndex(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	_, tool2, _ := NewTmuxTool(300, 30, nil, idx2, "test-agent", false, 30, 0, sock)

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
	// Verifies that adding a watch writes the session name and threshold to the persistent session index, enabling watch restoration after restart.
	t.Parallel()
	sock := tmuxIsolatedSocket(t)

	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
	_, tool, _ := NewTmuxTool(300, 30, notifier, idx, "test-agent", false, 30, 0, sock)

	name := "foci-test-persist-watch"

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
	rawW, errW := idx.GetAgentMetadata("test-agent", "tmux_watches")
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
	// Verifies that a new tool instance restores watches from pre-populated state, resuming monitoring for sessions that survived the restart.
	t.Parallel()
	sock := tmuxIsolatedSocket(t)

	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	name := "foci-test-restore-watch"

	// Create the tmux session (simulating it still exists from before restart)
	if err := exec.Command("tmux", "-S", sock, "new-session", "-d", "-s", name, "sleep", "60").Run(); err != nil {
		t.Fatalf("create tmux session: %v", err)
	}

	// Pre-populate state with owned session and watch
	ownedJSON, _ := json.Marshal(map[string]string{name: ""})
	if err := idx.SetAgentMetadata("test-agent", "tmux_owned", string(ownedJSON)); err != nil {
		t.Fatalf("set owned state: %v", err)
	}
	watchesJSON, _ := json.Marshal([]persistedWatch{
		{Session: name, Window: 0, ThresholdSecs: 30, AgentSessionKey: "test-session"},
	})
	if err := idx.SetAgentMetadata("test-agent", "tmux_watches", string(watchesJSON)); err != nil {
		t.Fatalf("set watch state: %v", err)
	}

	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
	_, _, cleanup := NewTmuxTool(300, 30, notifier, idx, "test-agent", false, 30, 0, sock)

	// Verify the watch was restored by checking the state is still persisted
	// (if the session was alive, it stays in the map; if stale, it gets cleaned)
	rawW, errW := idx.GetAgentMetadata("test-agent", "tmux_watches")
	if errW != nil || rawW == "" {
		t.Fatal("watches should still be in state")
	}
	var watches []persistedWatch
	if err := json.Unmarshal([]byte(rawW), &watches); err != nil {
		t.Fatalf("unmarshal watches: %v", err)
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
	// Verifies that watches for sessions that no longer exist are silently dropped during restore, keeping the persisted state consistent with reality.
	t.Parallel()
	sock := tmuxIsolatedSocket(t)

	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Pre-populate with a watch for a non-existent session
	watchesJSON, _ := json.Marshal([]persistedWatch{
		{Session: "foci-test-stale-watch-xyz", Window: 0, ThresholdSecs: 30, AgentSessionKey: "test-session"},
	})
	if err := idx.SetAgentMetadata("test-agent", "tmux_watches", string(watchesJSON)); err != nil {
		t.Fatalf("set watch state: %v", err)
	}

	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
	NewTmuxTool(300, 30, notifier, idx, "test-agent", false, 30, 0, sock)

	// Stale watch should have been cleaned from state
	rawW, errW := idx.GetAgentMetadata("test-agent", "tmux_watches")
	if errW != nil || rawW == "" {
		t.Fatal("watches key should still exist")
	}
	var watches []persistedWatch
	if err := json.Unmarshal([]byte(rawW), &watches); err != nil {
		t.Fatalf("unmarshal watches: %v", err)
	}
	if len(watches) != 0 {
		t.Errorf("persisted watches after stale cleanup = %d, want 0", len(watches))
	}
}

func TestTmuxUnwatchPersists(t *testing.T) {
	// Verifies that calling unwatch removes the watch entry from the session index, so the watch is not erroneously restored after a restart.
	t.Parallel()
	sock := tmuxIsolatedSocket(t)

	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
	_, tool, _ := NewTmuxTool(300, 30, notifier, idx, "test-agent", false, 30, 0, sock)

	name := "foci-test-unwatch-persist"

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
	rawW, errW := idx.GetAgentMetadata("test-agent", "tmux_watches")
	if errW != nil || rawW == "" {
		t.Fatal("watch should be persisted")
	}
	var watches []persistedWatch
	if err := json.Unmarshal([]byte(rawW), &watches); err != nil {
		t.Fatalf("unmarshal watches: %v", err)
	}
	if len(watches) != 1 {
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
	rawW, errW = idx.GetAgentMetadata("test-agent", "tmux_watches")
	if errW != nil || rawW == "" {
		t.Fatal("watches key should still exist")
	}
	if err := json.Unmarshal([]byte(rawW), &watches); err != nil {
		t.Fatalf("unmarshal watches: %v", err)
	}
	if len(watches) != 0 {
		t.Errorf("persisted watches after unwatch = %d, want 0", len(watches))
	}
}

func TestTmuxClearAllPersistsWatches(t *testing.T) {
	// Verifies that the ClearAll cleanup function removes all watches from the session index, ensuring no stale watches survive shutdown.
	t.Parallel()
	sock := tmuxIsolatedSocket(t)

	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
	_, tool, cleanup := NewTmuxTool(300, 30, notifier, idx, "test-agent", false, 30, 0, sock)

	name := "foci-test-clearall-watch"

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
	rawW, errW := idx.GetAgentMetadata("test-agent", "tmux_watches")
	if errW != nil || rawW == "" {
		t.Fatal("watch should be persisted before ClearAll")
	}
	var watches []persistedWatch
	if err := json.Unmarshal([]byte(rawW), &watches); err != nil {
		t.Fatalf("unmarshal watches: %v", err)
	}
	if len(watches) != 1 {
		t.Fatal("watch should be persisted before ClearAll")
	}

	// ClearAll
	cleanup()

	// Verify watches state is cleared
	rawW, errW = idx.GetAgentMetadata("test-agent", "tmux_watches")
	if errW != nil || rawW == "" {
		t.Fatal("watches key should still exist")
	}
	if err := json.Unmarshal([]byte(rawW), &watches); err != nil {
		t.Fatalf("unmarshal watches: %v", err)
	}
	if len(watches) != 0 {
		t.Errorf("persisted watches after ClearAll = %d, want 0", len(watches))
	}
}

func TestTmuxOwnsExactKey(t *testing.T) {
	// Proves that owns() matches on the exact session key: the key that started
	// a session can read, send to, and kill it. Session keys are stable
	// identities — there is no rotation, so exact match is the ownership rule.
	sock := tmuxIsolatedSocket(t)

	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, sock)
	t.Parallel()

	name := "foci-test-owns-exact"

	key := "agent1/c123"
	ctx := tools.WithSessionKey(context.Background(), key)

	// Start session
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
	})
	if _, err := tool.Execute(ctx, params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Read with the same key should succeed
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      name,
	})
	if _, err := tool.Execute(ctx, params); err != nil {
		t.Errorf("read with owning key should succeed: %v", err)
	}

	// Send with the same key should succeed
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      name,
		"keys":      "echo hello",
	})
	if _, err := tool.Execute(ctx, params); err != nil {
		t.Errorf("send with owning key should succeed: %v", err)
	}

	// Kill with the same key should succeed
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "kill",
		"name":      name,
	})
	if _, err := tool.Execute(ctx, params); err != nil {
		t.Errorf("kill with owning key should succeed: %v", err)
	}
}

func TestTmuxListExcludesOtherKeys(t *testing.T) {
	// Proves that list only shows sessions owned by the caller's exact session
	// key: a session started under one key is invisible to list under a
	// different key, and visible under its own key.
	t.Parallel()
	sock := tmuxIsolatedSocket(t)

	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, sock)

	name := "foci-test-list-exclude"

	ownerKey := "agent1/c456"
	otherKey := "agent1/c457"
	ctxOwner := tools.WithSessionKey(context.Background(), ownerKey)

	// Start session under ownerKey
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
		"command":   "sleep 60",
	})
	if _, err := tool.Execute(ctxOwner, params); err != nil {
		t.Fatalf("start: %v", err)
	}

	// List with a different key should not show the session
	ctxOther := tools.WithSessionKey(context.Background(), otherKey)
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "list",
	})
	result, err := tool.Execute(ctxOther, params)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(result.Text, name) {
		t.Errorf("list under a different key should not show session %s, got: %s", name, result.Text)
	}

	// List with the owning key should show it
	result, err = tool.Execute(ctxOwner, params)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(result.Text, name) {
		t.Errorf("list under owning key should show session %s, got: %s", name, result.Text)
	}
}

func TestTmuxUnwatchNotRestoredOnRestart(t *testing.T) {
	// Verifies that after unwatching and restarting, the session is not restored into the new instance's watch set, confirming the unwatch is durable.
	t.Parallel()
	sock := tmuxIsolatedSocket(t)

	dbPath := filepath.Join(t.TempDir(), "state.db")
	idx, err := session.NewSessionIndex(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	notifier := tools.NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {})
	_, tool1, cleanup1 := NewTmuxTool(300, 30, notifier, idx, "test-agent", false, 30, 0, sock)
	defer cleanup1()

	name := "foci-test-unwatch-restart"

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
	rawW, errW := idx.GetAgentMetadata("test-agent", "tmux_watches")
	if errW != nil || rawW == "" {
		t.Fatal("watch should be persisted")
	}
	var watches []persistedWatch
	if err := json.Unmarshal([]byte(rawW), &watches); err != nil {
		t.Fatalf("unmarshal watches: %v", err)
	}
	if len(watches) != 1 {
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

	// Simulate restart: create a new tool instance from the same DB
	idx2, err := session.NewSessionIndex(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	_, tool2, cleanup2 := NewTmuxTool(300, 30, notifier, idx2, "test-agent", false, 30, 0, sock)
	defer cleanup2()

	// The unwatched session should NOT be restored — verify by trying to unwatch
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "unwatch",
		"name":      name,
	})
	_, err = tool2.Execute(context.Background(), params)
	if err == nil {
		t.Error("expected error unwatching session that should not have been restored")
	}

	// Verify no watches in persisted state
	rawW2, _ := idx2.GetAgentMetadata("test-agent", "tmux_watches")
	if rawW2 != "" {
		var watches2 []persistedWatch
		if err := json.Unmarshal([]byte(rawW2), &watches2); err == nil && len(watches2) != 0 {
			t.Errorf("watches in state after restart = %d, want 0", len(watches2))
		}
	}
}
