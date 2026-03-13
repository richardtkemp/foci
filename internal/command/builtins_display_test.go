package command

import (
	"context"
	"strings"
	"testing"
)

// TestDisplayCommand_NoArgs verifies that /display with no arguments shows all
// current effective display settings with their override status.
func TestDisplayCommand_NoArgs(t *testing.T) {
	cmd := newTestDisplayCommand(nil)

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "show_tool_calls: off") {
		t.Errorf("missing show_tool_calls default, got:\n%s", result)
	}
	if !strings.Contains(result, "show_thinking: off") {
		t.Errorf("missing show_thinking default, got:\n%s", result)
	}
	if !strings.Contains(result, "stream_output: off") {
		t.Errorf("missing stream_output default, got:\n%s", result)
	}
	if !strings.Contains(result, "display_width: 44") {
		t.Errorf("missing display_width default, got:\n%s", result)
	}
}

// TestDisplayCommand_SetShowToolCalls verifies setting show_tool_calls accepts
// valid values and rejects invalid ones.
func TestDisplayCommand_SetShowToolCalls(t *testing.T) {
	overrides := make(map[string]string)
	cmd := newTestDisplayCommand(overrides)

	for _, val := range []string{"off", "preview", "full"} {
		result, err := cmd.Execute(context.Background(), "show_tool_calls "+val)
		if err != nil {
			t.Errorf("show_tool_calls %s: unexpected error: %v", val, err)
		}
		if !strings.Contains(result, val) {
			t.Errorf("show_tool_calls %s: result = %q", val, result)
		}
		if overrides["show_tool_calls"] != val {
			t.Errorf("override not stored: got %q, want %q", overrides["show_tool_calls"], val)
		}
	}

	// Invalid value
	_, err := cmd.Execute(context.Background(), "show_tool_calls invalid")
	if err == nil {
		t.Error("expected error for invalid show_tool_calls value")
	}
}

// TestDisplayCommand_SetShowThinking verifies setting show_thinking accepts
// valid values (off, compact, true) and rejects invalid ones.
func TestDisplayCommand_SetShowThinking(t *testing.T) {
	overrides := make(map[string]string)
	cmd := newTestDisplayCommand(overrides)

	for _, val := range []string{"off", "compact", "true"} {
		result, err := cmd.Execute(context.Background(), "show_thinking "+val)
		if err != nil {
			t.Errorf("show_thinking %s: unexpected error: %v", val, err)
		}
		if !strings.Contains(result, val) {
			t.Errorf("show_thinking %s: result = %q", val, result)
		}
		if overrides["show_thinking"] != val {
			t.Errorf("override not stored: got %q, want %q", overrides["show_thinking"], val)
		}
	}

	_, err := cmd.Execute(context.Background(), "show_thinking invalid")
	if err == nil {
		t.Error("expected error for invalid show_thinking value")
	}
}

// TestDisplayCommand_SetStreamOutput verifies setting stream_output accepts
// on/off/true/false and normalises to "true"/"false".
func TestDisplayCommand_SetStreamOutput(t *testing.T) {
	overrides := make(map[string]string)
	cmd := newTestDisplayCommand(overrides)

	// "on" normalises to stored value "true"
	result, err := cmd.Execute(context.Background(), "stream_output on")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "on") {
		t.Errorf("result = %q", result)
	}
	if overrides["stream_output"] != "true" {
		t.Errorf("override = %q, want \"true\"", overrides["stream_output"])
	}

	// "off" normalises to "false"
	_, err = cmd.Execute(context.Background(), "stream_output off")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrides["stream_output"] != "false" {
		t.Errorf("override = %q, want \"false\"", overrides["stream_output"])
	}

	// "true"/"false" also accepted
	_, _ = cmd.Execute(context.Background(), "stream_output true")
	if overrides["stream_output"] != "true" {
		t.Errorf("override = %q, want \"true\"", overrides["stream_output"])
	}

	_, err = cmd.Execute(context.Background(), "stream_output invalid")
	if err == nil {
		t.Error("expected error for invalid stream_output value")
	}
}

// TestDisplayCommand_SetDisplayWidth verifies setting display_width accepts
// valid numeric values (20–200) and rejects out-of-range or non-numeric input.
func TestDisplayCommand_SetDisplayWidth(t *testing.T) {
	overrides := make(map[string]string)
	cmd := newTestDisplayCommand(overrides)

	result, err := cmd.Execute(context.Background(), "display_width 80")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "80") {
		t.Errorf("result = %q", result)
	}
	if overrides["display_width"] != "80" {
		t.Errorf("override = %q, want \"80\"", overrides["display_width"])
	}

	// Out of range
	_, err = cmd.Execute(context.Background(), "display_width 10")
	if err == nil {
		t.Error("expected error for display_width < 20")
	}
	_, err = cmd.Execute(context.Background(), "display_width 300")
	if err == nil {
		t.Error("expected error for display_width > 200")
	}
	_, err = cmd.Execute(context.Background(), "display_width abc")
	if err == nil {
		t.Error("expected error for non-numeric display_width")
	}
}

// TestDisplayCommand_Reset verifies that /display reset clears all overrides
// and the response confirms the action.
func TestDisplayCommand_Reset(t *testing.T) {
	overrides := make(map[string]string)
	cmd := newTestDisplayCommand(overrides)

	// Set some overrides
	_, _ = cmd.Execute(context.Background(), "show_tool_calls full")
	_, _ = cmd.Execute(context.Background(), "display_width 80")
	if len(overrides) != 2 {
		t.Fatalf("expected 2 overrides before reset, got %d", len(overrides))
	}

	result, err := cmd.Execute(context.Background(), "reset")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "cleared") {
		t.Errorf("result = %q", result)
	}
	if len(overrides) != 0 {
		t.Errorf("overrides not cleared: %v", overrides)
	}
}

// TestDisplayCommand_ShowSingleKey verifies that /display <key> shows the
// current value for that specific key and reports override status.
func TestDisplayCommand_ShowSingleKey(t *testing.T) {
	overrides := make(map[string]string)
	cmd := newTestDisplayCommand(overrides)

	// Show without override
	result, err := cmd.Execute(context.Background(), "show_tool_calls")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(result, "override") {
		t.Errorf("should not show override marker: %q", result)
	}

	// Set override, then show
	_, _ = cmd.Execute(context.Background(), "show_tool_calls full")
	result, err = cmd.Execute(context.Background(), "show_tool_calls")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "override") {
		t.Errorf("should show override marker: %q", result)
	}
	if !strings.Contains(result, "full") {
		t.Errorf("should show override value: %q", result)
	}
}

// TestDisplayCommand_OverrideMarkerInStatus verifies that /display (no args)
// marks overridden values with "(override)" in the full status output.
func TestDisplayCommand_OverrideMarkerInStatus(t *testing.T) {
	overrides := make(map[string]string)
	cmd := newTestDisplayCommand(overrides)

	_, _ = cmd.Execute(context.Background(), "show_tool_calls preview")

	result, _ := cmd.Execute(context.Background(), "")
	if !strings.Contains(result, "show_tool_calls: preview (override)") {
		t.Errorf("expected override marker, got:\n%s", result)
	}
	// Non-overridden keys should NOT have "(override)"
	if strings.Contains(result, "display_width: 44 (override)") {
		t.Errorf("non-overridden key marked as override:\n%s", result)
	}
}

// TestDisplayCommand_UnknownKey verifies that /display with an unknown key
// returns an error listing valid keys.
func TestDisplayCommand_UnknownKey(t *testing.T) {
	cmd := newTestDisplayCommand(nil)

	_, err := cmd.Execute(context.Background(), "unknown_key value")
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if !strings.Contains(err.Error(), "unknown display key") {
		t.Errorf("error = %q", err)
	}

	// Get unknown key
	_, err = cmd.Execute(context.Background(), "bogus")
	if err == nil {
		t.Fatal("expected error for unknown key get")
	}
}

// TestDisplayCommand_StreamAlias verifies that "stream" is an alias for "stream_output".
func TestDisplayCommand_StreamAlias(t *testing.T) {
	overrides := make(map[string]string)
	cmd := newTestDisplayCommand(overrides)

	result, err := cmd.Execute(context.Background(), "stream on")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "on") {
		t.Errorf("result = %q", result)
	}
	if overrides["stream_output"] != "true" {
		t.Errorf("override = %q, want \"true\"", overrides["stream_output"])
	}
}

// TestDisplayCommand_WidthAlias verifies that "width" is an alias for "display_width".
func TestDisplayCommand_WidthAlias(t *testing.T) {
	overrides := make(map[string]string)
	cmd := newTestDisplayCommand(overrides)

	result, err := cmd.Execute(context.Background(), "width 60")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "60") {
		t.Errorf("result = %q", result)
	}
	if overrides["display_width"] != "60" {
		t.Errorf("override = %q, want \"60\"", overrides["display_width"])
	}
}

// TestDisplayCommand_StreamAliasGet verifies that "/display stream" (GET) works
// as an alias for "/display stream_output".
func TestDisplayCommand_StreamAliasGet(t *testing.T) {
	cmd := newTestDisplayCommand(nil)

	result, err := cmd.Execute(context.Background(), "stream")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "stream_output") {
		t.Errorf("expected canonical key in output, got %q", result)
	}
}

// TestDisplayCommand_WidthAliasGet verifies that "/display width" (GET) works
// as an alias for "/display display_width".
func TestDisplayCommand_WidthAliasGet(t *testing.T) {
	cmd := newTestDisplayCommand(nil)

	result, err := cmd.Execute(context.Background(), "width")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "display_width") {
		t.Errorf("expected canonical key in output, got %q", result)
	}
}

// TestDisplayCommand_KeyboardOptions verifies the command returns keyboard
// options for each setting key plus reset.
func TestDisplayCommand_KeyboardOptions(t *testing.T) {
	cmd := newTestDisplayCommand(nil)
	opts := cmd.KeyboardOptions(context.Background())
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

// newTestDisplayCommand creates a display command wired to in-memory maps for testing.
// overrides can be nil (no state tracking) or a map that accumulates set calls.
func newTestDisplayCommand(overrides map[string]string) *Command {
	if overrides == nil {
		overrides = make(map[string]string)
	}

	defaults := map[string]string{
		"show_tool_calls": "off",
		"show_thinking":   "off",
		"stream_output":   "off",
		"display_width":   "44",
	}

	makeGetter := func(key string) func(context.Context) (string, string) {
		return func(_ context.Context) (string, string) {
			if v, ok := overrides[key]; ok {
				effective := v
				if key == "stream_output" {
					if v == "true" {
						effective = "on"
					} else {
						effective = "off"
					}
				}
				return v, effective
			}
			return "", defaults[key]
		}
	}

	getters := DisplayGetters{
		ShowToolCalls: makeGetter("show_tool_calls"),
		ShowThinking:  makeGetter("show_thinking"),
		StreamOutput:  makeGetter("stream_output"),
		DisplayWidth:  makeGetter("display_width"),
	}

	setters := DisplaySetters{
		SetShowToolCalls: func(_ context.Context, v string) { overrides["show_tool_calls"] = v },
		SetShowThinking:  func(_ context.Context, v string) { overrides["show_thinking"] = v },
		SetStreamOutput:  func(_ context.Context, v string) { overrides["stream_output"] = v },
		SetDisplayWidth:  func(_ context.Context, v string) { overrides["display_width"] = v },
		ResetAll: func(_ context.Context) {
			for k := range overrides {
				delete(overrides, k)
			}
		},
	}

	return NewDisplayCommand(getters, setters)
}
