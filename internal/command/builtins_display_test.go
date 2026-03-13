package command

import (
	"context"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
)

// displayCC returns a minimal CommandContext for display command tests.
func displayCC() CommandContext {
	return CommandContext{
		Agent:       &agent.Agent{},
		AgentConfig: config.AgentConfig{},
		Config:      &config.Config{},
	}
}

// TestDisplayCommand_NoArgs verifies that /display with no arguments shows all
// current effective display settings with their override status.
func TestDisplayCommand_NoArgs(t *testing.T) {
	cmd := DisplayCommand()
	cc := displayCC()

	result, err := cmd.Execute(context.Background(), Request{}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Text, "show_tool_calls: off") {
		t.Errorf("missing show_tool_calls default, got:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "show_thinking: off") {
		t.Errorf("missing show_thinking default, got:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "stream_output: off") {
		t.Errorf("missing stream_output default, got:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "display_width: 44") {
		t.Errorf("missing display_width default, got:\n%s", result.Text)
	}
}

// TestDisplayCommand_SetShowToolCalls verifies setting show_tool_calls accepts
// valid values and rejects invalid ones.
func TestDisplayCommand_SetShowToolCalls(t *testing.T) {
	cmd := DisplayCommand()
	cc := displayCC()
	sk := "test-session"

	for _, val := range []string{"off", "preview", "full"} {
		result, err := cmd.Execute(context.Background(), Request{Args: "show_tool_calls " + val, SessionKey: sk}, cc)
		if err != nil {
			t.Errorf("show_tool_calls %s: unexpected error: %v", val, err)
		}
		if !strings.Contains(result.Text, val) {
			t.Errorf("show_tool_calls %s: result = %q", val, result.Text)
		}
	}

	// Invalid value
	_, err := cmd.Execute(context.Background(), Request{Args: "show_tool_calls invalid", SessionKey: sk}, cc)
	if err == nil {
		t.Error("expected error for invalid show_tool_calls value")
	}
}

// TestDisplayCommand_SetShowThinking verifies setting show_thinking accepts
// valid values (off, compact, true) and rejects invalid ones.
func TestDisplayCommand_SetShowThinking(t *testing.T) {
	cmd := DisplayCommand()
	cc := displayCC()
	sk := "test-session"

	for _, val := range []string{"off", "compact", "true"} {
		result, err := cmd.Execute(context.Background(), Request{Args: "show_thinking " + val, SessionKey: sk}, cc)
		if err != nil {
			t.Errorf("show_thinking %s: unexpected error: %v", val, err)
		}
		if !strings.Contains(result.Text, val) {
			t.Errorf("show_thinking %s: result = %q", val, result.Text)
		}
	}

	_, err := cmd.Execute(context.Background(), Request{Args: "show_thinking invalid", SessionKey: sk}, cc)
	if err == nil {
		t.Error("expected error for invalid show_thinking value")
	}
}

// TestDisplayCommand_SetStreamOutput verifies setting stream_output accepts
// on/off/true/false and normalises to "true"/"false".
func TestDisplayCommand_SetStreamOutput(t *testing.T) {
	cmd := DisplayCommand()
	cc := displayCC()
	sk := "test-session"

	result, err := cmd.Execute(context.Background(), Request{Args: "stream_output on", SessionKey: sk}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Text, "on") {
		t.Errorf("result = %q", result.Text)
	}

	_, err = cmd.Execute(context.Background(), Request{Args: "stream_output off", SessionKey: sk}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, _ = cmd.Execute(context.Background(), Request{Args: "stream_output true", SessionKey: sk}, cc)

	_, err = cmd.Execute(context.Background(), Request{Args: "stream_output invalid", SessionKey: sk}, cc)
	if err == nil {
		t.Error("expected error for invalid stream_output value")
	}
}

// TestDisplayCommand_SetDisplayWidth verifies setting display_width accepts
// valid numeric values (20–200) and rejects out-of-range or non-numeric input.
func TestDisplayCommand_SetDisplayWidth(t *testing.T) {
	cmd := DisplayCommand()
	cc := displayCC()
	sk := "test-session"

	result, err := cmd.Execute(context.Background(), Request{Args: "display_width 80", SessionKey: sk}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Text, "80") {
		t.Errorf("result = %q", result.Text)
	}

	_, err = cmd.Execute(context.Background(), Request{Args: "display_width 10", SessionKey: sk}, cc)
	if err == nil {
		t.Error("expected error for display_width < 20")
	}
	_, err = cmd.Execute(context.Background(), Request{Args: "display_width 300", SessionKey: sk}, cc)
	if err == nil {
		t.Error("expected error for display_width > 200")
	}
	_, err = cmd.Execute(context.Background(), Request{Args: "display_width abc", SessionKey: sk}, cc)
	if err == nil {
		t.Error("expected error for non-numeric display_width")
	}
}

// TestDisplayCommand_Reset verifies that /display reset clears all overrides
// and the response confirms the action.
func TestDisplayCommand_Reset(t *testing.T) {
	cmd := DisplayCommand()
	cc := displayCC()
	sk := "test-session"

	_, _ = cmd.Execute(context.Background(), Request{Args: "show_tool_calls full", SessionKey: sk}, cc)
	_, _ = cmd.Execute(context.Background(), Request{Args: "display_width 80", SessionKey: sk}, cc)

	result, err := cmd.Execute(context.Background(), Request{Args: "reset", SessionKey: sk}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Text, "cleared") {
		t.Errorf("result = %q", result.Text)
	}

	// After reset, show_tool_calls should be back to default
	status, _ := cmd.Execute(context.Background(), Request{SessionKey: sk}, cc)
	if strings.Contains(status.Text, "(override)") {
		t.Errorf("overrides not cleared:\n%s", status.Text)
	}
}

// TestDisplayCommand_ShowSingleKey verifies that /display <key> shows the
// current value for that specific key and reports override status.
func TestDisplayCommand_ShowSingleKey(t *testing.T) {
	cmd := DisplayCommand()
	cc := displayCC()
	sk := "test-session"

	// Show without override
	result, err := cmd.Execute(context.Background(), Request{Args: "show_tool_calls", SessionKey: sk}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(result.Text, "override") {
		t.Errorf("should not show override marker: %q", result.Text)
	}

	// Set override, then show
	_, _ = cmd.Execute(context.Background(), Request{Args: "show_tool_calls full", SessionKey: sk}, cc)
	result, err = cmd.Execute(context.Background(), Request{Args: "show_tool_calls", SessionKey: sk}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Text, "override") {
		t.Errorf("should show override marker: %q", result.Text)
	}
	if !strings.Contains(result.Text, "full") {
		t.Errorf("should show override value: %q", result.Text)
	}
}

// TestDisplayCommand_OverrideMarkerInStatus verifies that /display (no args)
// marks overridden values with "(override)" in the full status output.
func TestDisplayCommand_OverrideMarkerInStatus(t *testing.T) {
	cmd := DisplayCommand()
	cc := displayCC()
	sk := "test-session"

	_, _ = cmd.Execute(context.Background(), Request{Args: "show_tool_calls preview", SessionKey: sk}, cc)

	result, _ := cmd.Execute(context.Background(), Request{SessionKey: sk}, cc)
	if !strings.Contains(result.Text, "show_tool_calls: preview (override)") {
		t.Errorf("expected override marker, got:\n%s", result.Text)
	}
	if strings.Contains(result.Text, "display_width: 44 (override)") {
		t.Errorf("non-overridden key marked as override:\n%s", result.Text)
	}
}

// TestDisplayCommand_UnknownKey verifies that /display with an unknown key
// returns an error listing valid keys.
func TestDisplayCommand_UnknownKey(t *testing.T) {
	cmd := DisplayCommand()
	cc := displayCC()

	_, err := cmd.Execute(context.Background(), Request{Args: "unknown_key value"}, cc)
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if !strings.Contains(err.Error(), "unknown display key") {
		t.Errorf("error = %q", err)
	}

	_, err = cmd.Execute(context.Background(), Request{Args: "bogus"}, cc)
	if err == nil {
		t.Fatal("expected error for unknown key get")
	}
}

// TestDisplayCommand_StreamAlias verifies that "stream" is an alias for "stream_output".
func TestDisplayCommand_StreamAlias(t *testing.T) {
	cmd := DisplayCommand()
	cc := displayCC()
	sk := "test-session"

	result, err := cmd.Execute(context.Background(), Request{Args: "stream on", SessionKey: sk}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Text, "on") {
		t.Errorf("result = %q", result.Text)
	}
}

// TestDisplayCommand_WidthAlias verifies that "width" is an alias for "display_width".
func TestDisplayCommand_WidthAlias(t *testing.T) {
	cmd := DisplayCommand()
	cc := displayCC()
	sk := "test-session"

	result, err := cmd.Execute(context.Background(), Request{Args: "width 60", SessionKey: sk}, cc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Text, "60") {
		t.Errorf("result = %q", result.Text)
	}
}

// TestDisplayCommand_KeyboardOptions verifies the command returns keyboard
// options for each setting key plus reset.
func TestDisplayCommand_KeyboardOptions(t *testing.T) {
	cmd := DisplayCommand()
	cc := displayCC()
	opts := cmd.KeyboardOptions(context.Background(), cc)
	if len(opts) != 5 {
		t.Fatalf("expected 5 keyboard options, got %d", len(opts))
	}
	want := []string{"show_tool_calls", "show_thinking", "stream_output", "display_width", "reset"}
	for i, w := range want {
		if opts[i].Data != w {
			t.Errorf("option[%d].Data = %q, want %q", i, opts[i].Data, w)
		}
	}
}
