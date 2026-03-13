package command

import (
	"context"
	"strings"
	"testing"
)

func TestModelCommand(t *testing.T) {
	// Verifies model can be switched between options and short names are resolved.
	model := "claude-haiku-4-5"
	resolveModel := func(input string) (string, string, string) {
		switch strings.ToLower(strings.TrimSpace(input)) {
		case "opus":
			return "", "claude-opus-4-6", ""
		case "sonnet", "":
			return "", "claude-sonnet-4-6", ""
		case "haiku":
			return "", "claude-haiku-4-5", ""
		default:
			return "", input, ""
		}
	}
	cmd := NewModelCommand(
		func(context.Context) string { return model },
		func(_ context.Context, _ string, m string, _ string) { model = m },
		resolveModel,
		nil,
	)

	// Show current
	result, _ := cmd.Execute(context.Background(), "")
	if !strings.Contains(result, "claude-haiku-4-5") {
		t.Errorf("result = %q", result)
	}

	// Switch
	result, _ = cmd.Execute(context.Background(), "claude-opus-4-6")
	if model != "claude-opus-4-6" {
		t.Errorf("model not switched: %s", model)
	}
	if !strings.Contains(result, "claude-opus-4-6") {
		t.Errorf("result = %q", result)
	}

	// Switch with short name
	result, _ = cmd.Execute(context.Background(), "haiku")
	if model != "claude-haiku-4-5" {
		t.Errorf("short name not resolved: got %q, want %q", model, "claude-haiku-4-5")
	}
	if !strings.Contains(result, "claude-haiku-4-5") {
		t.Errorf("result = %q", result)
	}

	_, _ = cmd.Execute(context.Background(), "opus")
	if model != "claude-opus-4-6" {
		t.Errorf("short name not resolved: got %q, want %q", model, "claude-opus-4-6")
	}

	_, _ = cmd.Execute(context.Background(), "sonnet")
	if model != "claude-sonnet-4-6" {
		t.Errorf("short name not resolved: got %q, want %q", model, "claude-sonnet-4-6")
	}
}

func TestEffortCommand(t *testing.T) {
	// Verifies effort levels can be set by name or number and persisted.
	effort := ""
	cmd := NewEffortCommand(
		func(context.Context) string { return effort },
		func(_ context.Context, e string) { effort = e },
	)

	// Show when not set
	result, _ := cmd.Execute(context.Background(), "")
	if !strings.Contains(result, "not set") {
		t.Errorf("expected 'not set', got %q", result)
	}
	if !strings.Contains(result, "1) low") {
		t.Errorf("expected numbered options, got %q", result)
	}

	// Set valid levels by name
	for _, level := range []string{"low", "medium", "high"} {
		result, _ = cmd.Execute(context.Background(), level)
		if effort != level {
			t.Errorf("effort not set to %s: %s", level, effort)
		}
		if !strings.Contains(result, level) {
			t.Errorf("result = %q", result)
		}
	}

	// Set valid levels by number
	for num, level := range map[string]string{"1": "low", "2": "medium", "3": "high"} {
		result, _ = cmd.Execute(context.Background(), num)
		if effort != level {
			t.Errorf("/effort %s: expected %s, got %s", num, level, effort)
		}
		if !strings.Contains(result, level) {
			t.Errorf("result = %q", result)
		}
	}

	// Show when set
	effort = "high"
	result, _ = cmd.Execute(context.Background(), "")
	if !strings.Contains(result, "high") {
		t.Errorf("expected 'high', got %q", result)
	}
	if !strings.Contains(result, "1) low") {
		t.Errorf("expected numbered options when set, got %q", result)
	}

	// Invalid level
	result, _ = cmd.Execute(context.Background(), "turbo")
	if !strings.Contains(result, "Invalid") {
		t.Errorf("expected 'Invalid', got %q", result)
	}
	if !strings.Contains(result, "1) low") {
		t.Errorf("expected options in error message, got %q", result)
	}
	if effort != "high" {
		t.Errorf("effort changed on invalid input: %s", effort)
	}

	// Clear
	result, _ = cmd.Execute(context.Background(), "none")
	if effort != "" {
		t.Errorf("effort not cleared: %q", effort)
	}
	if !strings.Contains(result, "cleared") {
		t.Errorf("result = %q", result)
	}
}

func TestThinkingCommand(t *testing.T) {
	// Verifies thinking mode can be toggled between off, adaptive, and extended.
	thinking := ""
	cmd := NewThinkingCommand(
		func(context.Context) string { return thinking },
		func(_ context.Context, t string) { thinking = t },
	)

	// Show when off (default)
	result, _ := cmd.Execute(context.Background(), "")
	if !strings.Contains(result, "off") {
		t.Errorf("expected 'off', got %q", result)
	}

	// Set to adaptive
	result, _ = cmd.Execute(context.Background(), "adaptive")
	if thinking != "adaptive" {
		t.Errorf("thinking not set to adaptive: %q", thinking)
	}
	if !strings.Contains(result, "adaptive") {
		t.Errorf("result = %q", result)
	}

	// Set via numeric alias
	_, _ = cmd.Execute(context.Background(), "0")
	if thinking != "off" {
		t.Errorf("thinking not set to 'off' via '0': %q", thinking)
	}

	_, _ = cmd.Execute(context.Background(), "1")
	if thinking != "adaptive" {
		t.Errorf("thinking not set via '1': %q", thinking)
	}

	// Show when set
	result, _ = cmd.Execute(context.Background(), "")
	if !strings.Contains(result, "adaptive") {
		t.Errorf("expected 'adaptive', got %q", result)
	}

	// Turn off
	result, _ = cmd.Execute(context.Background(), "off")
	if thinking != "off" {
		t.Errorf("thinking not set to 'off': %q", thinking)
	}
	if !strings.Contains(result, "off") {
		t.Errorf("result = %q", result)
	}

	// Invalid value
	thinking = "adaptive"
	result, _ = cmd.Execute(context.Background(), "turbo")
	if !strings.Contains(result, "Invalid") {
		t.Errorf("expected 'Invalid', got %q", result)
	}
	if thinking != "adaptive" {
		t.Errorf("thinking changed on invalid input: %q", thinking)
	}
}

func TestThinkingCommandContextRouting(t *testing.T) {
	// Verifies the callback receives context so callers can resolve per-session state.
	// Verify the callback receives context so callers can resolve per-session state.
	// This tests the fix for bug #134 — Telegram commands need the ChatIDKey
	// from context to resolve the correct session key.
	var lastCtx context.Context
	cmd := NewThinkingCommand(
		func(ctx context.Context) string { lastCtx = ctx; return "" },
		func(ctx context.Context, _ string) { lastCtx = ctx },
	)

	// Simulate Telegram dispatch: context carries ChatIDKey
	ctx := context.WithValue(context.Background(), ChatIDKey{}, int64(99887766))
	cmd.Execute(ctx, "adaptive")

	// The callback should have received the context with ChatIDKey
	chatID, ok := lastCtx.Value(ChatIDKey{}).(int64)
	if !ok || chatID != 99887766 {
		t.Errorf("callback context ChatIDKey = %d, want 99887766", chatID)
	}
}

func TestConfigCommand(t *testing.T) {
	// Verifies config subcommands delegate correctly.
	cmd := NewConfigCommand(func(ctx context.Context, args string) (string, error) {
		switch args {
		case "toml":
			return "toml output", nil
		case "table":
			return "table output", nil
		case "available":
			return "available output", nil
		default:
			return "usage text", nil
		}
	}, nil, nil)
	// No args → usage
	result, _ := cmd.Execute(context.Background(), "")
	if result != "usage text" {
		t.Errorf("default result = %q, want usage text", result)
	}
	// toml subcommand
	result, _ = cmd.Execute(context.Background(), "toml")
	if result != "toml output" {
		t.Errorf("toml result = %q", result)
	}
	// table subcommand
	result, _ = cmd.Execute(context.Background(), "table")
	if result != "table output" {
		t.Errorf("table result = %q", result)
	}
	// available subcommand
	result, _ = cmd.Execute(context.Background(), "available")
	if result != "available output" {
		t.Errorf("available result = %q", result)
	}
}
