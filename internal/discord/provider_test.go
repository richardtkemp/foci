package discord

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/secrets"
)

// testSecretStore builds a secrets.Store from a TOML literal in a temp dir.
func testSecretStore(t *testing.T, toml string) *secrets.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.toml")
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := secrets.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestProviderName verifies the registry name.
func TestProviderName(t *testing.T) {
	p := &discordProvider{}
	if p.Name() != "discord" {
		t.Errorf("got %q", p.Name())
	}
}

// TestProviderIsConfigured verifies the configured checks: missing platform
// entry, empty allowed users, allowed_users_only opt-out, and a valid config.
func TestProviderIsConfigured(t *testing.T) {
	p := &discordProvider{}
	f := false

	tests := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"no platform entry", &config.Config{}, false},
		{
			"empty allowed users",
			&config.Config{Platforms: []config.PlatformConfig{{ID: "discord"}}},
			false,
		},
		{
			"allowed_users_only disabled",
			&config.Config{Platforms: []config.PlatformConfig{{
				ID:     "discord",
				Access: config.AccessConfig{AllowedUsersOnly: &f},
			}}},
			true,
		},
		{
			"allowed users present",
			&config.Config{Platforms: []config.PlatformConfig{{
				ID:     "discord",
				Access: config.AccessConfig{AllowedUsers: []string{"123"}},
			}}},
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := p.IsConfigured(tt.cfg)
			if got != tt.want {
				t.Errorf("got %v (%q), want %v", got, reason, tt.want)
			}
			if !got && reason == "" {
				t.Error("expected a reason when not configured")
			}
		})
	}
}

// TestProviderInitAndClose verifies Init creates the manager, connection
// adapter, and tool detail store under the config data dir, and Close releases
// the store.
func TestProviderInitAndClose(t *testing.T) {
	p := &discordProvider{}
	deps := platform.ProviderDeps{Config: &config.Config{DataDir: t.TempDir()}}
	if err := p.Init(deps); err != nil {
		t.Fatal(err)
	}
	if p.mgr == nil || p.connMgr == nil {
		t.Error("expected manager and connection adapter")
	}
	if p.toolDetailStore == nil {
		t.Fatal("expected tool detail store created")
	}
	if p.ConnectionManager() == nil {
		t.Error("expected connection manager")
	}
	if p.ToolDetailStore() == nil {
		t.Error("expected non-nil tool detail store accessor")
	}
	if err := p.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

// TestProviderCloseWithoutStore verifies Close is a no-op without a store and
// the accessor returns a nil interface.
func TestProviderCloseWithoutStore(t *testing.T) {
	p := &discordProvider{}
	if err := p.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if p.ToolDetailStore() != nil {
		t.Error("expected nil interface when no store")
	}
}

// TestProviderAgentPreFlight verifies the pre-flight warns when the agent's
// bot token secret is missing and passes when present.
func TestProviderAgentPreFlight(t *testing.T) {
	p := &discordProvider{deps: platform.ProviderDeps{
		SecretStore: testSecretStore(t, "[discord]\nmyagent = \"token\"\n"),
	}}

	if warnings := p.AgentPreFlight("myagent"); warnings != nil {
		t.Errorf("expected no warnings, got %v", warnings)
	}
	warnings := p.AgentPreFlight("other")
	if len(warnings) != 1 || !strings.Contains(warnings[0], "discord.other") {
		t.Errorf("expected missing-secret warning, got %v", warnings)
	}
}

// TestProviderDefaultPlatformConfig verifies key defaults: ID, tool calls off,
// mention required, auto-thread on, and a 60m facet TTL.
func TestProviderDefaultPlatformConfig(t *testing.T) {
	p := &discordProvider{}
	pc := p.DefaultPlatformConfig()

	if pc.ID != "discord" {
		t.Errorf("ID: got %q", pc.ID)
	}
	if pc.Display.ShowToolCalls == nil || *pc.Display.ShowToolCalls != config.ToolCallOff {
		t.Error("expected tool calls off by default")
	}
	if pc.Access.RequireMention == nil || !*pc.Access.RequireMention {
		t.Error("expected require_mention default true")
	}
	if pc.Discord == nil || pc.Discord.AutoThread == nil || !*pc.Discord.AutoThread {
		t.Error("expected auto_thread default true")
	}
	if pc.FacetSessionTTL != "60m" {
		t.Errorf("TTL: got %q", pc.FacetSessionTTL)
	}
	if len(p.ValidateConfig(pc)) != 0 {
		t.Error("expected no validation issues for defaults")
	}
}

// TestProviderSetLifecycleCallback verifies callbacks land on the agent's
// primary bot and missing bots are a safe no-op.
func TestProviderSetLifecycleCallback(t *testing.T) {
	p := &discordProvider{mgr: NewBotManager()}
	bot := &Bot{}
	p.mgr.AddPrimary("a", bot)

	var called []string
	p.SetLifecycleCallback("a", platform.OnUserMessage, func() { called = append(called, "user") })
	p.SetLifecycleCallback("ghost", platform.OnUserMessage, func() {}) // no bot: no-op

	bot.OnUserMessage()
	if len(called) != 1 || called[0] != "user" {
		t.Errorf("unexpected callback wiring %v", called)
	}
}

// TestProviderRestoreFacetSessionsNoIndex verifies RestoreFacetSessions is a
// safe no-op without a session index.
func TestProviderRestoreFacetSessionsNoIndex(t *testing.T) {
	p := &discordProvider{mgr: NewBotManager()}
	p.RestoreFacetSessions(platform.RestoreParams{}) // must not panic
}

// TestProviderSetupSharedFacetNoop verifies SetupSharedFacet is a no-op
// (Discord uses threads for facets).
func TestProviderSetupSharedFacetNoop(t *testing.T) {
	p := &discordProvider{}
	p.SetupSharedFacet(platform.SharedFacetParams{}) // must not panic
}
