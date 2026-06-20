package command

import (
	"context"
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/modelcaps"
	"foci/internal/tools"
)

// TestNewSessionSettingCommandShowEmpty verifies that the factory-generated
// Execute shows EmptyShow when the getter returns "" and no args are provided.
func TestNewSessionSettingCommandShowEmpty(t *testing.T) {
	var stored string
	cmd := newSessionSettingCommand(sessionSettingDef{
		Name:        "test",
		Description: "test setting",
		OptionsHint: "Options: a, b",
		EmptyShow:   "not configured",
		InvalidName: "test value",
		Get:         func(_ CommandContext, _ string) string { return stored },
		Set:         func(_ CommandContext, _ string, v string) { stored = v },
		Choices: []settingChoice{
			{Label: "a", SetValue: "a", Response: "Set: a"},
			{Label: "b", SetValue: "b", Response: "Set: b"},
		},
	})

	resp, err := cmd.Execute(context.Background(), Request{}, CommandContext{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "Test: not configured\nOptions: a, b" {
		t.Errorf("unexpected show text: %q", resp.Text)
	}
}

// TestNewSessionSettingCommandShowDefault verifies that the factory uses
// DefaultShow when the getter returns "" or matches DefaultShow.
func TestNewSessionSettingCommandShowDefault(t *testing.T) {
	var stored string
	cmd := newSessionSettingCommand(sessionSettingDef{
		Name:        "mode",
		Description: "test mode",
		OptionsHint: "Options: on, off",
		DefaultShow: "off",
		InvalidName: "mode",
		Get:         func(_ CommandContext, _ string) string { return stored },
		Set:         func(_ CommandContext, _ string, v string) { stored = v },
		Choices: []settingChoice{
			{Label: "off", SetValue: "off", Response: "Mode: off"},
			{Label: "on", SetValue: "on", Response: "Mode: on"},
		},
	})

	// Empty value → shows default
	resp, _ := cmd.Execute(context.Background(), Request{}, CommandContext{})
	if resp.Text != "Mode: off\nOptions: on, off" {
		t.Errorf("unexpected show text for empty: %q", resp.Text)
	}

	// Value matches default → shows default
	stored = "off"
	resp, _ = cmd.Execute(context.Background(), Request{}, CommandContext{})
	if resp.Text != "Mode: off\nOptions: on, off" {
		t.Errorf("unexpected show text for default match: %q", resp.Text)
	}
}

// TestNewSessionSettingCommandSetAndInvalid verifies that valid inputs set the
// value and invalid inputs return an error with the options hint.
func TestNewSessionSettingCommandSetAndInvalid(t *testing.T) {
	var stored string
	cmd := newSessionSettingCommand(sessionSettingDef{
		Name:        "level",
		Description: "test level",
		OptionsHint: "Options: 1) low  2) high",
		EmptyShow:   "unset",
		InvalidName: "level",
		Get:         func(_ CommandContext, _ string) string { return stored },
		Set:         func(_ CommandContext, _ string, v string) { stored = v },
		Choices: []settingChoice{
			{Label: "low", Aliases: []string{"1"}, SetValue: "low", Response: "Level: low"},
			{Label: "high", Aliases: []string{"2"}, SetValue: "high", Response: "Level: high"},
		},
	})

	// Set via label
	resp, _ := cmd.Execute(context.Background(), Request{Args: "low"}, CommandContext{})
	if stored != "low" || resp.Text != "Level: low" {
		t.Errorf("set low: stored=%q resp=%q", stored, resp.Text)
	}

	// Set via numeric alias
	resp, _ = cmd.Execute(context.Background(), Request{Args: "2"}, CommandContext{})
	if stored != "high" || resp.Text != "Level: high" {
		t.Errorf("set 2→high: stored=%q resp=%q", stored, resp.Text)
	}

	// Invalid
	resp, _ = cmd.Execute(context.Background(), Request{Args: "turbo"}, CommandContext{})
	if stored != "high" {
		t.Errorf("stored changed on invalid: %q", stored)
	}
	if resp.Text != "Invalid level: \"turbo\"\nOptions: 1) low  2) high" {
		t.Errorf("unexpected error text: %q", resp.Text)
	}
}

// TestNewSessionSettingCommandGateExecute verifies that GateExecute rejects
// when the capability check fails, even though Visible would also hide it.
func TestNewSessionSettingCommandGateExecute(t *testing.T) {
	ag := &agent.Agent{Model: "anthropic/claude-haiku-4-5-20251001"}
	sk := "test-session"
	cc := modelCC(ag)

	cmd := newSessionSettingCommand(sessionSettingDef{
		Name:        "speed",
		Description: "test speed",
		OptionsHint: "Options: 0) standard  1) fast",
		Capability:  func(c config.ModelCaps) bool { return c.Speed },
		GateExecute: true,
		GateMsg:     "Speed is not supported by %s",
		DefaultShow: "standard",
		InvalidName: "speed mode",
		Get:         func(cc CommandContext, sk string) string { return cc.Agent.SessionSpeed(sk) },
		Set:         func(cc CommandContext, sk, v string) { cc.Agent.SetSessionSpeed(sk, v) },
		Choices: []settingChoice{
			{Label: "standard", SetValue: "", Response: "Speed: standard"},
			{Label: "fast", Aliases: []string{"1"}, SetValue: "fast", Response: "Speed: fast"},
		},
	})

	resp, _ := cmd.Execute(context.Background(), Request{Args: "fast", SessionKey: sk}, cc)
	if ag.SessionSpeed(sk) != "" {
		t.Errorf("speed should not be set, gate should reject: %q", ag.SessionSpeed(sk))
	}
	if resp.Text != "Speed is not supported by anthropic/claude-haiku-4-5-20251001" {
		t.Errorf("unexpected gate response: %q", resp.Text)
	}
}

// TestNewSessionSettingCommandHiddenChoice verifies that choices with Hidden=true
// are accepted as input but don't appear in keyboard options.
func TestNewSessionSettingCommandHiddenChoice(t *testing.T) {
	var stored string
	cmd := newSessionSettingCommand(sessionSettingDef{
		Name:        "test",
		Description: "test",
		OptionsHint: "Options: a, b",
		EmptyShow:   "none",
		InvalidName: "test",
		Get:         func(_ CommandContext, _ string) string { return stored },
		Set:         func(_ CommandContext, _ string, v string) { stored = v },
		Choices: []settingChoice{
			{Label: "a", SetValue: "a", Response: "Set: a"},
			{Label: "b", SetValue: "b", Response: "Set: b"},
			{Label: "clear", SetValue: "", Response: "Cleared", Hidden: true},
		},
	})

	// Hidden choice works as input
	resp, _ := cmd.Execute(context.Background(), Request{Args: "clear"}, CommandContext{})
	if resp.Text != "Cleared" {
		t.Errorf("hidden choice not accepted: %q", resp.Text)
	}

	// But doesn't appear in keyboard
	opts := cmd.KeyboardOptions(context.Background(), CommandContext{})
	if len(opts) != 2 {
		t.Errorf("expected 2 keyboard options (hidden excluded), got %d", len(opts))
	}
	for _, o := range opts {
		if o.Label == "clear" {
			t.Error("hidden choice should not appear in keyboard")
		}
	}
}

// TestNewSessionSettingCommandVisibility verifies that the Visible callback
// delegates to the capability check and falls back to model config defaults.
func TestNewSessionSettingCommandVisibility(t *testing.T) {
	ag := &agent.Agent{}
	sk := "test-session"
	cc := modelCC(ag)

	cmd := newSessionSettingCommand(sessionSettingDef{
		Name:         "test",
		Description:  "test",
		OptionsHint:  "",
		Capability:   func(c config.ModelCaps) bool { return c.Effort },
		ModelDefault: func(md config.ModelDefaults) string { return md.Effort },
		InvalidName:  "test",
		Get:          func(cc CommandContext, sk string) string { return cc.Agent.SessionEffort(sk) },
		Set:          func(cc CommandContext, sk, v string) { cc.Agent.SetSessionEffort(sk, v) },
		Choices:      []settingChoice{{Label: "a", SetValue: "a", Response: "a"}},
	})

	if cmd.Visible == nil {
		t.Fatal("Visible should be set when Capability is provided")
	}
	skCtx := tools.WithSessionKey(context.Background(), sk)

	// No capability, no model default → not visible
	ag.SetSessionModel(sk, "anthropic/claude-haiku-4-5-20251001", "", "", nil)
	if cmd.Visible(skCtx, Request{}, cc) {
		t.Error("should not be visible for haiku (no effort support)")
	}

	// Has capability → visible
	ag.SetSessionModel(sk, "anthropic/claude-sonnet-4-6", "", "", nil)
	if !cmd.Visible(skCtx, Request{}, cc) {
		t.Error("should be visible for sonnet (has effort support)")
	}

	// No capability but model config has effort → visible
	ag.ModelDefaultsFn = func(model string) config.ModelDefaults {
		if model == "openrouter/qwen/qwen3.5-397b-a17b" {
			return config.ModelDefaults{Effort: "high"}
		}
		return config.ModelDefaults{}
	}
	ag.SetSessionModel(sk, "openrouter/qwen/qwen3.5-397b-a17b", "", "", nil)
	if !cmd.Visible(skCtx, Request{}, cc) {
		t.Error("should be visible when model config has effort set")
	}
}

// TestNewSessionSettingCommandShowModelDefault verifies that the display shows
// the effective value from model config when no session override is set.
func TestNewSessionSettingCommandShowModelDefault(t *testing.T) {
	ag := &agent.Agent{
		Model: "openrouter/qwen/qwen3.5-397b-a17b",
		ModelDefaultsFn: func(model string) config.ModelDefaults {
			if model == "openrouter/qwen/qwen3.5-397b-a17b" {
				return config.ModelDefaults{Effort: "high"}
			}
			return config.ModelDefaults{}
		},
	}
	sk := "test-session"
	cc := modelCC(ag)

	cmd := newSessionSettingCommand(sessionSettingDef{
		Name:         "effort",
		Description:  "test effort",
		OptionsHint:  "Options: 1) low  2) medium  3) high",
		ModelDefault: func(md config.ModelDefaults) string { return md.Effort },
		EmptyShow:    "not set",
		InvalidName:  "effort level",
		Get:          func(cc CommandContext, sk string) string { return cc.Agent.SessionEffort(sk) },
		Set:          func(cc CommandContext, sk, v string) { cc.Agent.SetSessionEffort(sk, v) },
		Choices: []settingChoice{
			{Label: "low", Aliases: []string{"1"}, SetValue: "low", Response: "Effort set to: low"},
			{Label: "high", Aliases: []string{"3"}, SetValue: "high", Response: "Effort set to: high"},
		},
	})

	skCtx := tools.WithSessionKey(context.Background(), sk)

	// No session override → shows model default with annotation
	resp, err := cmd.Execute(skCtx, Request{}, cc)
	if err != nil {
		t.Fatal(err)
	}
	want := "Effort: high (model default)\nOptions: 1) low  2) medium  3) high"
	if resp.Text != want {
		t.Errorf("show model default:\ngot  %q\nwant %q", resp.Text, want)
	}

	// Session override takes precedence
	ag.SetSessionEffort(sk, "low")
	resp, _ = cmd.Execute(skCtx, Request{}, cc)
	want = "Effort: low\nOptions: 1) low  2) medium  3) high"
	if resp.Text != want {
		t.Errorf("show session override:\ngot  %q\nwant %q", resp.Text, want)
	}

	// After clearing, model default reappears
	ag.SetSessionEffort(sk, "")
	resp, _ = cmd.Execute(skCtx, Request{}, cc)
	want = "Effort: high (model default)\nOptions: 1) low  2) medium  3) high"
	if resp.Text != want {
		t.Errorf("show after clear:\ngot  %q\nwant %q", resp.Text, want)
	}
}

// TestNewSessionSettingCommandKeyboardHeader verifies that KeyboardHeader returns
// the current effective value for display above the keyboard buttons.
func TestNewSessionSettingCommandKeyboardHeader(t *testing.T) {
	var stored string
	cmd := newSessionSettingCommand(sessionSettingDef{
		Name:        "effort",
		Description: "test effort",
		OptionsHint: "Options: 1) low  2) medium  3) high",
		EmptyShow:   "not set",
		InvalidName: "effort level",
		Get:         func(_ CommandContext, _ string) string { return stored },
		Set:         func(_ CommandContext, _ string, v string) { stored = v },
		Choices: []settingChoice{
			{Label: "low", Aliases: []string{"1"}, SetValue: "low", Response: "Effort set to: low"},
			{Label: "high", Aliases: []string{"3"}, SetValue: "high", Response: "Effort set to: high"},
		},
	})

	if cmd.KeyboardHeader == nil {
		t.Fatal("KeyboardHeader should be set by newSessionSettingCommand")
	}

	skCtx := tools.WithSessionKey(context.Background(), "test")
	cc := CommandContext{}
	req := Request{}

	// No value → shows empty display
	header := cmd.KeyboardHeader(skCtx, req, cc)
	if header != "/effort — Effort: not set" {
		t.Errorf("header with no value = %q", header)
	}

	// After setting a value → shows that value
	stored = "high"
	header = cmd.KeyboardHeader(skCtx, req, cc)
	if header != "/effort — Effort: high" {
		t.Errorf("header with value = %q", header)
	}
}

// TestNewSessionSettingCommandKeyboardHeaderModelDefault verifies that
// KeyboardHeader shows model defaults with annotation when no session override is set.
func TestNewSessionSettingCommandKeyboardHeaderModelDefault(t *testing.T) {
	ag := &agent.Agent{
		Model: "openrouter/qwen/qwen3.5-397b-a17b",
		ModelDefaultsFn: func(model string) config.ModelDefaults {
			if model == "openrouter/qwen/qwen3.5-397b-a17b" {
				return config.ModelDefaults{Effort: "high"}
			}
			return config.ModelDefaults{}
		},
	}
	sk := "test-session"
	cc := modelCC(ag)

	cmd := newSessionSettingCommand(sessionSettingDef{
		Name:         "effort",
		Description:  "test effort",
		OptionsHint:  "Options: 1) low  2) medium  3) high",
		ModelDefault: func(md config.ModelDefaults) string { return md.Effort },
		EmptyShow:    "not set",
		InvalidName:  "effort level",
		Get:          func(cc CommandContext, sk string) string { return cc.Agent.SessionEffort(sk) },
		Set:          func(cc CommandContext, sk, v string) { cc.Agent.SetSessionEffort(sk, v) },
		Choices: []settingChoice{
			{Label: "low", Aliases: []string{"1"}, SetValue: "low", Response: "Effort set to: low"},
			{Label: "high", Aliases: []string{"3"}, SetValue: "high", Response: "Effort set to: high"},
		},
	})

	skCtx := tools.WithSessionKey(context.Background(), sk)
	req := Request{}

	// No session override → shows model default with annotation
	header := cmd.KeyboardHeader(skCtx, req, cc)
	want := "/effort — Effort: high (model default)"
	if header != want {
		t.Errorf("header with model default:\ngot  %q\nwant %q", header, want)
	}

	// Session override takes precedence
	ag.SetSessionEffort(sk, "low")
	header = cmd.KeyboardHeader(skCtx, req, cc)
	want = "/effort — Effort: low"
	if header != want {
		t.Errorf("header with session override:\ngot  %q\nwant %q", header, want)
	}
}

// TestEffortDynamicChoicesFromCatalogue verifies that /effort sources its levels
// from the live model catalogue (modelcaps): a model advertising all five levels
// offers xhigh and max, the keyboard and numeric aliases follow catalogue order,
// the options hint stays in sync, and a model absent from the catalogue falls
// back to the static low/medium/high set. (#840)
func TestEffortDynamicChoicesFromCatalogue(t *testing.T) {
	// Seed the process-wide catalogue with an opus that supports all five levels.
	// Other models (e.g. sonnet) are deliberately absent → Lookup misses → static.
	// A bare Agent has no DelegatedManager → BackendType() == BackendAPI, so
	// seed the api backend's record.
	modelcaps.SetFetcher(modelcaps.BackendAPI, func(_ context.Context) (map[string]modelcaps.Caps, error) {
		return map[string]modelcaps.Caps{
			"claude-opus-4-8": {Effort: []string{"low", "medium", "high", "xhigh", "max"}},
		}, nil
	})
	if err := modelcaps.Refresh(context.Background(), modelcaps.BackendAPI); err != nil {
		t.Fatalf("seed catalogue: %v", err)
	}

	ag := &agent.Agent{}
	sk := "test-session"
	cc := modelCC(ag)
	skCtx := tools.WithSessionKey(context.Background(), sk)
	cmd := EffortCommand()

	// Model present in catalogue → dynamic levels.
	ag.SetSessionModel(sk, "anthropic/claude-opus-4-8", "", "", nil)

	opts := cmd.KeyboardOptions(skCtx, cc)
	gotLabels := make([]string, len(opts))
	for i, o := range opts {
		gotLabels[i] = o.Label
	}
	wantLabels := []string{"low", "medium", "high", "xhigh", "max"}
	if len(gotLabels) != len(wantLabels) {
		t.Fatalf("keyboard labels = %v, want %v", gotLabels, wantLabels)
	}
	for i := range wantLabels {
		if gotLabels[i] != wantLabels[i] {
			t.Errorf("keyboard label[%d] = %q, want %q", i, gotLabels[i], wantLabels[i])
		}
	}

	// xhigh accepted by label.
	resp, _ := cmd.Execute(skCtx, Request{Args: "xhigh", SessionKey: sk}, cc)
	if ag.SessionEffort(sk) != "xhigh" || resp.Text != "Effort set to: xhigh" {
		t.Errorf("set xhigh: stored=%q resp=%q", ag.SessionEffort(sk), resp.Text)
	}

	// Numeric alias 5 → max (catalogue order).
	resp, _ = cmd.Execute(skCtx, Request{Args: "5", SessionKey: sk}, cc)
	if ag.SessionEffort(sk) != "max" || resp.Text != "Effort set to: max" {
		t.Errorf("set 5→max: stored=%q resp=%q", ag.SessionEffort(sk), resp.Text)
	}

	// No-args show carries the dynamic hint (all five levels).
	ag.SetSessionEffort(sk, "")
	resp, _ = cmd.Execute(skCtx, Request{SessionKey: sk}, cc)
	wantHint := "Options: 1) low  2) medium  3) high  4) xhigh  5) max"
	if want := "Effort: not set\n" + wantHint; resp.Text != want {
		t.Errorf("dynamic show:\ngot  %q\nwant %q", resp.Text, want)
	}

	// Model absent from catalogue → static fallback (low/medium/high only).
	ag.SetSessionModel(sk, "anthropic/claude-sonnet-4-6", "", "", nil)
	opts = cmd.KeyboardOptions(skCtx, cc)
	if len(opts) != 3 {
		t.Errorf("fallback keyboard = %d options, want 3 (low/medium/high)", len(opts))
	}
	// And the static hint returns.
	resp, _ = cmd.Execute(skCtx, Request{SessionKey: sk}, cc)
	if want := "Effort: not set\nOptions: 1) low  2) medium  3) high"; resp.Text != want {
		t.Errorf("fallback show:\ngot  %q\nwant %q", resp.Text, want)
	}
}

// TestNewSessionSettingCommandNilCapability verifies that when Capability is nil,
// the Visible callback is not set (command is always visible).
func TestNewSessionSettingCommandNilCapability(t *testing.T) {
	cmd := newSessionSettingCommand(sessionSettingDef{
		Name:        "test",
		Description: "test",
		OptionsHint: "",
		InvalidName: "test",
		Get:         func(_ CommandContext, _ string) string { return "" },
		Set:         func(_ CommandContext, _, _ string) {},
		Choices:     []settingChoice{{Label: "a", SetValue: "a", Response: "a"}},
	})

	if cmd.Visible != nil {
		t.Error("Visible should be nil when Capability is nil")
	}
}
