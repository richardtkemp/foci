package main

import (
	"foci/internal/config"
	"testing"
)

// mapStore is a simple SecretGetter backed by a map, for testing.
type mapStore map[string]string

func (m mapStore) Get(key string) (string, bool) {
	v, ok := m[key]
	return v, ok
}

func TestMissingOptionalSecretDowngradedToInfo(t *testing.T) {
	// Proves that a missing optional secret (brave.api_key — search_provider
	// defaults to "brave", but the web-search tool self-gates on the key) is
	// downgraded to INFO rather than emitting a startup WARN on every boot (#852).
	brave := "brave"
	cfg := &config.Config{
		Agents: []config.AgentConfig{
			{ID: "a1", Tools: config.AgentToolsOverride{ToolConfig: config.ToolConfig{SearchProvider: &brave}}},
		},
	}
	store := mapStore{} // no brave.api_key

	var braveMissing *missingSecret
	results := checkMissingSecrets(cfg, store)
	for i, ms := range results {
		if ms.ref.Key == "brave.api_key" {
			braveMissing = &results[i]
			break
		}
	}
	if braveMissing == nil {
		t.Fatal("expected brave.api_key to be reported as missing")
	}
	if !braveMissing.downgraded {
		t.Error("expected brave.api_key (optional) to be downgraded to INFO, not WARN")
	}
}

func TestMissingPlatformSecretDowngradedWhenAlternativeExists(t *testing.T) {
	// Proves that when an agent has both Telegram and Discord configured and
	// only one secret is present, the missing platform secret is downgraded
	// (INFO not WARN) because the agent can still operate.
	cfg := &config.Config{
		Agents: []config.AgentConfig{
			{
				ID: "fotini",
				Platforms: []config.PlatformConfig{
					{ID: "telegram", Bot: "fotini"},
					{ID: "discord", Bot: "fotini"},
				},
			},
		},
	}
	store := mapStore{
		"telegram.fotini": "tok123", // telegram present
		// discord.fotini missing
	}

	results := checkMissingSecrets(cfg, store)

	var discordMissing *missingSecret
	for i, ms := range results {
		if ms.ref.Key == "discord.fotini" {
			discordMissing = &results[i]
			break
		}
	}
	if discordMissing == nil {
		t.Fatal("expected discord.fotini to be reported as missing")
	}
	if !discordMissing.downgraded {
		t.Error("expected discord.fotini to be downgraded (agent has working telegram)")
	}
}

func TestMissingPlatformSecretWarnWhenNoAlternative(t *testing.T) {
	// Proves that when an agent has only one platform configured and its
	// secret is missing, the warning is NOT downgraded — the agent has
	// no working platform at all.
	cfg := &config.Config{
		Agents: []config.AgentConfig{
			{
				ID: "fotini",
				Platforms: []config.PlatformConfig{
					{ID: "discord", Bot: "fotini"},
				},
			},
		},
	}
	store := mapStore{} // no secrets at all

	results := checkMissingSecrets(cfg, store)

	var discordMissing *missingSecret
	for i, ms := range results {
		if ms.ref.Key == "discord.fotini" {
			discordMissing = &results[i]
			break
		}
	}
	if discordMissing == nil {
		t.Fatal("expected discord.fotini to be reported as missing")
	}
	if discordMissing.downgraded {
		t.Error("expected discord.fotini NOT to be downgraded (no working platform)")
	}
}

func TestMissingPlatformSecretWarnWhenBothMissing(t *testing.T) {
	// Proves that when both platform secrets are missing, neither is
	// downgraded — the agent has no working platform.
	cfg := &config.Config{
		Agents: []config.AgentConfig{
			{
				ID: "fotini",
				Platforms: []config.PlatformConfig{
					{ID: "telegram", Bot: "fotini"},
					{ID: "discord", Bot: "fotini"},
				},
			},
		},
	}
	store := mapStore{} // no secrets

	results := checkMissingSecrets(cfg, store)

	for _, ms := range results {
		if ms.downgraded {
			t.Errorf("expected %q NOT to be downgraded (no working platforms)", ms.ref.Key)
		}
	}
}

func TestNonPlatformSecretNeverDowngraded(t *testing.T) {
	// Proves that non-platform secrets (e.g. endpoint API keys) are never
	// downgraded regardless of other platform state.
	cfg := &config.Config{
		Agents: []config.AgentConfig{
			{
				ID: "fotini",
				Platforms: []config.PlatformConfig{
					{ID: "telegram", Bot: "fotini"},
				},
			},
		},
		Groups: config.GroupsConfig{
			Groups: map[string]string{"powerful": "openrouter/some-model"},
		},
		Endpoints: map[string]config.EndpointConfig{
			"openrouter": {Format: "openai"},
		},
	}
	store := mapStore{
		"telegram.fotini": "tok123",
		// openrouter.api_key missing
	}

	results := checkMissingSecrets(cfg, store)

	for _, ms := range results {
		if ms.ref.Key == "openrouter.api_key" && ms.downgraded {
			t.Error("expected non-platform secret openrouter.api_key NOT to be downgraded")
		}
	}
}

func TestGlobalFacetBotNotDowngraded(t *testing.T) {
	// Proves that global facet bot secrets (no AgentID) are never downgraded,
	// since they don't belong to a specific agent.
	cfg := &config.Config{
		Platforms: []config.PlatformConfig{
			{ID: "telegram", FacetBots: []string{"shared_bot"}},
		},
		Agents: []config.AgentConfig{
			{
				ID: "fotini",
				Platforms: []config.PlatformConfig{
					{ID: "telegram", Bot: "fotini"},
				},
			},
		},
	}
	store := mapStore{
		"telegram.fotini": "tok123",
		// telegram.shared_bot missing
	}

	results := checkMissingSecrets(cfg, store)

	for _, ms := range results {
		if ms.ref.Key == "telegram.shared_bot" && ms.downgraded {
			t.Error("expected global facet bot secret NOT to be downgraded")
		}
	}
}

func TestAllSecretsPresent(t *testing.T) {
	// Proves that when all secrets are present, no missing entries are reported.
	cfg := &config.Config{
		Agents: []config.AgentConfig{
			{
				ID: "fotini",
				Platforms: []config.PlatformConfig{
					{ID: "telegram", Bot: "fotini"},
					{ID: "discord", Bot: "fotini"},
				},
			},
		},
	}
	store := mapStore{
		"telegram.fotini": "tok1",
		"discord.fotini":  "tok2",
	}

	results := checkMissingSecrets(cfg, store)
	if len(results) != 0 {
		t.Errorf("expected no missing secrets, got %d", len(results))
	}
}

func TestMissingPlatformSecretDowngradedWhenAppPlatformEnabled(t *testing.T) {
	// Proves that when the app (Android) platform is enabled globally, an
	// agent's missing telegram/discord bot secrets downgrade to INFO — the
	// agent is reachable via the app even with no bot tokens. And, crucially,
	// that without the app platform they still WARN (the check isn't vacuous —
	// app is config-gated, not default-on).
	cfg := &config.Config{
		Platforms: []config.PlatformConfig{
			{ID: "app"},
		},
		Agents: []config.AgentConfig{
			{
				ID: "arnix",
				Platforms: []config.PlatformConfig{
					{ID: "telegram", Bot: "arnix"},
					{ID: "discord", Bot: "arnix"},
				},
			},
		},
	}
	store := mapStore{} // no secrets

	seen := 0
	for _, ms := range checkMissingSecrets(cfg, store) {
		if ms.ref.Key == "telegram.arnix" || ms.ref.Key == "discord.arnix" {
			seen++
			if !ms.downgraded {
				t.Errorf("expected %q to be downgraded (app platform enabled)", ms.ref.Key)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("expected telegram.arnix + discord.arnix reported missing, saw %d", seen)
	}

	// Without the app platform, the same secrets must still WARN.
	cfg.Platforms = nil
	for _, ms := range checkMissingSecrets(cfg, store) {
		if (ms.ref.Key == "telegram.arnix" || ms.ref.Key == "discord.arnix") && ms.downgraded {
			t.Errorf("without app platform, %q should NOT be downgraded", ms.ref.Key)
		}
	}
}
