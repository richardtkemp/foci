package tmux

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestTmuxInvalidOperation(t *testing.T) {
	// Verifies that unknown operations return a meaningful error rather than silently succeeding or panicking.
	t.Parallel()
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, "")

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
	sock := tmuxIsolatedSocket(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, sock)

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
}

func TestTmuxSendNoEnter(t *testing.T) {
	// Verifies that keys can be sent without triggering Enter, leaving typed text in the input buffer without executing it.
	t.Parallel()
	sock := tmuxIsolatedSocket(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, sock)

	name := "foci-test-noenter"

	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

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
	sock := tmuxIsolatedSocket(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, sock)

	name := "foci-test-bareenter"

	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

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
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0, "")

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

func TestIsNoTmuxServer(t *testing.T) {
	// Verifies the classification of tmux's several "there is no server" phrasings,
	// which list() reads as "zero sessions" rather than as a failure.
	t.Parallel()
	for _, tc := range []struct {
		name string
		out  string
		want bool
	}{
		{"no server running", "no server running on /tmp/x.sock", true},
		{"missing socket file", "error connecting to /tmp/x.sock (No such file or directory)", true},
		{"no current session", "no current session", true},
		// Reached by an ordinary kill: killing the last session lets the server
		// be reaped, so the next list races the teardown. Missing this case made
		// TestTmuxKill flaky once the tests stopped sharing one server.
		{"server exited during teardown", "server exited unexpectedly", true},
		{"real error is not swallowed", "can't find session: nope", false},
		{"empty output", "", false},
	} {
		if got := isNoTmuxServer(tc.out); got != tc.want {
			t.Errorf("%s: isNoTmuxServer(%q) = %v, want %v", tc.name, tc.out, got, tc.want)
		}
	}
}
