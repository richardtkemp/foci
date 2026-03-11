package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestTmuxInvalidOperation verifies that invalid operations are rejected.
func TestTmuxInvalidOperation(t *testing.T) {
	t.Parallel()
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

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

// TestTmuxStartNoName verifies that sessions can be started without explicit names (auto-generated).
func TestTmuxStartNoName(t *testing.T) {
	t.Parallel()
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

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

// TestTmuxSendNoEnter verifies that text can be sent without pressing Enter.
func TestTmuxSendNoEnter(t *testing.T) {
	t.Parallel()
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

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

// TestTmuxSendBareEnter verifies that bare Enter (without keys) works when enter=true.
func TestTmuxSendBareEnter(t *testing.T) {
	t.Parallel()
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

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

// TestTmuxMissingName verifies that operations without name are properly rejected.
func TestTmuxMissingName(t *testing.T) {
	t.Parallel()
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

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
