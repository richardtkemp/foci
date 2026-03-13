package command

import (
	"context"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
)

// modelCC returns a CommandContext with a real agent for model/effort/thinking tests.
func modelCC(ag *agent.Agent, aliases map[string]string) CommandContext {
	return CommandContext{
		Agent:        ag,
		AgentConfig:  config.AgentConfig{},
		Config:       &config.Config{},
		ModelAliases: aliases,
	}
}

// TestModelCommand verifies model can be switched between options and short names are resolved.
func TestModelCommand(t *testing.T) {
	ag := &agent.Agent{Model: "claude-haiku-4-5"}
	sk := "test-session"
	aliases := map[string]string{
		"opus":   "anthropic/claude-opus-4-6",
		"sonnet": "anthropic/claude-sonnet-4-6",
		"haiku":  "anthropic/claude-haiku-4-5",
	}
	cc := modelCC(ag, aliases)
	cmd := ModelCommand()

	// Show current
	result, _ := cmd.Execute(context.Background(), Request{SessionKey: sk}, cc)
	if !strings.Contains(result.Text, "claude-haiku-4-5") {
		t.Errorf("result = %q", result.Text)
	}

	// Switch with full model ID
	result, _ = cmd.Execute(context.Background(), Request{Args: "anthropic/claude-opus-4-6", SessionKey: sk}, cc)
	if !strings.Contains(result.Text, "claude-opus-4-6") {
		t.Errorf("result = %q", result.Text)
	}

	// Switch with short alias
	result, _ = cmd.Execute(context.Background(), Request{Args: "haiku", SessionKey: sk}, cc)
	if !strings.Contains(result.Text, "claude-haiku-4-5") {
		t.Errorf("result = %q", result.Text)
	}
}

// TestEffortCommand verifies effort levels can be set by name or number and persisted.
func TestEffortCommand(t *testing.T) {
	ag := &agent.Agent{}
	sk := "test-session"
	cc := modelCC(ag, nil)
	cmd := EffortCommand()

	// Show when not set
	result, _ := cmd.Execute(context.Background(), Request{SessionKey: sk}, cc)
	if !strings.Contains(result.Text, "not set") {
		t.Errorf("expected 'not set', got %q", result.Text)
	}
	if !strings.Contains(result.Text, "1) low") {
		t.Errorf("expected numbered options, got %q", result.Text)
	}

	// Set valid levels by name
	for _, level := range []string{"low", "medium", "high"} {
		result, _ = cmd.Execute(context.Background(), Request{Args: level, SessionKey: sk}, cc)
		got := ag.SessionEffort(sk)
		if got != level {
			t.Errorf("effort not set to %s: %s", level, got)
		}
		if !strings.Contains(result.Text, level) {
			t.Errorf("result = %q", result.Text)
		}
	}

	// Set valid levels by number
	for num, level := range map[string]string{"1": "low", "2": "medium", "3": "high"} {
		result, _ = cmd.Execute(context.Background(), Request{Args: num, SessionKey: sk}, cc)
		got := ag.SessionEffort(sk)
		if got != level {
			t.Errorf("/effort %s: expected %s, got %s", num, level, got)
		}
	}

	// Show when set
	ag.SetSessionEffort(sk, "high")
	result, _ = cmd.Execute(context.Background(), Request{SessionKey: sk}, cc)
	if !strings.Contains(result.Text, "high") {
		t.Errorf("expected 'high', got %q", result.Text)
	}

	// Invalid level
	result, _ = cmd.Execute(context.Background(), Request{Args: "turbo", SessionKey: sk}, cc)
	if !strings.Contains(result.Text, "Invalid") {
		t.Errorf("expected 'Invalid', got %q", result.Text)
	}

	// Clear
	result, _ = cmd.Execute(context.Background(), Request{Args: "none", SessionKey: sk}, cc)
	if !strings.Contains(result.Text, "cleared") {
		t.Errorf("result = %q", result.Text)
	}
}

// TestThinkingCommand verifies thinking mode can be toggled between off, adaptive, and extended.
func TestThinkingCommand(t *testing.T) {
	ag := &agent.Agent{}
	sk := "test-session"
	cc := modelCC(ag, nil)
	cmd := ThinkingCommand()

	// Show when off (default)
	result, _ := cmd.Execute(context.Background(), Request{SessionKey: sk}, cc)
	if !strings.Contains(result.Text, "off") {
		t.Errorf("expected 'off', got %q", result.Text)
	}

	// Set to adaptive
	result, _ = cmd.Execute(context.Background(), Request{Args: "adaptive", SessionKey: sk}, cc)
	if ag.SessionThinking(sk) != "adaptive" {
		t.Errorf("thinking not set to adaptive: %q", ag.SessionThinking(sk))
	}
	if !strings.Contains(result.Text, "adaptive") {
		t.Errorf("result = %q", result.Text)
	}

	// Set via numeric alias
	_, _ = cmd.Execute(context.Background(), Request{Args: "0", SessionKey: sk}, cc)
	if ag.SessionThinking(sk) != "off" {
		t.Errorf("thinking not set to 'off' via '0': %q", ag.SessionThinking(sk))
	}

	_, _ = cmd.Execute(context.Background(), Request{Args: "1", SessionKey: sk}, cc)
	if ag.SessionThinking(sk) != "adaptive" {
		t.Errorf("thinking not set via '1': %q", ag.SessionThinking(sk))
	}

	// Turn off
	result, _ = cmd.Execute(context.Background(), Request{Args: "off", SessionKey: sk}, cc)
	if ag.SessionThinking(sk) != "off" {
		t.Errorf("thinking not set to 'off': %q", ag.SessionThinking(sk))
	}
	if !strings.Contains(result.Text, "off") {
		t.Errorf("result = %q", result.Text)
	}

	// Invalid value
	ag.SetSessionThinking(sk, "adaptive")
	result, _ = cmd.Execute(context.Background(), Request{Args: "turbo", SessionKey: sk}, cc)
	if !strings.Contains(result.Text, "Invalid") {
		t.Errorf("expected 'Invalid', got %q", result.Text)
	}
	if ag.SessionThinking(sk) != "adaptive" {
		t.Errorf("thinking changed on invalid input: %q", ag.SessionThinking(sk))
	}
}

// TestThinkingCommandContextRouting verifies the request carries session key for per-session state.
func TestThinkingCommandContextRouting(t *testing.T) {
	ag := &agent.Agent{}
	sk := "test-session"
	cc := modelCC(ag, nil)
	cmd := ThinkingCommand()

	_, _ = cmd.Execute(context.Background(), Request{Args: "adaptive", SessionKey: sk}, cc)

	// The agent should have the thinking mode set for this session key
	if ag.SessionThinking(sk) != "adaptive" {
		t.Errorf("thinking not set for session %q: %q", sk, ag.SessionThinking(sk))
	}
}

// TestConfigCommand verifies config subcommands delegate correctly.
func TestConfigCommand(t *testing.T) {
	cc := CommandContext{
		Config:      &config.Config{},
		AgentConfig: config.AgentConfig{},
		ConfigPath:  "/tmp/foci.toml",
	}
	cmd := ConfigCommand()

	// No args → usage
	result, _ := cmd.Execute(context.Background(), Request{}, cc)
	if !strings.Contains(result.Text, "/config toml") {
		t.Errorf("expected usage text, got %q", result.Text)
	}
	// toml subcommand
	result, _ = cmd.Execute(context.Background(), Request{Args: "toml"}, cc)
	if result.Text == "" {
		t.Error("toml result should not be empty")
	}
	// table subcommand
	result, _ = cmd.Execute(context.Background(), Request{Args: "table"}, cc)
	if result.Text == "" {
		t.Error("table result should not be empty")
	}
	// available subcommand
	result, _ = cmd.Execute(context.Background(), Request{Args: "available"}, cc)
	if result.Text == "" {
		t.Error("available result should not be empty")
	}
}
