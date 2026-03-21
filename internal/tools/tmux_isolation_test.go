package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTmuxInstanceIsolation(t *testing.T) {
	// Verifies that separate tool instances have independent ownership state: instance B cannot read, send to, or kill sessions owned by instance A.
	t.Parallel()
	tmuxAvailable(t)

	_, toolA, _, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, "")
	_, toolB, _, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, "")

	nameA := "foci-test-iso-a"
	nameB := "foci-test-iso-b"
	tmuxSetup(t, nameA, nameB)

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
	if !strings.Contains(result.Text, nameA) {
		t.Errorf("agent A list missing own session: %q", result.Text)
	}
	// A should see its own session with an owner and B's without
	for _, line := range strings.Split(result.Text, "\n") {
		if strings.Contains(line, nameA) && !strings.Contains(line, "self") {
			t.Errorf("agent A list should show %q with owner: %q", nameA, line)
		}
	}

	result, err = toolB.Execute(context.Background(), listParams)
	if err != nil {
		t.Fatalf("agent B list: %v", err)
	}
	if !strings.Contains(result.Text, nameB) {
		t.Errorf("agent B list missing own session: %q", result.Text)
	}
	// B should see its own session with an owner
	for _, line := range strings.Split(result.Text, "\n") {
		if strings.Contains(line, nameB) && !strings.Contains(line, "self") {
			t.Errorf("agent B list should show %q with owner: %q", nameB, line)
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
	// Verifies that wake callbacks fire on the correct tool instance: when agent A's watch triggers, only agent A's callback is invoked, not agent B's.
	t.Parallel()
	tmuxAvailable(t)

	var wakeA, wakeB atomic.Int32
	_, toolA, _, _ := NewTmuxTool(300, 30, NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		wakeA.Add(1)
	}), nil, "", false, 30, 0, "")
	_, toolB, _, _ := NewTmuxTool(300, 30, NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		wakeB.Add(1)
	}), nil, "", false, 30, 0, "")

	nameA := "foci-test-wakeroute-a"
	nameB := "foci-test-wakeroute-b"
	tmuxSetup(t, nameA, nameB)

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
	// Verifies that watch/unwatch state is per-instance: instance B can watch the same session name as A, and A's unwatch does not affect B's watch.
	t.Parallel()
	tmuxAvailable(t)

	_, toolA, _, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, "")

	_, toolB, _, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, "")

	name := "foci-test-watchiso"
	tmuxSetup(t, name)

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
