package discord

import (
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/platform"
)

// TestApplyAgentDisplaySettings verifies resolved display config is applied
// field-by-field, zero values keep existing settings, and previously-set
// fields (ToolCallPreviewChars) are preserved.
func TestApplyAgentDisplaySettings(t *testing.T) {
	bot := &Bot{}
	bot.display.ToolCallPreviewChars = 300

	ApplyAgentDisplaySettings(bot, config.ResolvedDisplay{
		ShowToolCalls:         "preview",
		ShowThinking:          "compact",
		DisplayWidth:          44,
		ReceivedFilesDir:      "/tmp/files",
		StreamOutput:          true,
		StreamInterval:        "2s",
		InjectedMessageHeader: "[inject]",
	}, config.ResolvedDebug{MessagesInLog: true})

	d := bot.display
	if d.ShowToolCalls != "preview" || d.ShowThinking != "compact" || d.DisplayWidth != 44 {
		t.Errorf("display not applied: %+v", d)
	}
	if d.ReceivedFilesDir != "/tmp/files" || !d.StreamOutput || d.InjectedMessageHeader != "[inject]" {
		t.Errorf("display not applied: %+v", d)
	}
	if d.StreamUpdateInterval != 2*time.Second {
		t.Errorf("expected 2s stream interval, got %v", d.StreamUpdateInterval)
	}
	if !d.MessagesInLog {
		t.Error("expected MessagesInLog applied")
	}
	if d.ToolCallPreviewChars != 300 {
		t.Error("expected ToolCallPreviewChars preserved")
	}

	// Zero values keep current settings; bad interval is ignored.
	ApplyAgentDisplaySettings(bot, config.ResolvedDisplay{StreamInterval: "bogus"}, config.ResolvedDebug{})
	d = bot.display
	if d.ShowToolCalls != "preview" || d.DisplayWidth != 44 || d.StreamUpdateInterval != 2*time.Second {
		t.Errorf("zero-value apply should keep settings: %+v", d)
	}
	if d.MessagesInLog {
		t.Error("MessagesInLog always tracks debug config")
	}
}

// TestResolveDiscordAllowedUsers verifies per-agent and global allowed users
// are merged with deduplication.
func TestResolveDiscordAllowedUsers(t *testing.T) {
	acfg := config.AgentConfig{Platforms: []config.PlatformConfig{{
		ID:     "discord",
		Access: config.AccessConfig{AllowedUsers: []string{"1", "2"}},
	}}}
	cfg := &config.Config{Platforms: []config.PlatformConfig{{
		ID:     "discord",
		Access: config.AccessConfig{AllowedUsers: []string{"2", "3"}},
	}}}

	got := resolveDiscordAllowedUsers(acfg, cfg)
	seen := map[string]bool{}
	for _, u := range got {
		if seen[u] {
			t.Errorf("duplicate user %q in %v", u, got)
		}
		seen[u] = true
	}
	if !seen["1"] || !seen["2"] || !seen["3"] {
		t.Errorf("expected merged users 1,2,3, got %v", got)
	}

	// No platform entries at all: empty.
	if got := resolveDiscordAllowedUsers(config.AgentConfig{}, &config.Config{}); len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

// TestSetupAgentNoPlatformConfigured verifies SetupAgent returns nil when the
// agent has no discord platform (no bot is created).
func TestSetupAgentNoPlatformConfigured(t *testing.T) {
	mgr := NewBotManager()
	res := SetupAgent(mgr, AgentSetupParams{
		AgentConfig:  config.AgentConfig{ID: "a"},
		GlobalConfig: &config.Config{},
	})
	if res != nil {
		t.Error("expected nil result without discord config")
	}
	if mgr.PrimaryBot("a") != nil {
		t.Error("expected no primary bot registered")
	}
}

// TestSetupAgentResultClosures verifies the SetupResult closures returned for
// an existing primary bot: DisplayDefaultsFn snapshots the bot's display
// config and ConfigureFacetConn rewires a facet bot.
func TestSetupAgentResultClosures(t *testing.T) {
	mgr := NewBotManager()
	primary, _, _ := newTestBot(t, "a")
	primary.display = BotDisplayConfig{
		ShowToolCalls: "full",
		ShowThinking:  "compact",
		StreamOutput:  true,
		DisplayWidth:  44,
	}
	mgr.AddPrimary("a", primary)

	cfg := &config.Config{}
	acfg := config.AgentConfig{ID: "a"} // no discord platform: setup skips bot creation
	res := SetupAgent(mgr, AgentSetupParams{
		AgentConfig:  acfg,
		GlobalConfig: cfg,
		Resolved:     config.Resolve(cfg, acfg),
	})
	if res == nil {
		t.Fatal("expected result for existing primary bot")
	}

	ds := res.DisplayDefaultsFn()
	want := platform.DisplaySettings{ShowToolCalls: "full", ShowThinking: "compact", StreamOutput: "on", DisplayWidth: "44"}
	if ds != want {
		t.Errorf("display defaults: got %+v, want %+v", ds, want)
	}

	facet, _, _ := newTestBot(t, "")
	res.ConfigureFacetConn(facet)
	if facet.dispatcher == nil {
		t.Error("expected facet command context configured")
	}

	// Non-discord connections are ignored.
	res.ConfigureFacetConn(nil)
}

// TestSetupAgentMissingToken verifies setup without a resolvable bot token
// registers no bots.
func TestSetupAgentMissingToken(t *testing.T) {
	mgr := NewBotManager()
	acfg := config.AgentConfig{
		ID: "a",
		Platforms: []config.PlatformConfig{{
			ID:  "discord",
			Bot: "mybot",
		}},
	}
	res := SetupAgent(mgr, AgentSetupParams{
		AgentConfig:  acfg,
		GlobalConfig: &config.Config{},
		SecretStore:  testSecretStore(t, ""),
	})
	if res != nil || mgr.PrimaryBot("a") != nil {
		t.Error("expected no setup without a token secret")
	}
}
