package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTmuxSendRateLimit(t *testing.T) {
	// Verifies that consecutive sends are rate-limited: the first send completes quickly but the second is delayed ~2s, enforcing the send rate limit.
	t.Parallel()
	tmuxAvailable(t)
	_, tool, _, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, "")

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
	// Verifies that session key isolation is enforced: one session context cannot read, send to, or kill sessions owned by a different context.
	t.Parallel()
	tmuxAvailable(t)

	// Single tool instance, two different session keys
	_, tool, _, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, "")

	nameA := "foci-test-skiso-a"
	nameB := "foci-test-skiso-b"
	tmuxSetup(t, nameA, nameB)

	ctxA := WithSessionKey(context.Background(), "test/c111/1000")
	ctxB := WithSessionKey(context.Background(), "test/c222/1000")

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

func TestTmuxReapExpiredSessions(t *testing.T) {
	// Verifies that the reaper removes sessions whose lastAccess time is past the TTL, killing both the internal tracking state and the actual tmux session.
	t.Parallel()
	tmuxAvailable(t)

	name := "foci-test-reap"
	tmuxSetup(t, name)

	inst := &tmuxInstance{
		watched:           make(map[string]*watchedSession),
		owned:             make(map[string]string),
		lastSend:          make(map[string]time.Time),
		lastAccess:        make(map[string]time.Time),
		sessionTTL:        100 * time.Millisecond,
		socketPath:        tmuxSocketPath,
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

func TestTmuxReapPreservesActiveSession(t *testing.T) {
	// Verifies that recently-accessed sessions are not reaped even when the reaper runs, distinguishing active from expired by TTL comparison.
	t.Parallel()
	tmuxAvailable(t)

	name := "foci-test-reap-active"
	tmuxSetup(t, name)

	inst := &tmuxInstance{
		watched:           make(map[string]*watchedSession),
		owned:             make(map[string]string),
		lastSend:          make(map[string]time.Time),
		lastAccess:        make(map[string]time.Time),
		sessionTTL:        1 * time.Hour,
		socketPath:        tmuxSocketPath,
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
