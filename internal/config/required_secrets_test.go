package config

import (
	"testing"
)

// TestReflectFindsExplicitSecrets verifies that the reflection walker finds
// string fields tagged as secret references (secret, *_secret, api_key) in
// all config struct types, and ignores empty values.
func TestReflectFindsExplicitSecrets(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "a1", BotSecret: "custom.bot_token"},
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

// TestReflectIgnoresEmptySecrets verifies that empty secret fields are not
// reported as required secrets.
func TestReflectIgnoresEmptySecrets(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "a1", BotSecret: ""},
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

// TestConventionTelegramBot verifies that an agent with telegram_bot set but
// no bot_secret produces a "telegram.<bot_name>" convention secret ref.
func TestConventionTelegramBot(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "scout", Model: "anthropic/claude-sonnet-4-5-20250929", TelegramBot: "scout_bot"},
		},
	}

	refs := RequiredSecrets(&cfg)
	assertHasKey(t, refs, "telegram.scout_bot")
}

// TestConventionTelegramBotWithOverride verifies that when bot_secret is set,
// the convention "telegram.<bot>" ref is NOT produced (the explicit one is
// found by reflection instead).
func TestConventionTelegramBotWithOverride(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "scout", Model: "anthropic/claude-sonnet-4-5-20250929", TelegramBot: "scout_bot", BotSecret: "custom.token"},
		},
	}

	refs := RequiredSecrets(&cfg)
	assertHasKey(t, refs, "custom.token")
	assertMissingKey(t, refs, "telegram.scout_bot")
}

// TestConventionMultiballBots verifies that both per-agent and global multiball
// bot entries produce "telegram.<name>" convention secret refs.
func TestConventionMultiballBots(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "a1", Model: "anthropic/claude-sonnet-4-5-20250929", MultiballBots: []string{"extra1"}},
		},
		Telegram: TelegramConfig{
			MultiballBots: []string{"shared1", "shared2"},
		},
	}

	refs := RequiredSecrets(&cfg)
	assertHasKey(t, refs, "telegram.extra1")
	assertHasKey(t, refs, "telegram.shared1")
	assertHasKey(t, refs, "telegram.shared2")
}

// TestConventionEndpointAPIKey verifies that an endpoint used by an agent with
// no explicit api_key field produces an "<endpoint>.api_key" convention ref.
// The anthropic endpoint is excluded (it has its own credential resolution).
func TestConventionEndpointAPIKey(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "a1", Model: "anthropic/claude-sonnet-4-5-20250929"},
			{ID: "a2", Model: "deepseek/deepseek-chat", Endpoint: "openrouter"},
		},
		Endpoints: map[string]EndpointConfig{
			"openrouter": {Format: "openai"},
		},
	}

	refs := RequiredSecrets(&cfg)
	assertHasKey(t, refs, "openrouter.api_key")
	assertMissingKey(t, refs, "anthropic.api_key")
}

// TestConventionEndpointExplicitAPIKey verifies that an endpoint with an
// explicit api_key field does not produce a convention ref (the explicit one
// is found by reflection).
func TestConventionEndpointExplicitAPIKey(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "a1", Model: "deepseek/deepseek-chat", Endpoint: "openrouter"},
		},
		Endpoints: map[string]EndpointConfig{
			"openrouter": {Format: "openai", APIKey: "or.key"},
		},
	}

	refs := RequiredSecrets(&cfg)
	assertHasKey(t, refs, "or.key")
	assertMissingKey(t, refs, "openrouter.api_key")
}

// TestConventionBraveSearch verifies that brave.api_key is required when any
// agent effectively uses brave search, considering the per-agent → defaults →
// tools resolution chain.
func TestConventionBraveSearch(t *testing.T) {
	t.Run("explicit brave", func(t *testing.T) {
		cfg := Config{
			Agents: []AgentConfig{
				{ID: "a1", Model: "anthropic/claude-sonnet-4-5-20250929", SearchProvider: "brave"},
			},
		}
		assertHasKey(t, RequiredSecrets(&cfg), "brave.api_key")
	})

	t.Run("default brave via tools", func(t *testing.T) {
		cfg := Config{
			Agents: []AgentConfig{
				{ID: "a1", Model: "anthropic/claude-sonnet-4-5-20250929"},
			},
			Tools: ToolsConfig{SearchProvider: "brave"},
		}
		assertHasKey(t, RequiredSecrets(&cfg), "brave.api_key")
	})

	t.Run("anthropic search — no brave key needed", func(t *testing.T) {
		cfg := Config{
			Agents: []AgentConfig{
				{ID: "a1", Model: "anthropic/claude-sonnet-4-5-20250929", SearchProvider: "anthropic"},
			},
		}
		assertMissingKey(t, RequiredSecrets(&cfg), "brave.api_key")
	})
}

// TestConventionTTSHostname verifies that a TTS entry with no explicit secret
// derives the key from the endpoint hostname. edge-tts entries are skipped.
func TestConventionTTSHostname(t *testing.T) {
	cfg := Config{
		TTS: []TTSConfig{
			{ID: "groq", Format: "openai", Endpoint: "https://api.groq.com/openai/v1/audio/speech"},
			{ID: "edge", Format: "edge-tts"},
		},
	}

	refs := RequiredSecrets(&cfg)
	assertHasKey(t, refs, "groq.api_key")
}

// TestConventionSTTHostname verifies that an STT entry with no explicit secret
// derives the key from the endpoint hostname.
func TestConventionSTTHostname(t *testing.T) {
	cfg := Config{
		STT: []STTConfig{
			{ID: "groq", Format: "openai", Endpoint: "https://api.groq.com/openai/v1/audio/transcriptions"},
		},
	}

	refs := RequiredSecrets(&cfg)
	assertHasKey(t, refs, "groq.api_key")
}

// TestDeduplication verifies that the same secret key referenced by multiple
// agents or by both reflection and convention only appears once.
func TestDeduplication(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "a1", Model: "deepseek/deepseek-chat", Endpoint: "openrouter"},
			{ID: "a2", Model: "meta-llama/llama-3-70b", Endpoint: "openrouter"},
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

// TestHostnameSecretKey verifies the URL → secret key derivation.
func TestHostnameSecretKey(t *testing.T) {
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

// TestContextPathsUseIDs verifies that the context field uses slice element IDs
// (not numeric indices) when available.
func TestContextPathsUseIDs(t *testing.T) {
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

// TestUnusedEndpointSecretNotRequired verifies that default endpoints are only
// created for developers agents actually reference. An anthropic-only config
// should NOT produce openai.api_key, gemini.api_key, or openrouter.api_key
// requirements, while TTS/STT secrets (hostname convention + explicit) still
// appear correctly.
func TestUnusedEndpointSecretNotRequired(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{ID: "main", Model: "anthropic/claude-sonnet-4-5-20250929"},
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
