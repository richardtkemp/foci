package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTmuxInvalidOperation(t *testing.T) {
	// Verifies that unknown operations return a meaningful error rather than silently succeeding or panicking.
	t.Parallel()
	_, tool, _, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

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
	// Verifies that omitting the name parameter auto-generates a foci-prefixed session name rather than failing.
	t.Parallel()
	tmuxAvailable(t)
	_, tool, _, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"command":   "sleep 60",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(result.Text, "foci-") {
		t.Errorf("result = %q, want auto-generated foci-N name", result.Text)
	}

	// Extract name and clean up
	name := strings.TrimPrefix(result.Text, "Session started: ")
	tmuxSetup(t, name)
}

func TestTmuxSendNoEnter(t *testing.T) {
	// Verifies that keys can be sent without triggering Enter, leaving typed text in the input buffer without executing it.
	t.Parallel()
	tmuxAvailable(t)
	_, tool, _, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

	name := "foci-test-noenter"
	tmuxSetup(t, name)

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
	if result.Text != "Keys sent." {
		t.Errorf("result = %q", result.Text)
	}
}

func TestTmuxSendBareEnter(t *testing.T) {
	// Verifies that enter=true with no keys succeeds (sends just Enter), while enter=false with no keys correctly fails as an empty operation.
	t.Parallel()
	tmuxAvailable(t)
	_, tool, _, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

	name := "foci-test-bareenter"
	tmuxSetup(t, name)

	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Send bare Enter (no keys, enter=true)
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      name,
		"enter":     true,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("bare enter send should succeed: %v", err)
	}
	if result.Text != "Keys sent." {
		t.Errorf("result = %q, want %q", result.Text, "Keys sent.")
	}

	// Verify: no keys + no enter should fail
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      name,
		"enter":     false,
	})
	_, err = tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for empty keys with enter=false")
	}
}

func TestTmuxMissingName(t *testing.T) {
	// Verifies that send, read, and kill all fail with an error when no session name is supplied, covering required-parameter validation.
	t.Parallel()
	_, tool, _, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

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
