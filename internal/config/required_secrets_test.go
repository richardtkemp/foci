package config

import (
	"testing"
)

func TestReflectFindsExplicitSecrets(t *testing.T) {
	// Proves the reflection walker finds all explicitly-declared secret references
	// across agent, TTS, STT, and endpoint config structs, ignoring empty values.
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "a1", Platforms: &PlatformsConfig{Telegram: &TelegramPlatformConfig{BotSecret: "custom.bot_token"}}},
		},
		TTS: []TTSConfig{
			{ID: "tts1", Secret: "groq.api_key"},
		},
		STT: []STTConfig{
			{ID: "stt1", Secret: "deepgram.api_key"},
		},
		Endpoints: map[string]EndpointConfig{
			"openrouter": {APIKey: "openrouter.api_key"},
		},
	}

	refs := RequiredSecrets(&cfg)
	want := map[string]bool{
		"custom.bot_token":   true,
		"groq.api_key":       true,
		"deepgram.api_key":   true,
		"openrouter.api_key": true,
	}

	found := make(map[string]bool)
	for _, ref := range refs {
		if want[ref.Key] {
			found[ref.Key] = true
		}
	}
	for key := range want {
		if !found[key] {
			t.Errorf("expected secret ref %q not found in results", key)
		}
	}
}

func TestReflectIgnoresEmptySecrets(t *testing.T) {
	// Proves that empty secret fields do not produce entries in the required
	// secrets list, preventing spurious missing-secret warnings.
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "a1", Platforms: &PlatformsConfig{Telegram: &TelegramPlatformConfig{BotSecret: ""}}},
		},
		TTS: []TTSConfig{
			{ID: "tts1", Secret: ""},
		},
	}

	refs := RequiredSecrets(&cfg)
	for _, ref := range refs {
		if ref.Key == "" {
			t.Error("empty key should not appear in results")
		}
	}
}

func TestConventionTelegramBot(t *testing.T) {
	// Proves that an agent with telegram_bot set but no bot_secret produces
	// a "telegram.<bot_name>" convention secret reference.
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "scout", Platforms: &PlatformsConfig{Telegram: &TelegramPlatformConfig{Bot: "scout_bot"}}},
		},
	}

	refs := RequiredSecrets(&cfg)
	assertHasKey(t, refs, "telegram.scout_bot")
}

func TestConventionTelegramBotWithOverride(t *testing.T) {
	// Proves that when bot_secret is set, the convention "telegram.<bot>" ref is
	// not produced; only the explicit override key is reported.
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "scout", Platforms: &PlatformsConfig{Telegram: &TelegramPlatformConfig{Bot: "scout_bot", BotSecret: "custom.token"}}},
		},
	}

	refs := RequiredSecrets(&cfg)
	assertHasKey(t, refs, "custom.token")
	assertMissingKey(t, refs, "telegram.scout_bot")
}

func TestConventionFacetBots(t *testing.T) {
	// Proves that both per-agent and global [telegram] facet_bots entries
	// produce "telegram.<name>" convention secret references.
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "a1", Platforms: &PlatformsConfig{Telegram: &TelegramPlatformConfig{FacetBots: []string{"extra1"}}}},
		},
		Telegram: TelegramConfig{
			FacetBots: []string{"shared1", "shared2"},
		},
	}

	refs := RequiredSecrets(&cfg)
	assertHasKey(t, refs, "telegram.extra1")
	assertHasKey(t, refs, "telegram.shared1")
	assertHasKey(t, refs, "telegram.shared2")
}

func TestConventionEndpointAPIKey(t *testing.T) {
	// Proves that a model group resolving to a non-anthropic endpoint with no
	// explicit api_key generates a "<endpoint>.api_key" convention ref, while
	// anthropic endpoints are excluded from this convention.
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "a1"},
		},
		Models: ModelsConfig{
			Powerful: "anthropic/claude-sonnet-4-5-20250929",
			Cheap:    "deepseek/deepseek-chat", // resolves to openrouter endpoint
		},
		Endpoints: map[string]EndpointConfig{
			"openrouter": {Format: "openai"},
		},
	}

	refs := RequiredSecrets(&cfg)
	assertHasKey(t, refs, "openrouter.api_key")
	assertMissingKey(t, refs, "anthropic.api_key")
}

func TestConventionEndpointExplicitAPIKey(t *testing.T) {
	// Proves that when an endpoint has an explicit api_key, only that key is
	// reported and no convention ref is generated for the endpoint name.
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "a1"},
		},
		Models: ModelsConfig{
			Powerful: "openrouter/some-model",
		},
		Endpoints: map[string]EndpointConfig{
			"openrouter": {Format: "openai", APIKey: "or.key"},
		},
	}

	refs := RequiredSecrets(&cfg)
	assertHasKey(t, refs, "or.key")
	assertMissingKey(t, refs, "openrouter.api_key")
}

func TestConventionBraveSearch(t *testing.T) {
	// Proves that brave.api_key is required whenever any agent uses brave search
	// (directly or via tools default), and is not required when another provider
	// is configured.
	t.Run("explicit brave", func(t *testing.T) {
		cfg := Config{
			Agents: []AgentConfig{
				{ID: "a1", SearchProvider: "brave"},
			},
		}
		assertHasKey(t, RequiredSecrets(&cfg), "brave.api_key")
	})

	t.Run("default brave via tools", func(t *testing.T) {
		cfg := Config{
			Agents: []AgentConfig{
				{ID: "a1"},
			},
			Tools: ToolsConfig{SearchProvider: "brave"},
		}
		assertHasKey(t, RequiredSecrets(&cfg), "brave.api_key")
	})

	t.Run("anthropic search — no brave key needed", func(t *testing.T) {
		cfg := Config{
			Agents: []AgentConfig{
				{ID: "a1", SearchProvider: "anthropic"},
			},
		}
		assertMissingKey(t, RequiredSecrets(&cfg), "brave.api_key")
	})
}

func TestConventionTTSHostname(t *testing.T) {
	// Proves that a TTS entry with no explicit secret derives the required key
	// from the endpoint's hostname, and that edge-tts entries require no secret.
	cfg := Config{
		TTS: []TTSConfig{
			{ID: "groq", Format: "openai", Endpoint: "https://api.groq.com/openai/v1/audio/speech"},
			{ID: "edge", Format: "edge-tts"},
		},
	}

	refs := RequiredSecrets(&cfg)
	assertHasKey(t, refs, "groq.api_key")
}

func TestConventionSTTHostname(t *testing.T) {
	// Proves that an STT entry with no explicit secret derives the required key
	// from the endpoint's hostname.
	cfg := Config{
		STT: []STTConfig{
			{ID: "groq", Format: "openai", Endpoint: "https://api.groq.com/openai/v1/audio/transcriptions"},
		},
	}

	refs := RequiredSecrets(&cfg)
	assertHasKey(t, refs, "groq.api_key")
}

func TestDeduplication(t *testing.T) {
	// Proves that the same secret key referenced by multiple sources only
	// appears once in the required secrets list.
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "a1"},
			{ID: "a2"},
		},
		Models: ModelsConfig{
			Powerful: "openrouter/some-model",
			Cheap:    "openrouter/another-model",
		},
		Endpoints: map[string]EndpointConfig{
			"openrouter": {Format: "openai"},
		},
	}

	refs := RequiredSecrets(&cfg)
	count := 0
	for _, ref := range refs {
		if ref.Key == "openrouter.api_key" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected openrouter.api_key exactly once, got %d", count)
	}
}

func TestHostnameSecretKey(t *testing.T) {
	// Proves HostnameSecretKey correctly derives "<hostname>.api_key" from a
	// full URL, stripping the "api." subdomain prefix where present.
	tests := []struct {
		endpoint string
		want     string
	}{
		{"https://api.groq.com/openai/v1", "groq.api_key"},
		{"https://openrouter.ai/api/v1", "openrouter.api_key"},
		{"https://api.deepgram.com/v1/listen", "deepgram.api_key"},
		{"http://localhost:8080/v1", "localhost.api_key"},
		{"", ""},
	}
	for _, tt := range tests {
		got := HostnameSecretKey(tt.endpoint)
		if got != tt.want {
			t.Errorf("HostnameSecretKey(%q) = %q, want %q", tt.endpoint, got, tt.want)
		}
	}
}

func TestContextPathsUseIDs(t *testing.T) {
	// Proves that the Context field in SecretRef uses the element's ID (e.g.
	// "tts[groq-tts].secret") rather than a numeric index for readability.
	cfg := Config{
		TTS: []TTSConfig{
			{ID: "groq-tts", Secret: "groq.api_key"},
		},
	}

	refs := RequiredSecrets(&cfg)
	for _, ref := range refs {
		if ref.Key == "groq.api_key" {
			if ref.Context != "tts[groq-tts].secret" {
				t.Errorf("expected context with ID, got %q", ref.Context)
			}
			return
		}
	}
	t.Error("groq.api_key ref not found")
}

func TestUnusedEndpointSecretNotRequired(t *testing.T) {
	// Proves that endpoints not referenced by any agent do not generate secret
	// requirements, so an anthropic-only deployment does not ask for openai,
	// gemini, or openrouter keys, while TTS/STT secrets still appear.
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "main"},
		},
		TTS: []TTSConfig{
			{ID: "groq-tts", Format: "openai", Endpoint: "https://api.groq.com/openai/v1/audio/speech"},
		},
		STT: []STTConfig{
			{ID: "groq-stt", Format: "openai", Endpoint: "https://api.groq.com/openai/v1/audio/transcriptions", Secret: "groq.api_key"},
		},
	}

	refs := RequiredSecrets(&cfg)

	// Unused endpoint defaults should NOT be created, so their api_keys
	// should not appear in required secrets.
	assertMissingKey(t, refs, "openai.api_key")
	assertMissingKey(t, refs, "gemini.api_key")
	assertMissingKey(t, refs, "openrouter.api_key")

	// TTS hostname convention and STT explicit secret should still work.
	assertHasKey(t, refs, "groq.api_key")
}

func assertHasKey(t *testing.T, refs []SecretRef, key string) {
	t.Helper()
	for _, ref := range refs {
		if ref.Key == key {
			return
		}
	}
	t.Errorf("expected secret ref %q not found", key)
}

func assertMissingKey(t *testing.T, refs []SecretRef, key string) {
	t.Helper()
	for _, ref := range refs {
		if ref.Key == key {
			t.Errorf("unexpected secret ref %q found (context: %s)", key, ref.Context)
			return
		}
	}
}
