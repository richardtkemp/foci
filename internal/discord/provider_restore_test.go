package discord

import (
	"path/filepath"
	"testing"

	"foci/internal/command"
	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/session"
)

// TestRestoreFacetSessions verifies persisted facet mappings are restored after
// restart: live sessions are re-attached to their facet bot (rewired to the
// agent and inheriting the primary's channel), while mappings to dead sessions
// are cleaned up.
func TestRestoreFacetSessions(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.NewStore(t.TempDir())

	// A live session for facet bot fb1 and a dead mapping for fb2.
	liveKey := "a/c42/123"
	if err := sessions.TestAppend(liveKey, provider.Message{Role: "user"}); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetAgentMetadata("_system", "discord_facet:fb1", liveKey); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetAgentMetadata("_system", "discord_facet:fb2", "a/c99/dead"); err != nil {
		t.Fatal(err)
	}

	mgr := NewBotManager()
	primary, _, _ := newTestBot(t, "a")
	primary.SetChatID(42)
	mgr.AddPrimary("a", primary)

	liveFacet, _, _ := newTestBot(t, "")
	liveFacet.botUserID = "fb1"
	mgr.AddFacet("a", liveFacet)
	deadFacet, _, _ := newTestBot(t, "")
	deadFacet.botUserID = "fb2"
	mgr.AddFacet("a", deadFacet)

	cfg := &config.Config{}
	restoreFacetSessions(mgr, idx, sessions, cfg, platform.RestoreParams{
		AgentOrder: []string{"a"},
		Resolver: func(agentID string) (platform.MessageHandler, any, any, config.AgentConfig, bool) {
			if agentID != "a" {
				return nil, nil, nil, config.AgentConfig{}, false
			}
			return nil, command.NewRegistry(), command.CommandContext{}, config.AgentConfig{ID: "a"}, true
		},
	})

	if got := liveFacet.SessionKey(); got != liveKey {
		t.Errorf("expected live facet restored to %q, got %q", liveKey, got)
	}
	if liveFacet.dispatcher == nil {
		t.Error("expected live facet command context rewired")
	}
	if liveFacet.ChatID() != 42 {
		t.Errorf("expected live facet to inherit primary channel, got %d", liveFacet.ChatID())
	}

	if got := deadFacet.SessionKey(); got != "" {
		t.Errorf("expected dead facet left idle, got %q", got)
	}
	meta, err := idx.AgentMetadataByPrefix("_system", "discord_facet:")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := meta["discord_facet:fb2"]; ok {
		t.Error("expected dead facet mapping deleted")
	}
	if meta["discord_facet:fb1"] != liveKey {
		t.Error("expected live facet mapping retained")
	}
}

// TestSetupAgentConnectionNoPlatform verifies the provider-level setup adapter
// returns nil when the agent has no discord platform configured.
func TestSetupAgentConnectionNoPlatform(t *testing.T) {
	p := &discordProvider{
		mgr:  NewBotManager(),
		deps: platform.ProviderDeps{Config: &config.Config{}},
	}
	res := p.SetupAgentConnection(platform.AgentConnectionParams{
		AgentConfig: config.AgentConfig{ID: "a"},
		Commands:    command.NewRegistry(),
	})
	if res != nil {
		t.Error("expected nil setup result without discord platform config")
	}
}
