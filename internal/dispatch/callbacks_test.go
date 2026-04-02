package dispatch

import "testing"

// TestParseCallbackCommand verifies that the "cmd:" prefix is parsed as
// CallbackCommand with the command text extracted.
func TestParseCallbackCommand(t *testing.T) {
	action, data := ParseCallback("cmd:/status")
	if action != CallbackCommand {
		t.Errorf("expected CallbackCommand, got %d", action)
	}
	if data != "/status" {
		t.Errorf("expected /status, got %q", data)
	}
}

// TestParseCallbackCommandWithArgs verifies that arguments after the prefix
// are preserved in the extracted data.
func TestParseCallbackCommandWithArgs(t *testing.T) {
	action, data := ParseCallback("cmd:/model opus")
	if action != CallbackCommand {
		t.Errorf("expected CallbackCommand, got %d", action)
	}
	if data != "/model opus" {
		t.Errorf("expected '/model opus', got %q", data)
	}
}

// TestParseCallbackInteractive verifies that the "im:" prefix is parsed as
// CallbackInteractive with the payload extracted.
func TestParseCallbackInteractive(t *testing.T) {
	action, data := ParseCallback("im:abc123")
	if action != CallbackInteractive {
		t.Errorf("expected CallbackInteractive, got %d", action)
	}
	if data != "abc123" {
		t.Errorf("expected abc123, got %q", data)
	}
}

// TestParseCallbackToolCall verifies that the "tc:" prefix is parsed as
// CallbackToolCall with the tool call ID extracted.
func TestParseCallbackToolCall(t *testing.T) {
	action, data := ParseCallback("tc:tool_call_42")
	if action != CallbackToolCall {
		t.Errorf("expected CallbackToolCall, got %d", action)
	}
	if data != "tool_call_42" {
		t.Errorf("expected tool_call_42, got %q", data)
	}
}

// TestParseCallbackThinking verifies that the "th:" prefix is parsed as
// CallbackThinking with the thinking block ID extracted.
func TestParseCallbackThinking(t *testing.T) {
	action, data := ParseCallback("th:block_7")
	if action != CallbackThinking {
		t.Errorf("expected CallbackThinking, got %d", action)
	}
	if data != "block_7" {
		t.Errorf("expected block_7, got %q", data)
	}
}

// TestParseCallbackUnknownPrefix verifies that an unrecognized prefix returns
// CallbackUnknown with the original data string.
func TestParseCallbackUnknownPrefix(t *testing.T) {
	action, data := ParseCallback("xx:something")
	if action != CallbackUnknown {
		t.Errorf("expected CallbackUnknown, got %d", action)
	}
	if data != "xx:something" {
		t.Errorf("expected original data, got %q", data)
	}
}

// TestParseCallbackNoColon verifies that input without a colon separator
// returns CallbackUnknown with the raw input.
func TestParseCallbackNoColon(t *testing.T) {
	action, data := ParseCallback("noprefix")
	if action != CallbackUnknown {
		t.Errorf("expected CallbackUnknown, got %d", action)
	}
	if data != "noprefix" {
		t.Errorf("expected original data, got %q", data)
	}
}

// TestParseCallbackEmpty verifies that an empty string returns CallbackUnknown.
func TestParseCallbackEmpty(t *testing.T) {
	action, data := ParseCallback("")
	if action != CallbackUnknown {
		t.Errorf("expected CallbackUnknown, got %d", action)
	}
	if data != "" {
		t.Errorf("expected empty data, got %q", data)
	}
}

// TestParseCallbackEmptyPayload verifies that a prefix with an empty payload
// correctly returns the action with empty data (e.g. "cmd:" → CallbackCommand, "").
func TestParseCallbackEmptyPayload(t *testing.T) {
	tests := []struct {
		input  string
		action CallbackAction
	}{
		{"cmd:", CallbackCommand},
		{"im:", CallbackInteractive},
		{"tc:", CallbackToolCall},
		{"th:", CallbackThinking},
	}
	for _, tt := range tests {
		action, data := ParseCallback(tt.input)
		if action != tt.action {
			t.Errorf("ParseCallback(%q): expected action %d, got %d", tt.input, tt.action, action)
		}
		if data != "" {
			t.Errorf("ParseCallback(%q): expected empty data, got %q", tt.input, data)
		}
	}
}

// TestParseCallbackColonInPayload verifies that colons within the payload are
// preserved and not split further — only the first colon separates prefix from data.
func TestParseCallbackColonInPayload(t *testing.T) {
	action, data := ParseCallback("tc:key:value:extra")
	if action != CallbackToolCall {
		t.Errorf("expected CallbackToolCall, got %d", action)
	}
	if data != "key:value:extra" {
		t.Errorf("expected 'key:value:extra', got %q", data)
	}
}
