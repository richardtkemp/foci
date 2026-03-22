package telegram

import (
	"testing"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
)


// newBotForTest creates a Bot without connecting to the Telegram API.
func newBotForTest() *Bot {
	lg := log.NewComponentLogger("telegram:test")
	b := &Bot{
		log: lg,
	}
	b.mq = platform.NewMessageQueue(platform.MessageQueueConfig{
		Size:       64,
		TurnActive: b.isTurnActive,
		Logger:     lg,
	})
	return b
}

func TestApplyAgentDisplaySettings_AgentOverridesGlobal(t *testing.T) {
	// Verifies that per-agent display settings take precedence over global defaults.
	bot := newBotForTest()
	recvDir := "/agent/files"
	acfg := config.AgentConfig{
		Display: config.DisplayConfig{
			ShowToolCalls: config.Ptr(config.ToolCallFull),
			ShowThinking:  config.Ptr(config.ShowThinkingCompact),
		},
		Debug: config.DebugConfig{
			MessagesInLog: config.Ptr(true),
		},
		Platforms: []config.PlatformConfig{{
			ID: "telegram",
			Display: config.DisplayConfig{
				ShowToolCalls:    config.Ptr(config.ToolCallFull),
				ShowThinking:     config.Ptr(config.ShowThinkingCompact),
				DisplayWidth:     config.Ptr(80),
				ReceivedFilesDir: &recvDir,
			},
			Telegram: &config.TelegramSpecific{},
		}},
	}
	cfg := &config.Config{
		Platforms: []config.PlatformConfig{{
			ID: "telegram",
			Display: config.DisplayConfig{
				ShowToolCalls:    config.Ptr(config.ToolCallOff),
				ShowThinking:     config.Ptr(config.ShowThinkingOff),
				DisplayWidth:     config.Ptr(44),
				ReceivedFilesDir: config.Ptr("/global/files"),
			},
		}},
		Debug: config.DebugConfig{
			MessagesInLog: config.Ptr(false),
		},
	}

	rc := config.Resolve(cfg, acfg)
	ApplyAgentDisplaySettings(bot, rc.PlatformDisplay("telegram"), rc.Debug, acfg.Platform("telegram"))

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
	// Verifies that when no agent-level settings are set, platform defaults are used.
	bot := newBotForTest()
	// Agent has a telegram platform entry with merged values (as would happen
	// after ApplyProviderDefaults + ApplyDefaults).
	recvDir := "/global/files"
	acfg := config.AgentConfig{
		Platforms: []config.PlatformConfig{{
			ID: "telegram",
			Display: config.DisplayConfig{
				ShowToolCalls:    config.Ptr(config.ToolCallPreview),
				ShowThinking:     config.Ptr(config.ShowThinkingTrue),
				DisplayWidth:     config.Ptr(60),
				ReceivedFilesDir: &recvDir,
			},
			Telegram: &config.TelegramSpecific{},
		}},
	}
	cfg := &config.Config{
		Debug: config.DebugConfig{
			MessagesInLog: config.Ptr(true),
		},
	}

	rc := config.Resolve(cfg, acfg)
	ApplyAgentDisplaySettings(bot, rc.PlatformDisplay("telegram"), rc.Debug, acfg.Platform("telegram"))

	stc, st, dw, mil, rfd, _ := bot.DisplaySettings()
	if stc != "preview" {
		t.Errorf("ShowToolCalls = %q, want %q (platform fallback)", stc, "preview")
	}
	if st != "true" {
		t.Errorf("ShowThinking = %q, want %q (platform fallback)", st, "true")
	}
	if dw != 60 {
		t.Errorf("DisplayWidth = %d, want 60 (platform fallback)", dw)
	}
	if !mil {
		t.Error("MessagesInLog = false, want true (global fallback)")
	}
	if rfd != "/global/files" {
		t.Errorf("ReceivedFilesDir = %q, want %q (platform fallback)", rfd, "/global/files")
	}
}

func TestApplyAgentDisplaySettings_ReceivedFilesDirBothEmpty(t *testing.T) {
	// Verifies that a pre-existing ReceivedFilesDir is not overwritten when
	// both agent and global platform have empty values.
	bot := newBotForTest()
	// Pre-set a value to verify it's NOT overwritten when both are empty
	bot.display.ReceivedFilesDir = "/pre-existing"

	acfg := config.AgentConfig{
		Platforms: []config.PlatformConfig{{
			ID:       "telegram",
			Telegram: &config.TelegramSpecific{},
		}},
	}
	cfg := &config.Config{}

	rc := config.Resolve(cfg, acfg)
	ApplyAgentDisplaySettings(bot, rc.PlatformDisplay("telegram"), rc.Debug, acfg.Platform("telegram"))

	_, _, _, _, rfd, _ := bot.DisplaySettings()
	if rfd != "/pre-existing" {
		t.Errorf("ReceivedFilesDir = %q, want %q (should not be overwritten when both empty)", rfd, "/pre-existing")
	}
}

// TestDisplayOverrideFn_UsesSessionKey verifies that the display override
// function receives the session key as a parameter. This ensures multi-chat
// bots resolve overrides for the chat being served.
func TestDisplayOverrideFn_UsesSessionKey(t *testing.T) {
	bot := newBotForTest()
	bot.display.ShowToolCalls = "off" // bot default

	// Override function returns "full" for sk-turn, nothing for other keys.
	bot.SetDisplayOverrideFn(func(sk string) DisplayOverrides {
		if sk == "sk-turn" {
			return DisplayOverrides{ShowToolCalls: "full"}
		}
		return DisplayOverrides{}
	})

	// With a different session key, should fall back to bot default.
	if got := bot.resolveDisplay("sk-other").ShowToolCalls; got != "off" {
		t.Errorf("with sk-other: got %q, want %q", got, "off")
	}

	// With the matching session key, should resolve the override.
	if got := bot.resolveDisplay("sk-turn").ShowToolCalls; got != "full" {
		t.Errorf("with sk-turn: got %q, want %q", got, "full")
	}

	// Empty session key, should fall back to bot default.
	if got := bot.resolveDisplay("").ShowToolCalls; got != "off" {
		t.Errorf("with empty sk: got %q, want %q", got, "off")
	}
}

func TestApplyAgentDisplaySettings_PartialOverride(t *testing.T) {
	// Verifies that partial agent overrides work correctly with platform
	// defaults filling the gaps.
	bot := newBotForTest()
	// Only override ShowToolCalls at agent level; rest comes from platform config.
	acfg := config.AgentConfig{
		Display: config.DisplayConfig{
			ShowToolCalls: config.Ptr(config.ToolCallFull),
		},
		Platforms: []config.PlatformConfig{{
			ID: "telegram",
			Display: config.DisplayConfig{
				ShowToolCalls: config.Ptr(config.ToolCallOff),
				ShowThinking:  config.Ptr(config.ShowThinkingCompact),
				DisplayWidth:  config.Ptr(44),
			},
			Telegram: &config.TelegramSpecific{},
		}},
	}
	cfg := &config.Config{
		Debug: config.DebugConfig{
			MessagesInLog: config.Ptr(true),
		},
	}

	rc := config.Resolve(cfg, acfg)
	ApplyAgentDisplaySettings(bot, rc.PlatformDisplay("telegram"), rc.Debug, acfg.Platform("telegram"))

	stc, st, dw, mil, _, _ := bot.DisplaySettings()
	// Per-agent platform config (most specific) takes precedence over agent-level
	// via the Merge cascade: per-agent-platform → per-agent → global-platform → defaults.
	if stc != "off" {
		t.Errorf("ShowToolCalls = %q, want %q (platform)", stc, "off")
	}
	if st != "compact" {
		t.Errorf("ShowThinking = %q, want %q (platform fallback)", st, "compact")
	}
	if dw != 44 {
		t.Errorf("DisplayWidth = %d, want 44 (platform fallback)", dw)
	}
	if !mil {
		t.Error("MessagesInLog = false, want true (global fallback)")
	}
}
