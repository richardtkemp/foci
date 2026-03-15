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

// TestModelKeyboardOptionsFiltersUnconfiguredEndpoints proves that /model only shows
// aliases whose resolved endpoint exists in the config, hiding models that would fail.
func TestModelKeyboardOptionsFiltersUnconfiguredEndpoints(t *testing.T) {
	ag := &agent.Agent{}
	aliases := map[string]string{
		"opus":     "anthropic/claude-opus-4-6",
		"sonnet":   "anthropic/claude-sonnet-4-6",
		"haiku":    "anthropic/claude-haiku-4-5-20251001",
		"gemini-flash": "google/gemini-2.5-flash",
		"deepseek":     "deepseek/deepseek-chat",
	}
	// Only anthropic endpoint is configured.
	cc := CommandContext{
		Agent:        ag,
		AgentConfig:  config.AgentConfig{},
		ModelAliases: aliases,
		Config: &config.Config{
			Endpoints: map[string]config.EndpointConfig{
				"anthropic": {Format: "anthropic", APIKey: "anthropic.api_key"},
			},
		},
	}
	cmd := ModelCommand()
	opts := cmd.KeyboardOptions(context.Background(), cc)

	got := make(map[string]bool)
	for _, o := range opts {
		got[o.Label] = true
	}
	// Anthropic aliases should appear.
	for _, want := range []string{"opus", "sonnet", "haiku"} {
		if !got[want] {
			t.Errorf("expected alias %q in keyboard options", want)
		}
	}
	// Non-anthropic aliases should be filtered out.
	for _, unwanted := range []string{"gemini-flash", "deepseek"} {
		if got[unwanted] {
			t.Errorf("alias %q should not appear (endpoint not configured)", unwanted)
		}
	}
}

// TestModelKeyboardOptionsAllEndpoints proves that when multiple endpoints are
// configured, aliases for all of them appear.
func TestModelKeyboardOptionsAllEndpoints(t *testing.T) {
	ag := &agent.Agent{}
	aliases := map[string]string{
		"opus":  "anthropic/claude-opus-4-6",
		"gemini-flash": "google/gemini-2.5-flash",
	}
	cc := CommandContext{
		Agent:        ag,
		AgentConfig:  config.AgentConfig{},
		ModelAliases: aliases,
		Config: &config.Config{
			Endpoints: map[string]config.EndpointConfig{
				"anthropic": {Format: "anthropic"},
				"gemini":    {Format: "gemini"},
			},
		},
	}
	cmd := ModelCommand()
	opts := cmd.KeyboardOptions(context.Background(), cc)
	if len(opts) != 2 {
		t.Fatalf("expected 2 options, got %d", len(opts))
	}
}

// TestModelKeyboardOptionsNoAliases proves that when no aliases are configured,
// nil is returned instead of hardcoded defaults.
func TestModelKeyboardOptionsNoAliases(t *testing.T) {
	ag := &agent.Agent{}
	cc := CommandContext{
		Agent:        ag,
		AgentConfig:  config.AgentConfig{},
		Config:       &config.Config{},
		ModelAliases: nil,
	}
	cmd := ModelCommand()
	opts := cmd.KeyboardOptions(context.Background(), cc)
	if opts != nil {
		t.Errorf("expected nil options when no aliases, got %d", len(opts))
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
	// table subcommand (returns Parts, not Text)
	result, _ = cmd.Execute(context.Background(), Request{Args: "table"}, cc)
	if len(result.Parts) == 0 {
		t.Error("table result should have parts")
	}
	// available subcommand
	result, _ = cmd.Execute(context.Background(), Request{Args: "available"}, cc)
	if result.Text == "" {
		t.Error("available result should not be empty")
	}
}

// TestSpeedCommand verifies speed mode can be set to fast/standard by name or number and cleared.
func TestSpeedCommand(t *testing.T) {
	ag := &agent.Agent{Model: "anthropic/claude-opus-4-6"}
	sk := "test-session"
	cc := modelCC(ag, nil)
	cmd := SpeedCommand()

	// Show when standard (default)
	result, _ := cmd.Execute(context.Background(), Request{SessionKey: sk}, cc)
	if !strings.Contains(result.Text, "standard") {
		t.Errorf("expected 'standard', got %q", result.Text)
	}

	// Set to fast
	result, _ = cmd.Execute(context.Background(), Request{Args: "fast", SessionKey: sk}, cc)
	if ag.SessionSpeed(sk) != "fast" {
		t.Errorf("speed not set to fast: %q", ag.SessionSpeed(sk))
	}
	if !strings.Contains(result.Text, "fast") {
		t.Errorf("result = %q", result.Text)
	}

	// Set via numeric alias
	_, _ = cmd.Execute(context.Background(), Request{Args: "0", SessionKey: sk}, cc)
	if ag.SessionSpeed(sk) != "" {
		t.Errorf("speed not cleared via '0': %q", ag.SessionSpeed(sk))
	}

	_, _ = cmd.Execute(context.Background(), Request{Args: "1", SessionKey: sk}, cc)
	if ag.SessionSpeed(sk) != "fast" {
		t.Errorf("speed not set via '1': %q", ag.SessionSpeed(sk))
	}

	// Show when set
	result, _ = cmd.Execute(context.Background(), Request{SessionKey: sk}, cc)
	if !strings.Contains(result.Text, "fast") {
		t.Errorf("expected 'fast', got %q", result.Text)
	}

	// Clear via "standard"
	result, _ = cmd.Execute(context.Background(), Request{Args: "standard", SessionKey: sk}, cc)
	if ag.SessionSpeed(sk) != "" {
		t.Errorf("speed not cleared: %q", ag.SessionSpeed(sk))
	}
	if !strings.Contains(result.Text, "standard") {
		t.Errorf("result = %q", result.Text)
	}

	// Invalid value
	ag.SetSessionSpeed(sk, "fast")
	result, _ = cmd.Execute(context.Background(), Request{Args: "turbo", SessionKey: sk}, cc)
	if !strings.Contains(result.Text, "Invalid") {
		t.Errorf("expected 'Invalid', got %q", result.Text)
	}
	if ag.SessionSpeed(sk) != "fast" {
		t.Errorf("speed changed on invalid input: %q", ag.SessionSpeed(sk))
	}
}

// TestSpeedCommandUnsupportedModel proves that /speed returns an error when the model doesn't support fast mode.
func TestSpeedCommandUnsupportedModel(t *testing.T) {
	ag := &agent.Agent{Model: "anthropic/claude-haiku-4-5-20251001"}
	sk := "test-session"
	cc := modelCC(ag, nil)
	cmd := SpeedCommand()

	result, _ := cmd.Execute(context.Background(), Request{Args: "fast", SessionKey: sk}, cc)
	if !strings.Contains(result.Text, "not supported") {
		t.Errorf("expected unsupported error, got %q", result.Text)
	}
	if ag.SessionSpeed(sk) != "" {
		t.Errorf("speed should not be set: %q", ag.SessionSpeed(sk))
	}
}

// TestSpeedCommandVisibility proves that the Visible callback returns false for haiku and true for opus.
func TestSpeedCommandVisibility(t *testing.T) {
	ag := &agent.Agent{}
	sk := "test-session"
	cc := modelCC(ag, nil)
	cmd := SpeedCommand()

	if cmd.Visible == nil {
		t.Fatal("Visible should be set")
	}
	ctx := context.Background()

	ag.SetSessionModel(sk, "anthropic/claude-haiku-4-5-20251001", "", "", nil)
	if cmd.Visible(ctx, Request{SessionKey: sk}, cc) {
		t.Error("Visible should return false for haiku")
	}

	ag.SetSessionModel(sk, "anthropic/claude-opus-4-6", "", "", nil)
	if !cmd.Visible(ctx, Request{SessionKey: sk}, cc) {
		t.Error("Visible should return true for opus")
	}

	ag.SetSessionModel(sk, "anthropic/claude-sonnet-4-6", "", "", nil)
	if cmd.Visible(ctx, Request{SessionKey: sk}, cc) {
		t.Error("Visible should return false for sonnet")
	}
}

// TestEffortCommandVisibility proves that the Visible callback returns false for haiku and true for sonnet/opus.
func TestEffortCommandVisibility(t *testing.T) {
	ag := &agent.Agent{}
	sk := "test-session"
	cc := modelCC(ag, nil)
	cmd := EffortCommand()

	if cmd.Visible == nil {
		t.Fatal("Visible should be set")
	}
	ctx := context.Background()

	ag.SetSessionModel(sk, "anthropic/claude-haiku-4-5-20251001", "", "", nil)
	if cmd.Visible(ctx, Request{SessionKey: sk}, cc) {
		t.Error("Visible should return false for haiku")
	}

	ag.SetSessionModel(sk, "anthropic/claude-sonnet-4-6", "", "", nil)
	if !cmd.Visible(ctx, Request{SessionKey: sk}, cc) {
		t.Error("Visible should return true for sonnet")
	}
}

// TestThinkingCommandVisibility proves that the Visible callback returns false for haiku and true for sonnet/opus.
func TestThinkingCommandVisibility(t *testing.T) {
	ag := &agent.Agent{}
	sk := "test-session"
	cc := modelCC(ag, nil)
	cmd := ThinkingCommand()

	if cmd.Visible == nil {
		t.Fatal("Visible should be set")
	}
	ctx := context.Background()

	ag.SetSessionModel(sk, "anthropic/claude-haiku-4-5-20251001", "", "", nil)
	if cmd.Visible(ctx, Request{SessionKey: sk}, cc) {
		t.Error("Visible should return false for haiku")
	}

	ag.SetSessionModel(sk, "anthropic/claude-opus-4-6", "", "", nil)
	if !cmd.Visible(ctx, Request{SessionKey: sk}, cc) {
		t.Error("Visible should return true for opus")
	}
}
