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
			Powerful: "openrouter/some-model",
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
