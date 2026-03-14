package telegram

import (
	"testing"

	"foci/internal/config"
)

func ptr[T any](v T) *T { return &v }

func TestApplyAgentDisplaySettings_AgentOverridesGlobal(t *testing.T) {
	// Verifies that per-agent
	// display settings take precedence over global defaults.
	bot := NewBotForTest()
	acfg := config.AgentConfig{
		ShowToolCalls: ptr(config.ToolCallFull),
		ShowThinking:  ptr(config.ShowThinkingCompact),
		MessagesInLog: ptr(true),
		Platforms: &config.PlatformsConfig{Telegram: &config.TelegramPlatformConfig{
			DisplayWidth:     ptr(80),
			ReceivedFilesDir: "/agent/files",
		}},
	}
	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			ShowToolCalls:    ptr(config.ToolCallOff),
			ShowThinking:     ptr(config.ShowThinkingOff),
			DisplayWidth:     ptr(44),
			ReceivedFilesDir: "/global/files",
		},
		Logging: config.LoggingConfig{
			MessagesInLog: false,
		},
	}

	ApplyAgentDisplaySettings(bot, acfg, cfg)

	stc, st, dw, mil, rfd, _ := bot.DisplaySettings()
	if stc != "full" {
		t.Errorf("ShowToolCalls = %q, want %q", stc, "full")
	}
	if st != "compact" {
		t.Errorf("ShowThinking = %q, want %q", st, "compact")
	}
	if dw != 80 {
		t.Errorf("DisplayWidth = %d, want 80", dw)
	}
	if !mil {
		t.Error("MessagesInLog = false, want true")
	}
	if rfd != "/agent/files" {
		t.Errorf("ReceivedFilesDir = %q, want %q", rfd, "/agent/files")
	}
}

func TestApplyAgentDisplaySettings_FallsBackToDefaults(t *testing.T) {
	// Verifies that when no
	// agent-level settings are set, global defaults are used.
	bot := NewBotForTest()
	acfg := config.AgentConfig{} // all nil/zero — should fall back to telegram
	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			ShowToolCalls:    ptr(config.ToolCallPreview),
			ShowThinking:     ptr(config.ShowThinkingTrue),
			DisplayWidth:     ptr(60),
			ReceivedFilesDir: "/global/files",
		},
		Logging: config.LoggingConfig{
			MessagesInLog: true,
		},
	}

	ApplyAgentDisplaySettings(bot, acfg, cfg)

	stc, st, dw, mil, rfd, _ := bot.DisplaySettings()
	if stc != "preview" {
		t.Errorf("ShowToolCalls = %q, want %q (telegram fallback)", stc, "preview")
	}
	if st != "true" {
		t.Errorf("ShowThinking = %q, want %q (telegram fallback)", st, "true")
	}
	if dw != 60 {
		t.Errorf("DisplayWidth = %d, want 60 (telegram fallback)", dw)
	}
	if !mil {
		t.Error("MessagesInLog = false, want true (global fallback)")
	}
	if rfd != "/global/files" {
		t.Errorf("ReceivedFilesDir = %q, want %q (global fallback)", rfd, "/global/files")
	}
}

func TestApplyAgentDisplaySettings_ReceivedFilesDirBothEmpty(t *testing.T) {
	// Verifies that a
	// pre-existing ReceivedFilesDir is not overwritten when both agent and global are empty.
	bot := NewBotForTest()
	// Pre-set a value to verify it's NOT overwritten when both are empty
	bot.SetReceivedFilesDir("/pre-existing")

	acfg := config.AgentConfig{}
	cfg := &config.Config{
		Telegram: config.TelegramConfig{ReceivedFilesDir: ""},
	}

	ApplyAgentDisplaySettings(bot, acfg, cfg)

	_, _, _, _, rfd, _ := bot.DisplaySettings()
	if rfd != "/pre-existing" {
		t.Errorf("ReceivedFilesDir = %q, want %q (should not be overwritten when both empty)", rfd, "/pre-existing")
	}
}

// TestDisplayOverrideFn_UsesTurnSessionKey verifies that the display override
// closure reads the turn-scoped session key rather than the bot's default
// SessionKey(). This ensures multi-chat bots resolve overrides for the chat
// that is actually being served, not the first chat that ever messaged.
func TestDisplayOverrideFn_UsesTurnSessionKey(t *testing.T) {
	bot := NewBotForTest()
	bot.SetShowToolCalls("off") // bot default

	// Simulate a display override function that returns "full" for sk-turn
	// and nothing for any other key.
	bot.SetDisplayOverrideFn(func() DisplayOverrides {
		sk := bot.turnSessionKey
		if sk == "sk-turn" {
			return DisplayOverrides{ShowToolCalls: "full"}
		}
		return DisplayOverrides{}
	})

	// Without turnSessionKey set, should fall back to bot default.
	if got := bot.effectiveShowToolCalls(); got != "off" {
		t.Errorf("without turnSessionKey: got %q, want %q", got, "off")
	}

	// With turnSessionKey set, should resolve the override.
	bot.turnSessionKey = "sk-turn"
	if got := bot.effectiveShowToolCalls(); got != "full" {
		t.Errorf("with turnSessionKey: got %q, want %q", got, "full")
	}

	// Clear it, should go back to default.
	bot.turnSessionKey = ""
	if got := bot.effectiveShowToolCalls(); got != "off" {
		t.Errorf("after clearing turnSessionKey: got %q, want %q", got, "off")
	}
}

func TestApplyAgentDisplaySettings_PartialOverride(t *testing.T) {
	// Verifies that partial agent
	// overrides work correctly with defaults filling the gaps.
	bot := NewBotForTest()
	// Only override ShowToolCalls; rest falls back to telegram
	acfg := config.AgentConfig{
		ShowToolCalls: ptr(config.ToolCallFull),
	}
	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			ShowToolCalls: ptr(config.ToolCallOff),
			ShowThinking:  ptr(config.ShowThinkingCompact),
			DisplayWidth:  ptr(44),
		},
		Logging: config.LoggingConfig{
			MessagesInLog: true,
		},
	}

	ApplyAgentDisplaySettings(bot, acfg, cfg)

	stc, st, dw, mil, _, _ := bot.DisplaySettings()
	if stc != "full" {
		t.Errorf("ShowToolCalls = %q, want %q (agent override)", stc, "full")
	}
	if st != "compact" {
		t.Errorf("ShowThinking = %q, want %q (telegram fallback)", st, "compact")
	}
	if dw != 44 {
		t.Errorf("DisplayWidth = %d, want 44 (telegram fallback)", dw)
	}
	if !mil {
		t.Error("MessagesInLog = false, want true (global fallback)")
	}
}
