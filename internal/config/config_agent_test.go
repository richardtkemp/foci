package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tomlParser "github.com/BurntSushi/toml"
)

func TestResolveBotToken(t *testing.T) {
	// Proves that ResolveBotToken follows the telegram.<botName> convention by
	// default and uses an explicit bot_secret override when provided, returning
	// empty when the bot name is empty or the secret is not found.
	t.Run("convention: telegram.<botName>", func(t *testing.T) {
		secrets := mockSecrets{
			"telegram.primary": "token-primary-123",
			"telegram.scout":   "token-scout-456",
		}

		if got := ResolveBotToken("primary", "", secrets); got != "token-primary-123" {
			t.Errorf("ResolveBotToken(primary) = %q, want %q", got, "token-primary-123")
		}
		if got := ResolveBotToken("scout", "", secrets); got != "token-scout-456" {
			t.Errorf("ResolveBotToken(scout) = %q, want %q", got, "token-scout-456")
		}
	})

	t.Run("custom bot_secret override", func(t *testing.T) {
		secrets := mockSecrets{
			"custom.key": "token-custom-789",
		}

		if got := ResolveBotToken("mybot", "custom.key", secrets); got != "token-custom-789" {
			t.Errorf("ResolveBotToken(mybot, custom.key) = %q, want %q", got, "token-custom-789")
		}
	})

	t.Run("empty botName returns empty", func(t *testing.T) {
		secrets := mockSecrets{}

		if got := ResolveBotToken("", "", secrets); got != "" {
			t.Errorf("ResolveBotToken(\"\") = %q, want empty", got)
		}
	})

	t.Run("missing secret returns empty", func(t *testing.T) {
		secrets := mockSecrets{}

		if got := ResolveBotToken("anything", "", secrets); got != "" {
			t.Errorf("ResolveBotToken(anything) = %q, want empty", got)
		}
	})
}

func TestMultiAgentSessionKeys(t *testing.T) {
	// Proves that a multi-agent config produces distinct session key namespaces per
	// agent, and that bot token resolution maps each agent's telegram_bot to a unique
	// secret, including facet bots resolving independently.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[[agents]]
id = "clutch"
model = "anthropic/claude-sonnet-4-6"
workspace = "/tmp/ws1"

[agents.platforms.telegram]
bot = "primary"
facet_bots = ["secondary"]

[[agents]]
id = "scout"
workspace = "/tmp/ws2"

[agents.platforms.telegram]
bot = "scout"

[telegram]
allowed_users = ["111"]
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Verify session key patterns that main.go will generate
	for _, acfg := range cfg.Agents {
		mainKey := acfg.ID + "/i0/0"
		wakeKey := acfg.ID + "/icron-wake-12345/0"
		facetKey := acfg.ID + "/if-12345/0"

		// Ensure agent IDs produce distinct namespaces
		if acfg.ID == "clutch" {
			if mainKey != "clutch/i0/0" {
				t.Errorf("clutch mainKey = %q", mainKey)
			}
			if wakeKey != "clutch/icron-wake-12345/0" {
				t.Errorf("clutch wakeKey = %q", wakeKey)
			}
			if facetKey != "clutch/if-12345/0" {
				t.Errorf("clutch facetKey = %q", facetKey)
			}
			tg := acfg.GetTelegramPlatform()
			if tg == nil {
				t.Fatal("clutch: GetTelegramPlatform() = nil")
			}
			if len(tg.FacetBots) != 1 || tg.FacetBots[0] != "secondary" {
				t.Errorf("clutch FacetBots = %v, want [secondary]", tg.FacetBots)
			}
		}
		if acfg.ID == "scout" {
			if mainKey != "scout/i0/0" {
				t.Errorf("scout mainKey = %q", mainKey)
			}
			tg := acfg.GetTelegramPlatform()
			if tg != nil && len(tg.FacetBots) != 0 {
				t.Errorf("scout FacetBots = %v, want empty", tg.FacetBots)
			}
		}
	}

	// Verify bot token resolution would work with correct secrets
	secrets := mockSecrets{
		"telegram.primary":   "token-primary",
		"telegram.secondary": "token-secondary",
		"telegram.scout":     "token-scout",
	}

	// Each agent's bot should resolve to a different token
	tg0 := cfg.Agents[0].GetTelegramPlatform()
	tg1 := cfg.Agents[1].GetTelegramPlatform()
	clutchToken := ResolveBotToken(tg0.Bot, tg0.BotSecret, secrets)
	scoutToken := ResolveBotToken(tg1.Bot, tg1.BotSecret, secrets)

	if clutchToken == scoutToken {
		t.Errorf("clutch and scout resolved to same token: %q", clutchToken)
	}
	if clutchToken != "token-primary" {
		t.Errorf("clutch token = %q, want token-primary", clutchToken)
	}
	if scoutToken != "token-scout" {
		t.Errorf("scout token = %q, want token-scout", scoutToken)
	}

	// Facet bot should resolve differently from primary
	facetToken := ResolveBotToken(tg0.FacetBots[0], "", secrets)
	if facetToken != "token-secondary" {
		t.Errorf("facet token = %q, want token-secondary", facetToken)
	}
}

func TestAgentTTSRateRecognized(t *testing.T) {
	// Proves that tts_rate is recognized as a valid agent config field, not
	// flagged as an unknown key, and correctly decoded into the TTSRate field.
	tomlData := `
[[agents]]
id = "clutch"
tts_rate = 1.3
`
	var cfg Config
	md, err := tomlParser.Decode(tomlData, &cfg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	keys := UnknownKeys(md)
	for _, k := range keys {
		if strings.Contains(k, "tts_rate") {
			t.Errorf("tts_rate should not be flagged as unknown, got: %v", keys)
		}
	}

	if len(cfg.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(cfg.Agents))
	}
	if cfg.Agents[0].TTSRate != 1.3 {
		t.Errorf("TTSRate = %v, want 1.3", cfg.Agents[0].TTSRate)
	}
}

func TestAgentNameDefault(t *testing.T) {
	// Proves that an agent's display name defaults to the title-cased version of
	// its ID when no explicit name is configured, and that explicit names are preserved.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
[[agents]]
id = "clutch"

[[agents]]
id = "scout"
name = "Scout Override"
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agents[0].Name != "Clutch" {
		t.Errorf("Agents[0].Name = %q, want %q", cfg.Agents[0].Name, "Clutch")
	}
	if cfg.Agents[1].Name != "Scout Override" {
		t.Errorf("Agents[1].Name = %q, want %q", cfg.Agents[1].Name, "Scout Override")
	}
}

func TestAgentMemorySourcesDefault(t *testing.T) {
	// Proves that an agent without explicit memory sources gets a default source
	// pointing to <workspace>/memory, while an agent with explicit sources retains
	// those and does not get the default.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
[[agents]]
id = "clutch"
workspace = "/home/foci/clutch"

[[agents]]
id = "scout"
workspace = "/home/foci/scout"

[[agents.memory.sources]]
name = "custom"
dir = "/custom/memory"
weight = 0.5
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// clutch: should get default memory source
	if len(cfg.Agents[0].Memory.Sources) != 1 {
		t.Fatalf("Agents[0].Memory.Sources len = %d, want 1", len(cfg.Agents[0].Memory.Sources))
	}
	src := cfg.Agents[0].Memory.Sources[0]
	if src.Name != "clutch" {
		t.Errorf("default source name = %q, want %q", src.Name, "clutch")
	}
	if src.Dir != "/home/foci/clutch/memory" {
		t.Errorf("default source dir = %q, want %q", src.Dir, "/home/foci/clutch/memory")
	}
	if src.Weight != 1.0 {
		t.Errorf("default source weight = %f, want 1.0", src.Weight)
	}

	// scout: should keep explicit sources
	if len(cfg.Agents[1].Memory.Sources) != 1 {
		t.Fatalf("Agents[1].Memory.Sources len = %d, want 1", len(cfg.Agents[1].Memory.Sources))
	}
	if cfg.Agents[1].Memory.Sources[0].Name != "custom" {
		t.Errorf("explicit source name = %q, want %q", cfg.Agents[1].Memory.Sources[0].Name, "custom")
	}
}

func TestBraindeadThresholdDefault(t *testing.T) {
	// Proves that braindead_threshold defaults to 10 when not explicitly configured.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
[[agents]]
id = "test"
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agents[0].BraindeadThreshold != 10 {
		t.Errorf("BraindeadThreshold = %d, want 10", cfg.Agents[0].BraindeadThreshold)
	}
}

func TestBraindeadThresholdExplicit(t *testing.T) {
	// Proves that explicitly setting braindead_threshold and braindead_prompt in
	// an agent block is correctly loaded and overrides any default.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
[[agents]]
id = "test"
braindead_threshold = 5
braindead_prompt = "custom warning"
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agents[0].BraindeadThreshold != 5 {
		t.Errorf("BraindeadThreshold = %d, want 5", cfg.Agents[0].BraindeadThreshold)
	}
	if cfg.Agents[0].BraindeadPrompt != "custom warning" {
		t.Errorf("BraindeadPrompt = %q, want %q", cfg.Agents[0].BraindeadPrompt, "custom warning")
	}
}

func TestBraindeadThresholdPerAgent(t *testing.T) {
	// Proves that a global braindead_threshold in [defaults] is inherited by agents
	// that don't override it, while agents with explicit values keep their own.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
[defaults]
braindead_threshold = 15
braindead_prompt = "defaults prompt"

[[agents]]
id = "a"

[[agents]]
id = "b"
braindead_threshold = 5
braindead_prompt = "agent prompt"
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Agent "a" inherits from defaults
	if cfg.Agents[0].BraindeadThreshold != 15 {
		t.Errorf("agent a threshold = %d, want 15", cfg.Agents[0].BraindeadThreshold)
	}
	if cfg.Agents[0].BraindeadPrompt != "defaults prompt" {
		t.Errorf("agent a prompt = %q, want %q", cfg.Agents[0].BraindeadPrompt, "defaults prompt")
	}

	// Agent "b" overrides
	if cfg.Agents[1].BraindeadThreshold != 5 {
		t.Errorf("agent b threshold = %d, want 5", cfg.Agents[1].BraindeadThreshold)
	}
	if cfg.Agents[1].BraindeadPrompt != "agent prompt" {
		t.Errorf("agent b prompt = %q, want %q", cfg.Agents[1].BraindeadPrompt, "agent prompt")
	}
}

func TestBraindeadThresholdDisabled(t *testing.T) {
	// Proves that setting braindead_threshold = 0 in [defaults] disables the
	// feature (threshold remains 0, not overridden by the built-in default).
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
[defaults]
braindead_threshold = 0

[[agents]]
id = "test"
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agents[0].BraindeadThreshold != 0 {
		t.Errorf("BraindeadThreshold = %d, want 0 (disabled)", cfg.Agents[0].BraindeadThreshold)
	}
}

func TestAgentExplicitZeroNotOverwritten(t *testing.T) {
	// An agent that explicitly sets braindead_threshold = 0 should NOT
	// have it overwritten by the defaults value. This tests the IsDefined
	// fix in the reflect-based defaults waterfall.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
[defaults]
braindead_threshold = 15

[[agents]]
id = "explicit-zero"
braindead_threshold = 0

[[agents]]
id = "inherits"
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Agent that explicitly set 0 should keep 0
	if cfg.Agents[0].BraindeadThreshold != 0 {
		t.Errorf("explicit-zero agent: BraindeadThreshold = %d, want 0", cfg.Agents[0].BraindeadThreshold)
	}

	// Agent that didn't set it should inherit 15
	if cfg.Agents[1].BraindeadThreshold != 15 {
		t.Errorf("inherits agent: BraindeadThreshold = %d, want 15", cfg.Agents[1].BraindeadThreshold)
	}
}
