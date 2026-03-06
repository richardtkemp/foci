package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestTmuxSendRateLimit verifies that sends are rate-limited to ~2s apart.
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

// TestTmuxSessionKeyIsolation verifies that different session keys can't access each other's sessions.
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

// TestTmuxReapExpiredSessions verifies that expired sessions are cleaned up.
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

// TestTmuxReapPreservesActiveSession verifies that recently-accessed sessions survive reap.
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
