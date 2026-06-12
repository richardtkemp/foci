package telegram

import (
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/secrets"
	"foci/internal/tooldetail"
)

// emptySecretStore returns a usable, empty secrets.Store backed by a
// nonexistent file in a temp dir.
func emptySecretStore(t *testing.T) *secrets.Store {
	t.Helper()
	s, err := secrets.Load(filepath.Join(t.TempDir(), "secrets.toml"))
	if err != nil {
		t.Fatalf("load secrets: %v", err)
	}
	return s
}

func TestProviderName(t *testing.T) {
	// Proves the provider registers under the canonical platform name.
	p := &telegramProvider{}
	if got := p.Name(); got != "telegram" {
		t.Errorf("Name = %q, want telegram", got)
	}
}

func TestProviderIsConfigured(t *testing.T) {
	// Proves IsConfigured requires a [[platforms]] telegram entry with either
	// allowed users or an explicit allowed_users_only=false opt-out.
	off := false
	tests := []struct {
		name       string
		cfg        *config.Config
		want       bool
		wantReason string
	}{
		{
			name:       "no platform entry",
			cfg:        &config.Config{},
			want:       false,
			wantReason: "no [[platforms]] entry",
		},
		{
			name:       "no allowed users",
			cfg:        &config.Config{Platforms: []config.PlatformConfig{{ID: "telegram"}}},
			want:       false,
			wantReason: "allowed_users is empty",
		},
		{
			name: "allowed users set",
			cfg: &config.Config{Platforms: []config.PlatformConfig{{
				ID:     "telegram",
				Access: config.AccessConfig{AllowedUsers: []string{"111"}},
			}}},
			want: true,
		},
		{
			name: "open access opt-out",
			cfg: &config.Config{Platforms: []config.PlatformConfig{{
				ID:     "telegram",
				Access: config.AccessConfig{AllowedUsersOnly: &off},
			}}},
			want: true,
		},
	}
	p := &telegramProvider{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := p.IsConfigured(tt.cfg)
			if got != tt.want {
				t.Errorf("IsConfigured = %v, want %v", got, tt.want)
			}
			if tt.wantReason != "" && !strings.Contains(reason, tt.wantReason) {
				t.Errorf("reason = %q, want containing %q", reason, tt.wantReason)
			}
		})
	}
}

func TestProviderInitAndClose(t *testing.T) {
	// Proves Init builds the bot manager, connection manager, and the SQLite
	// tool detail store under the configured data dir, and Close shuts the
	// store down cleanly.
	p := &telegramProvider{}
	deps := platform.ProviderDeps{Config: &config.Config{DataDir: t.TempDir()}}

	if err := p.Init(deps); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.mgr == nil || p.connMgr == nil {
		t.Fatal("Init left manager/connection manager nil")
	}
	if p.ConnectionManager() != platform.ConnectionManager(p.connMgr) {
		t.Error("ConnectionManager returned a different adapter")
	}
	if p.ToolDetailStore() == nil {
		t.Error("ToolDetailStore = nil, want store from Init")
	}
	if err := p.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestProviderToolDetailStoreNil(t *testing.T) {
	// Proves ToolDetailStore returns a true nil interface (not a typed-nil
	// *tooldetail.Store) when no store was created, and Close is a safe no-op.
	p := &telegramProvider{}
	if got := p.ToolDetailStore(); got != nil {
		t.Errorf("ToolDetailStore = %v, want untyped nil", got)
	}
	if err := p.Close(); err != nil {
		t.Errorf("Close without store: %v", err)
	}
}

func TestProviderAgentPreFlight(t *testing.T) {
	// Proves AgentPreFlight warns when the agent's bot token secret is missing
	// and returns no warnings once it exists.
	store := emptySecretStore(t)
	p := &telegramProvider{deps: platform.ProviderDeps{SecretStore: store}}

	warnings := p.AgentPreFlight("scout")
	if len(warnings) != 1 || !strings.Contains(warnings[0], "telegram.scout") {
		t.Fatalf("warnings = %v, want one mentioning telegram.scout", warnings)
	}

	store.Set("telegram.scout", "tok")
	if warnings := p.AgentPreFlight("scout"); warnings != nil {
		t.Errorf("warnings = %v, want nil when secret present", warnings)
	}
}

func TestProviderSetLifecycleCallback(t *testing.T) {
	// Proves SetLifecycleCallback wires each lifecycle event to the agent's
	// primary bot and silently ignores unknown agents.
	p := &telegramProvider{mgr: NewBotManager()}
	bot := newBotForTest()
	p.mgr.AddPrimary("scout", bot)

	called := map[platform.LifecycleEvent]bool{}
	for _, ev := range []platform.LifecycleEvent{platform.OnUserMessage, platform.OnTurnComplete, platform.OnTurnEnd} {
		ev := ev
		p.SetLifecycleCallback("scout", ev, func() { called[ev] = true })
	}
	bot.OnUserMessage()
	bot.OnTurnComplete()
	bot.OnTurnEnd()
	if len(called) != 3 {
		t.Errorf("called = %v, want all three lifecycle events wired", called)
	}

	// Unknown agent: must not panic.
	p.SetLifecycleCallback("unknown", platform.OnUserMessage, func() {})
}

func TestProviderDefaultPlatformConfig(t *testing.T) {
	// Proves the default platform config carries the documented telegram
	// defaults (id, display modes, 44-char width, 60m facet TTL, long-poll).
	p := &telegramProvider{}
	dc := p.DefaultPlatformConfig()

	if dc.ID != "telegram" {
		t.Errorf("ID = %q, want telegram", dc.ID)
	}
	if *dc.Display.ShowToolCalls != config.ToolCallOff {
		t.Errorf("ShowToolCalls = %q, want off", *dc.Display.ShowToolCalls)
	}
	if *dc.Display.DisplayWidth != 44 {
		t.Errorf("DisplayWidth = %d, want 44", *dc.Display.DisplayWidth)
	}
	if dc.FacetSessionTTL != "60m" {
		t.Errorf("FacetSessionTTL = %q, want 60m", dc.FacetSessionTTL)
	}
	if dc.Telegram == nil || dc.Telegram.LongPollTimeout != "30s" {
		t.Error("Telegram.LongPollTimeout missing or wrong")
	}
	if errs := p.ValidateConfig(dc); errs != nil {
		t.Errorf("ValidateConfig = %v, want nil", errs)
	}
}

func TestProviderCloseWithStore(t *testing.T) {
	// Proves Close vacuums and closes an attached tool detail store without error.
	store, err := tooldetail.NewStore(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	p := &telegramProvider{toolDetailStore: store}
	if err := p.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
