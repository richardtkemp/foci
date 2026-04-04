package ccstream

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/delegator"
)

func TestCheckAndSendSteers_DrainsPending(t *testing.T) {
	// Proves that checkAndSendSteers reads from the handler's SteerCheckFunc
	// and sends each steer as a "now"-priority user message with [user] prefix.
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer: NewWriter(nopWriteCloser{&buf}),
	}
	b.turnHandler = &delegator.EventHandler{
		SteerCheckFunc: func() []string {
			return []string{"stop that", "do this instead"}
		},
	}

	b.checkAndSendSteers()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2; output:\n%s", len(lines), buf.String())
	}

	for i, line := range lines {
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d: invalid JSON: %v", i, err)
		}

		if got["type"] != "user" {
			t.Errorf("line %d: type = %v, want %q", i, got["type"], "user")
		}
		if got["priority"] != PriorityNow {
			t.Errorf("line %d: priority = %v, want %q", i, got["priority"], PriorityNow)
		}

		msg, ok := got["message"].(map[string]any)
		if !ok {
			t.Fatalf("line %d: message is not an object", i)
		}
		content, _ := msg["content"].(string)
		if !strings.HasPrefix(content, "[user] ") {
			t.Errorf("line %d: content = %q, want [user] prefix", i, content)
		}
	}
}

func TestCheckAndSendSteers_NilHandler(t *testing.T) {
	// Verifies that checkAndSendSteers is a no-op when there's no handler
	// or no SteerCheckFunc.
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer: NewWriter(nopWriteCloser{&buf}),
	}

	// No handler at all.
	b.checkAndSendSteers()
	if buf.Len() != 0 {
		t.Errorf("expected no output with nil handler, got %q", buf.String())
	}

	// Handler without SteerCheckFunc.
	b.turnHandler = &delegator.EventHandler{}
	b.checkAndSendSteers()
	if buf.Len() != 0 {
		t.Errorf("expected no output with nil SteerCheckFunc, got %q", buf.String())
	}
}

func TestCheckAndSendSteers_EmptyDrain(t *testing.T) {
	// Verifies no messages are sent when SteerCheckFunc returns nil.
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer: NewWriter(nopWriteCloser{&buf}),
	}
	b.turnHandler = &delegator.EventHandler{
		SteerCheckFunc: func() []string { return nil },
	}

	b.checkAndSendSteers()
	if buf.Len() != 0 {
		t.Errorf("expected no output when steer returns nil, got %q", buf.String())
	}
}

func TestCheckAndSendSteers_SkipsEmptyStrings(t *testing.T) {
	// Verifies that empty steer strings are silently skipped.
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer: NewWriter(nopWriteCloser{&buf}),
	}
	b.turnHandler = &delegator.EventHandler{
		SteerCheckFunc: func() []string { return []string{"", "redirect", ""} },
	}

	b.checkAndSendSteers()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1 (empty strings skipped); output:\n%s", len(lines), buf.String())
	}
}
