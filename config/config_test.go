package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	tomlParser "github.com/BurntSushi/toml"
)

func TestLoadFullConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	toml := `
[agent]
id = "main"
model = "claude-haiku-4-5"
workspace = "/tmp/workspace"


[anthropic]
token = "sk-ant-oat01-test"
brave_api_key = "brave-key"

[telegram]
bot_token = "123:ABC"
allowed_users = ["111", "222"]

[sessions]
dir = "/tmp/sessions"
compaction_threshold = 0.7

[memory]
dir = "/tmp/memory"

[http]
port = 9999
bind = "0.0.0.0"

[logging]
level = "DEBUG"
event_file = "/tmp/foci.log"
api_file = "/tmp/api.jsonl"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agent.ID != "main" {
		t.Errorf("Agent.ID = %q, want %q", cfg.Agent.ID, "main")
	}
	if cfg.Agent.Model != "claude-haiku-4-5" {
		t.Errorf("Agent.Model = %q, want %q", cfg.Agent.Model, "claude-haiku-4-5")
	}
	if cfg.Agent.Workspace != "/tmp/workspace" {
		t.Errorf("Agent.Workspace = %q", cfg.Agent.Workspace)
	}
	if cfg.Anthropic.Token != "sk-ant-oat01-test" {
		t.Errorf("Anthropic.Token = %q", cfg.Anthropic.Token)
	}
	if cfg.Anthropic.BraveAPIKey != "brave-key" {
		t.Errorf("Anthropic.BraveAPIKey = %q", cfg.Anthropic.BraveAPIKey)
	}
	if cfg.Telegram.BotToken != "123:ABC" {
		t.Errorf("Telegram.BotToken = %q", cfg.Telegram.BotToken)
	}
	if len(cfg.Telegram.AllowedUsers) != 2 || cfg.Telegram.AllowedUsers[0] != "111" {
		t.Errorf("Telegram.AllowedUsers = %v", cfg.Telegram.AllowedUsers)
	}
	if cfg.Sessions.Dir != "/tmp/sessions" {
		t.Errorf("Sessions.Dir = %q", cfg.Sessions.Dir)
	}
	if cfg.Sessions.CompactionThreshold != 0.7 {
		t.Errorf("Sessions.CompactionThreshold = %f, want 0.7", cfg.Sessions.CompactionThreshold)
	}
	if cfg.Memory.Dir != "/tmp/memory" {
		t.Errorf("Memory.Dir = %q", cfg.Memory.Dir)
	}
	if cfg.HTTP.Port != 9999 {
		t.Errorf("HTTP.Port = %d, want 9999", cfg.HTTP.Port)
	}
	if cfg.HTTP.Bind != "0.0.0.0" {
		t.Errorf("HTTP.Bind = %q", cfg.HTTP.Bind)
	}
	if cfg.Logging.Level != "DEBUG" {
		t.Errorf("Logging.Level = %q, want %q", cfg.Logging.Level, "DEBUG")
	}
	if cfg.Logging.EventFile != "/tmp/foci.log" {
		t.Errorf("Logging.EventFile = %q", cfg.Logging.EventFile)
	}
	if cfg.Logging.APIFile != "/tmp/api.jsonl" {
		t.Errorf("Logging.APIFile = %q", cfg.Logging.APIFile)
	}
}

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	// Minimal config — only required fields
	toml := `
[agent]
id = "test"

[anthropic]
token = "test-token"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agent.Model != "claude-haiku-4-5" {
		t.Errorf("default Model = %q, want %q", cfg.Agent.Model, "claude-haiku-4-5")
	}
	if cfg.Sessions.CompactionThreshold != 0.8 {
		t.Errorf("default CompactionThreshold = %f, want 0.8", cfg.Sessions.CompactionThreshold)
	}
	if cfg.HTTP.Port != 18791 {
		t.Errorf("default HTTP.Port = %d, want 18791", cfg.HTTP.Port)
	}
	if cfg.HTTP.Bind != "127.0.0.1" {
		t.Errorf("default HTTP.Bind = %q, want %q", cfg.HTTP.Bind, "127.0.0.1")
	}
	if cfg.Logging.Level != "INFO" {
		t.Errorf("default Logging.Level = %q, want %q", cfg.Logging.Level, "INFO")
	}
	home, _ := os.UserHomeDir()
	wantEventFile := filepath.Join(home, "logs/foci.log")
	if cfg.Logging.EventFile != wantEventFile {
		t.Errorf("default Logging.EventFile = %q, want %q", cfg.Logging.EventFile, wantEventFile)
	}
	wantAPIFile := filepath.Join(home, "logs/api.jsonl")
	if cfg.Logging.APIFile != wantAPIFile {
		t.Errorf("default Logging.APIFile = %q, want %q", cfg.Logging.APIFile, wantAPIFile)
	}
	if cfg.ManaWarnings.Name != "mana" {
		t.Errorf("default ManaWarnings.Name = %q, want %q", cfg.ManaWarnings.Name, "mana")
	}
}

func TestLoadCustomManaName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"
[anthropic]
token = "test-token"

[usage_warnings]
name = "juice"
thresholds = [50, 25, 10]
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ManaWarnings.Name != "juice" {
		t.Errorf("ManaWarnings.Name = %q, want %q", cfg.ManaWarnings.Name, "juice")
	}
	if len(cfg.ManaWarnings.Thresholds) != 3 {
		t.Errorf("len(Thresholds) = %d, want 3", len(cfg.ManaWarnings.Thresholds))
	}
}

func TestLoadCustomCommands(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"
[anthropic]
token = "test-token"

[[commands]]
name = "usage"
description = "Show API usage"
script = "jq '.cost_usd' api.jsonl"

[[commands]]
name = "health"
description = "Health check"
script = "~/scripts/health.sh"
timeout = 30
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(cfg.Commands) != 2 {
		t.Fatalf("Commands len = %d, want 2", len(cfg.Commands))
	}
	if cfg.Commands[0].Name != "usage" {
		t.Errorf("Commands[0].Name = %q", cfg.Commands[0].Name)
	}
	if cfg.Commands[0].Script != "jq '.cost_usd' api.jsonl" {
		t.Errorf("Commands[0].Script = %q", cfg.Commands[0].Script)
	}
	if cfg.Commands[1].Timeout != 30 {
		t.Errorf("Commands[1].Timeout = %d, want 30", cfg.Commands[1].Timeout)
	}
}

func TestLoadSingleAgentBackwardCompat(t *testing.T) {
	// Old [agent] format should populate Agents slice
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "main"
model = "claude-sonnet-4-6"
workspace = "/tmp/workspace"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Agents slice should be populated from [agent]
	if len(cfg.Agents) != 1 {
		t.Fatalf("Agents len = %d, want 1", len(cfg.Agents))
	}
	if cfg.Agents[0].ID != "main" {
		t.Errorf("Agents[0].ID = %q, want %q", cfg.Agents[0].ID, "main")
	}
	if cfg.Agents[0].Model != "claude-sonnet-4-6" {
		t.Errorf("Agents[0].Model = %q, want %q", cfg.Agents[0].Model, "claude-sonnet-4-6")
	}

	// cfg.Agent should mirror first agent
	if cfg.Agent.ID != "main" {
		t.Errorf("Agent.ID = %q, want %q", cfg.Agent.ID, "main")
	}
}

func TestLoadMultiAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[[agents]]
id = "clutch"
model = "claude-sonnet-4-6"
workspace = "/home/rich/workspace1"

telegram_bot = "primary"
multiball_bots = ["secondary"]

[[agents]]
id = "scout"
workspace = "/home/rich/workspace2"
telegram_bot = "scout"

[telegram]
allowed_users = ["111"]

[telegram.bots]
primary = { token_secret = "telegram.primary" }
secondary = { token_secret = "telegram.secondary" }
scout = { token_secret = "telegram.scout" }
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Two agents
	if len(cfg.Agents) != 2 {
		t.Fatalf("Agents len = %d, want 2", len(cfg.Agents))
	}

	// First agent
	if cfg.Agents[0].ID != "clutch" {
		t.Errorf("Agents[0].ID = %q", cfg.Agents[0].ID)
	}
	if cfg.Agents[0].Model != "claude-sonnet-4-6" {
		t.Errorf("Agents[0].Model = %q", cfg.Agents[0].Model)
	}
	if cfg.Agents[0].TelegramBot != "primary" {
		t.Errorf("Agents[0].TelegramBot = %q", cfg.Agents[0].TelegramBot)
	}
	if len(cfg.Agents[0].MultiballBots) != 1 || cfg.Agents[0].MultiballBots[0] != "secondary" {
		t.Errorf("Agents[0].MultiballBots = %v, want [secondary]", cfg.Agents[0].MultiballBots)
	}

	// Second agent — defaults applied
	if cfg.Agents[1].ID != "scout" {
		t.Errorf("Agents[1].ID = %q", cfg.Agents[1].ID)
	}
	if cfg.Agents[1].Model != "claude-haiku-4-5" {
		t.Errorf("Agents[1].Model = %q, want default", cfg.Agents[1].Model)
	}
	if cfg.Agents[1].TelegramBot != "scout" {
		t.Errorf("Agents[1].TelegramBot = %q", cfg.Agents[1].TelegramBot)
	}
	if len(cfg.Agents[1].MultiballBots) != 0 {
		t.Errorf("Agents[1].MultiballBots = %v, want empty", cfg.Agents[1].MultiballBots)
	}

	// cfg.Agent should mirror first agent
	if cfg.Agent.ID != "clutch" {
		t.Errorf("Agent.ID = %q, want %q", cfg.Agent.ID, "clutch")
	}

	// Telegram bots map
	if len(cfg.Telegram.Bots) != 3 {
		t.Fatalf("Telegram.Bots len = %d, want 3", len(cfg.Telegram.Bots))
	}
	if cfg.Telegram.Bots["primary"].TokenSecret != "telegram.primary" {
		t.Errorf("Bots[primary].TokenSecret = %q", cfg.Telegram.Bots["primary"].TokenSecret)
	}
	if cfg.Telegram.Bots["scout"].TokenSecret != "telegram.scout" {
		t.Errorf("Bots[scout].TokenSecret = %q", cfg.Telegram.Bots["scout"].TokenSecret)
	}
}

func TestLoadPerAgentUsageWarnings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[usage_warnings]
thresholds = [50, 25, 10]

[[agents]]
id = "main"

[agents.usage_warnings]
thresholds = [5]

[[agents]]
id = "other"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// First agent should have per-agent thresholds
	if len(cfg.Agents[0].UsageWarnings.Thresholds) != 1 || cfg.Agents[0].UsageWarnings.Thresholds[0] != 5 {
		t.Errorf("Agents[0].UsageWarnings.Thresholds = %v, want [5]", cfg.Agents[0].UsageWarnings.Thresholds)
	}

	// Second agent should have no per-agent thresholds (falls back to global)
	if len(cfg.Agents[1].UsageWarnings.Thresholds) != 0 {
		t.Errorf("Agents[1].UsageWarnings.Thresholds = %v, want []", cfg.Agents[1].UsageWarnings.Thresholds)
	}

	// Global should still be set
	if len(cfg.ManaWarnings.Thresholds) != 3 {
		t.Errorf("ManaWarnings.Thresholds = %v, want [50, 25, 10]", cfg.ManaWarnings.Thresholds)
	}
}

func TestLoadAgentsIgnoresLegacyWhenBothPresent(t *testing.T) {
	// If both [agent] and [[agents]] are present, [[agents]] wins
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "ignored"

[[agents]]
id = "used"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(cfg.Agents) != 1 {
		t.Fatalf("Agents len = %d, want 1", len(cfg.Agents))
	}
	if cfg.Agents[0].ID != "used" {
		t.Errorf("Agents[0].ID = %q, want %q", cfg.Agents[0].ID, "used")
	}
	// cfg.Agent should be the first from [[agents]], not the [agent] block
	if cfg.Agent.ID != "used" {
		t.Errorf("Agent.ID = %q, want %q", cfg.Agent.ID, "used")
	}
}

// mockSecrets implements config.SecretGetter for testing.
type mockSecrets map[string]string

func (m mockSecrets) Get(key string) (string, bool) {
	v, ok := m[key]
	return v, ok
}

func TestResolveBotToken(t *testing.T) {
	t.Run("new format: telegram.bots map + secrets", func(t *testing.T) {
		cfg := &Config{
			Telegram: TelegramConfig{
				Bots: map[string]TelegramBotConfig{
					"primary": {TokenSecret: "telegram.primary"},
					"scout":   {TokenSecret: "telegram.scout"},
				},
			},
		}
		secrets := mockSecrets{
			"telegram.primary": "token-primary-123",
			"telegram.scout":   "token-scout-456",
		}

		if got := cfg.ResolveBotToken("primary", secrets); got != "token-primary-123" {
			t.Errorf("ResolveBotToken(primary) = %q, want %q", got, "token-primary-123")
		}
		if got := cfg.ResolveBotToken("scout", secrets); got != "token-scout-456" {
			t.Errorf("ResolveBotToken(scout) = %q, want %q", got, "token-scout-456")
		}
	})

	t.Run("legacy format: telegram.bot_token in secrets", func(t *testing.T) {
		cfg := &Config{
			Telegram: TelegramConfig{
				BotToken: "config-token",
			},
		}
		secrets := mockSecrets{
			"telegram.bot_token": "secret-token",
		}

		// Unknown bot name falls through to legacy
		if got := cfg.ResolveBotToken("anything", secrets); got != "secret-token" {
			t.Errorf("ResolveBotToken(anything) = %q, want %q", got, "secret-token")
		}
	})

	t.Run("legacy format: telegram.bot_token in config", func(t *testing.T) {
		cfg := &Config{
			Telegram: TelegramConfig{
				BotToken: "config-token",
			},
		}
		secrets := mockSecrets{}

		if got := cfg.ResolveBotToken("anything", secrets); got != "config-token" {
			t.Errorf("ResolveBotToken(anything) = %q, want %q", got, "config-token")
		}
	})
}

func TestMultiAgentSessionKeys(t *testing.T) {
	// Verify that multi-agent config produces correct session key namespaces
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[[agents]]
id = "clutch"
model = "claude-sonnet-4-6"
workspace = "/tmp/ws1"
telegram_bot = "primary"
multiball_bots = ["secondary"]

[[agents]]
id = "scout"
workspace = "/tmp/ws2"
telegram_bot = "scout"

[telegram]
allowed_users = ["111"]

[telegram.bots]
primary = { token_secret = "telegram.primary" }
secondary = { token_secret = "telegram.secondary" }
scout = { token_secret = "telegram.scout" }
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Verify session key patterns that main.go will generate
	for _, acfg := range cfg.Agents {
		mainKey := "agent:" + acfg.ID + ":main"
		wakeKey := "agent:" + acfg.ID + ":cron:wake-12345"
		mbKey := "agent:" + acfg.ID + ":multiball:mb-12345"

		// Ensure agent IDs produce distinct namespaces
		if acfg.ID == "clutch" {
			if mainKey != "agent:clutch:main" {
				t.Errorf("clutch mainKey = %q", mainKey)
			}
			if wakeKey != "agent:clutch:cron:wake-12345" {
				t.Errorf("clutch wakeKey = %q", wakeKey)
			}
			if mbKey != "agent:clutch:multiball:mb-12345" {
				t.Errorf("clutch mbKey = %q", mbKey)
			}
			if len(acfg.MultiballBots) != 1 || acfg.MultiballBots[0] != "secondary" {
				t.Errorf("clutch MultiballBots = %v, want [secondary]", acfg.MultiballBots)
			}
		}
		if acfg.ID == "scout" {
			if mainKey != "agent:scout:main" {
				t.Errorf("scout mainKey = %q", mainKey)
			}
			if len(acfg.MultiballBots) != 0 {
				t.Errorf("scout MultiballBots = %v, want empty", acfg.MultiballBots)
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
	clutchToken := cfg.ResolveBotToken(cfg.Agents[0].TelegramBot, secrets)
	scoutToken := cfg.ResolveBotToken(cfg.Agents[1].TelegramBot, secrets)

	if clutchToken == scoutToken {
		t.Errorf("clutch and scout resolved to same token: %q", clutchToken)
	}
	if clutchToken != "token-primary" {
		t.Errorf("clutch token = %q, want token-primary", clutchToken)
	}
	if scoutToken != "token-scout" {
		t.Errorf("scout token = %q, want token-scout", scoutToken)
	}

	// Multiball bot should resolve differently from primary
	mbToken := cfg.ResolveBotToken(cfg.Agents[0].MultiballBots[0], secrets)
	if mbToken != "token-secondary" {
		t.Errorf("multiball token = %q, want token-secondary", mbToken)
	}
}

func TestUnknownKeysDetected(t *testing.T) {
	tomlData := `
[agent]
id = "main"
bogus_field = "oops"

[unknown_section]
foo = "bar"
some_key = "value"
`
	var cfg Config
	md, err := tomlParser.Decode(tomlData, &cfg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	keys := UnknownKeys(md)
	if len(keys) == 0 {
		t.Fatal("expected unknown keys, got none")
	}

	sort.Strings(keys)
	expected := []string{"agent.bogus_field", "unknown_section", "unknown_section.foo", "unknown_section.some_key"}
	sort.Strings(expected)

	if len(keys) != len(expected) {
		t.Fatalf("unknown keys = %v, want %v", keys, expected)
	}
	for i, k := range keys {
		if k != expected[i] {
			t.Errorf("keys[%d] = %q, want %q", i, k, expected[i])
		}
	}
}

func TestLoadWarnsUnknownKeys(t *testing.T) {
	// Load should succeed even with unknown keys (just warns)
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	toml := `
[agent]
id = "main"

[unknown_section]
foo = "bar"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.ID != "main" {
		t.Errorf("Agent.ID = %q, want %q", cfg.Agent.ID, "main")
	}
}

func TestAgentTTSRateRecognized(t *testing.T) {
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

func TestLoadTelegramToggleDefaults(t *testing.T) {
	// When not set, both toggles default to true
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Telegram.EnableStopAliases {
		t.Error("EnableStopAliases should default to true")
	}
	if !cfg.Telegram.EnableStartupNotify {
		t.Error("EnableStartupNotify should default to true")
	}
}

func TestLoadTelegramTogglesExplicitFalse(t *testing.T) {
	// When explicitly set to false, they stay false
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"

[telegram]
enable_stop_aliases = false
enable_startup_notify = false
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Telegram.EnableStopAliases {
		t.Error("EnableStopAliases should be false when explicitly set")
	}
	if cfg.Telegram.EnableStartupNotify {
		t.Error("EnableStartupNotify should be false when explicitly set")
	}
}

func TestAgentStartupNotification(t *testing.T) {
	t.Run("defaults to nil", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		toml := `
[[agents]]
id = "test"
`
		os.WriteFile(path, []byte(toml), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Agents[0].StartupNotification != nil {
			t.Error("StartupNotification should default to nil (use global)")
		}
	})

	t.Run("explicit true", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		toml := `
[[agents]]
id = "test"
startup_notification = true
`
		os.WriteFile(path, []byte(toml), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Agents[0].StartupNotification == nil || !*cfg.Agents[0].StartupNotification {
			t.Error("StartupNotification should be true")
		}
	})

	t.Run("explicit false", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		toml := `
[[agents]]
id = "test"
startup_notification = false
`
		os.WriteFile(path, []byte(toml), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Agents[0].StartupNotification == nil || *cfg.Agents[0].StartupNotification {
			t.Error("StartupNotification should be false")
		}
	})
}

func TestValidateCompactionThreshold(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			"threshold too high",
			"[agent]\nid = \"test\"\n[sessions]\ncompaction_threshold = 1.5",
			"compaction_threshold = 1.5",
		},
		{
			"threshold negative",
			"[agent]\nid = \"test\"\n[sessions]\ncompaction_threshold = -0.1",
			"compaction_threshold = -0.1",
		},
		{
			"threshold valid",
			"[agent]\nid = \"test\"\n[sessions]\ncompaction_threshold = 0.7",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(tt.toml), 0644)

			_, err := Load(path)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func TestValidateHTTPPort(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			"port too high",
			"[agent]\nid = \"test\"\n[http]\nport = 70000",
			"port = 70000",
		},
		{
			"port zero",
			// port 0 gets defaulted to 18791, so it should pass
			"[agent]\nid = \"test\"\n[http]\nport = 0",
			"",
		},
		{
			"port valid",
			"[agent]\nid = \"test\"\n[http]\nport = 8080",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(tt.toml), 0644)

			_, err := Load(path)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func TestValidateLoggingLevel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte("[agent]\nid = \"test\"\n[logging]\nlevel = \"BOGUS\""), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid logging level")
	}
	if !strings.Contains(err.Error(), "BOGUS") {
		t.Errorf("error = %q, want mention of BOGUS", err.Error())
	}
}

func TestValidateCacheStrategy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte("[agent]\nid = \"test\"\n[cache]\nstrategy = \"invalid\""), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid cache strategy")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("error = %q, want mention of invalid", err.Error())
	}
}

func TestValidateWarningWindowDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte("[agent]\nid = \"test\"\n[logging]\nwarning_window_duration = \"bogus\""), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid warning_window_duration")
	}
	if !strings.Contains(err.Error(), "warning_window_duration") {
		t.Errorf("error = %q, want mention of warning_window_duration", err.Error())
	}
}

func TestValidateMemorySourceWeight(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"

[[memory.sources]]
name = "bad"
dir = "/tmp"
weight = 2.0
`
	os.WriteFile(path, []byte(toml), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for weight > 1.0")
	}
	if !strings.Contains(err.Error(), "weight") {
		t.Errorf("error = %q, want mention of weight", err.Error())
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/foci.toml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadInvalidTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")
	os.WriteFile(path, []byte("this is not valid toml [[["), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid TOML")
	}
}

func TestLoadMemoryConversationWeightDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Memory.ConversationWeight != 0.1 {
		t.Errorf("ConversationWeight = %f, want default 0.1", cfg.Memory.ConversationWeight)
	}
}

func TestLoadMemoryConversationWeightCustom(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"

[memory]
conversation_weight = 0.25
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Memory.ConversationWeight != 0.25 {
		t.Errorf("ConversationWeight = %f, want 0.25", cfg.Memory.ConversationWeight)
	}
}

func TestValidateMemoryConversationWeight(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			"weight too high",
			"[agent]\nid = \"test\"\n[memory]\nconversation_weight = 1.5",
			"conversation_weight = 1.5",
		},
		{
			"weight negative",
			"[agent]\nid = \"test\"\n[memory]\nconversation_weight = -0.1",
			"conversation_weight = -0.1",
		},
		{
			"weight valid",
			"[agent]\nid = \"test\"\n[memory]\nconversation_weight = 0.5",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(tt.toml), 0644)

			_, err := Load(path)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func TestLoadNewConfigFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	toml := `
[agent]
id = "test"
max_tool_loops = 50
max_output_tokens = 16384

[anthropic]
token = "test-token"
http_timeout = "180s"
usage_api_timeout = "15s"

[telegram]
message_queue_size = 128
long_poll_timeout = "70s"

[http]
graceful_shutdown_timeout = "10s"

[memory]
search_limit = 50

[database]
busy_timeout = "10s"

[tools]
exec_default_timeout = 60
exec_max_output_chars = 200000
tmux_command_timeout = "10s"
web_fetch_timeout = "45s"
web_fetch_max_bytes = 2097152
web_fetch_max_chars = 100000
web_search_timeout = "20s"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agent.MaxToolLoops != 50 {
		t.Errorf("Agent.MaxToolLoops = %d, want 50", cfg.Agent.MaxToolLoops)
	}
	if cfg.Agent.MaxOutputTokens != 16384 {
		t.Errorf("Agent.MaxOutputTokens = %d, want 16384", cfg.Agent.MaxOutputTokens)
	}
	if cfg.Anthropic.HTTPTimeout != "180s" {
		t.Errorf("Anthropic.HTTPTimeout = %q, want 180s", cfg.Anthropic.HTTPTimeout)
	}
	if cfg.Anthropic.UsageAPITimeout != "15s" {
		t.Errorf("Anthropic.UsageAPITimeout = %q, want 15s", cfg.Anthropic.UsageAPITimeout)
	}
	if cfg.Telegram.MessageQueueSize != 128 {
		t.Errorf("Telegram.MessageQueueSize = %d, want 128", cfg.Telegram.MessageQueueSize)
	}
	if cfg.Telegram.LongPollTimeout != "70s" {
		t.Errorf("Telegram.LongPollTimeout = %q, want 70s", cfg.Telegram.LongPollTimeout)
	}
	if cfg.HTTP.GracefulShutdownTimeout != "10s" {
		t.Errorf("HTTP.GracefulShutdownTimeout = %q, want 10s", cfg.HTTP.GracefulShutdownTimeout)
	}
	if cfg.Memory.SearchLimit != 50 {
		t.Errorf("Memory.SearchLimit = %d, want 50", cfg.Memory.SearchLimit)
	}
	if cfg.Database.BusyTimeout != "10s" {
		t.Errorf("Database.BusyTimeout = %q, want 10s", cfg.Database.BusyTimeout)
	}
	if cfg.Tools.ExecDefaultTimeout != 60 {
		t.Errorf("Tools.ExecDefaultTimeout = %d, want 60", cfg.Tools.ExecDefaultTimeout)
	}
	if cfg.Tools.ExecMaxOutputChars != 200000 {
		t.Errorf("Tools.ExecMaxOutputChars = %d, want 200000", cfg.Tools.ExecMaxOutputChars)
	}
	if cfg.Tools.TmuxCommandTimeout != "10s" {
		t.Errorf("Tools.TmuxCommandTimeout = %q, want 10s", cfg.Tools.TmuxCommandTimeout)
	}
	if cfg.Tools.WebFetchTimeout != "45s" {
		t.Errorf("Tools.WebFetchTimeout = %q, want 45s", cfg.Tools.WebFetchTimeout)
	}
	if cfg.Tools.WebFetchMaxBytes != 2097152 {
		t.Errorf("Tools.WebFetchMaxBytes = %d, want 2097152", cfg.Tools.WebFetchMaxBytes)
	}
	if cfg.Tools.WebFetchMaxChars != 100000 {
		t.Errorf("Tools.WebFetchMaxChars = %d, want 100000", cfg.Tools.WebFetchMaxChars)
	}
	if cfg.Tools.WebSearchTimeout != "20s" {
		t.Errorf("Tools.WebSearchTimeout = %q, want 20s", cfg.Tools.WebSearchTimeout)
	}
}

func TestNewConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	toml := `
[agent]
id = "test"

[anthropic]
token = "test-token"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agent.MaxToolLoops != 25 {
		t.Errorf("default Agent.MaxToolLoops = %d, want 25", cfg.Agent.MaxToolLoops)
	}
	if cfg.Agent.MaxOutputTokens != 8192 {
		t.Errorf("default Agent.MaxOutputTokens = %d, want 8192", cfg.Agent.MaxOutputTokens)
	}
	if cfg.Anthropic.HTTPTimeout != "600s" {
		t.Errorf("default Anthropic.HTTPTimeout = %q, want 600s", cfg.Anthropic.HTTPTimeout)
	}
	if cfg.Anthropic.UsageAPITimeout != "10s" {
		t.Errorf("default Anthropic.UsageAPITimeout = %q, want 10s", cfg.Anthropic.UsageAPITimeout)
	}
	if cfg.Telegram.MessageQueueSize != 64 {
		t.Errorf("default Telegram.MessageQueueSize = %d, want 64", cfg.Telegram.MessageQueueSize)
	}
	if cfg.Telegram.LongPollTimeout != "65s" {
		t.Errorf("default Telegram.LongPollTimeout = %q, want 65s", cfg.Telegram.LongPollTimeout)
	}
	if cfg.HTTP.GracefulShutdownTimeout != "30s" {
		t.Errorf("default HTTP.GracefulShutdownTimeout = %q, want 30s", cfg.HTTP.GracefulShutdownTimeout)
	}
	if cfg.Memory.SearchLimit != 20 {
		t.Errorf("default Memory.SearchLimit = %d, want 20", cfg.Memory.SearchLimit)
	}
	if cfg.Database.BusyTimeout != "5s" {
		t.Errorf("default Database.BusyTimeout = %q, want 5s", cfg.Database.BusyTimeout)
	}
	if cfg.Tools.ExecDefaultTimeout != 30 {
		t.Errorf("default Tools.ExecDefaultTimeout = %d, want 30", cfg.Tools.ExecDefaultTimeout)
	}
	if cfg.Tools.ExecMaxOutputChars != 100000 {
		t.Errorf("default Tools.ExecMaxOutputChars = %d, want 100000", cfg.Tools.ExecMaxOutputChars)
	}
	if cfg.Tools.TmuxCommandTimeout != "5s" {
		t.Errorf("default Tools.TmuxCommandTimeout = %q, want 5s", cfg.Tools.TmuxCommandTimeout)
	}
	if cfg.Tools.WebFetchTimeout != "30s" {
		t.Errorf("default Tools.WebFetchTimeout = %q, want 30s", cfg.Tools.WebFetchTimeout)
	}
	if cfg.Tools.WebFetchMaxBytes != 1048576 {
		t.Errorf("default Tools.WebFetchMaxBytes = %d, want 1048576", cfg.Tools.WebFetchMaxBytes)
	}
	if cfg.Tools.WebFetchMaxChars != 50000 {
		t.Errorf("default Tools.WebFetchMaxChars = %d, want 50000", cfg.Tools.WebFetchMaxChars)
	}
	if cfg.Tools.WebSearchTimeout != "15s" {
		t.Errorf("default Tools.WebSearchTimeout = %q, want 15s", cfg.Tools.WebSearchTimeout)
	}
}

func TestResolvePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	// Absolute paths returned as-is
	got := ResolvePath("/absolute/path")
	if got != "/absolute/path" {
		t.Errorf("ResolvePath(/absolute/path) = %q, want /absolute/path", got)
	}

	// Relative paths resolved against home
	got = ResolvePath("relative/path")
	want := filepath.Join(home, "relative/path")
	if got != want {
		t.Errorf("ResolvePath(relative/path) = %q, want %q", got, want)
	}
}

func TestDataPathAbsoluteDataDir(t *testing.T) {
	cfg := &Config{DataDir: "/opt/foci/data"}
	got := cfg.DataPath("memory.db")
	want := "/opt/foci/data/memory.db"
	if got != want {
		t.Errorf("DataPath() = %q, want %q", got, want)
	}
}

func TestDataPathRelativeDataDir(t *testing.T) {
	home, _ := os.UserHomeDir()
	cfg := &Config{DataDir: "mydata"}
	got := cfg.DataPath("state.json")
	want := filepath.Join(home, "mydata", "state.json")
	if got != want {
		t.Errorf("DataPath() = %q, want %q", got, want)
	}
}

func TestDataPathDefault(t *testing.T) {
	home, _ := os.UserHomeDir()
	cfg := &Config{DataDir: ""}
	got := cfg.DataPath("memory.db")
	want := filepath.Join(home, "data", "memory.db")
	if got != want {
		t.Errorf("DataPath() = %q, want %q", got, want)
	}
}

func TestDataPathLoadsFromConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
data_dir = "/opt/foci/data"

[agent]
id = "test"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataDir != "/opt/foci/data" {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, "/opt/foci/data")
	}
	got := cfg.DataPath("memory.db")
	want := "/opt/foci/data/memory.db"
	if got != want {
		t.Errorf("DataPath() = %q, want %q", got, want)
	}
}

func TestPromptFilePathsConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"

[sessions]
session_reset_prompt = "/home/foci/shared/prompts/reset.md"
compaction_summary_prompt = "/home/foci/shared/prompts/compaction-summary.md"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Sessions.SessionResetPrompt != "/home/foci/shared/prompts/reset.md" {
		t.Errorf("SessionResetPrompt = %q", cfg.Sessions.SessionResetPrompt)
	}
	if cfg.Sessions.CompactionSummaryPrompt != "/home/foci/shared/prompts/compaction-summary.md" {
		t.Errorf("CompactionSummaryPrompt = %q", cfg.Sessions.CompactionSummaryPrompt)
	}
}

func TestPromptFilePathsDefaultEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Sessions.SessionResetPrompt != "" {
		t.Errorf("SessionResetPrompt should default to empty, got %q", cfg.Sessions.SessionResetPrompt)
	}
	if cfg.Sessions.CompactionSummaryPrompt != "" {
		t.Errorf("CompactionSummaryPrompt should default to empty, got %q", cfg.Sessions.CompactionSummaryPrompt)
	}
}

func TestResolveAllPaths(t *testing.T) {
	home, _ := os.UserHomeDir()

	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	// Minimal config with no path overrides — all defaults
	toml := `
[agent]
id = "test"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Log files should resolve to $HOME/logs/...
	wantEventFile := filepath.Join(home, "logs/foci.log")
	if cfg.Logging.EventFile != wantEventFile {
		t.Errorf("EventFile = %q, want %q", cfg.Logging.EventFile, wantEventFile)
	}
	wantAPIFile := filepath.Join(home, "logs/api.jsonl")
	if cfg.Logging.APIFile != wantAPIFile {
		t.Errorf("APIFile = %q, want %q", cfg.Logging.APIFile, wantAPIFile)
	}

	// Conversation file should default to $HOME/data/conversation.db
	wantConvFile := filepath.Join(home, "data/conversation.db")
	if cfg.Logging.ConversationFile != wantConvFile {
		t.Errorf("ConversationFile = %q, want %q", cfg.Logging.ConversationFile, wantConvFile)
	}

	// Sessions dir should default to $HOME/data/sessions
	wantSessionsDir := filepath.Join(home, "data/sessions")
	if cfg.Sessions.Dir != wantSessionsDir {
		t.Errorf("Sessions.Dir = %q, want %q", cfg.Sessions.Dir, wantSessionsDir)
	}

	// Welcome file should resolve to $HOME/data/WELCOME.md
	wantWelcome := filepath.Join(home, "data/WELCOME.md")
	if cfg.WelcomeFile != wantWelcome {
		t.Errorf("WelcomeFile = %q, want %q", cfg.WelcomeFile, wantWelcome)
	}
}

func TestResolveAllPathsAbsoluteOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
welcome_file = "/opt/welcome.md"

[agent]
id = "test"

[logging]
event_file = "/var/log/foci.log"
api_file = "/var/log/api.jsonl"
conversation_file = "/var/data/conv.db"

[sessions]
dir = "/var/sessions"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Absolute paths should be preserved
	if cfg.Logging.EventFile != "/var/log/foci.log" {
		t.Errorf("EventFile = %q, want /var/log/foci.log", cfg.Logging.EventFile)
	}
	if cfg.Logging.APIFile != "/var/log/api.jsonl" {
		t.Errorf("APIFile = %q, want /var/log/api.jsonl", cfg.Logging.APIFile)
	}
	if cfg.Logging.ConversationFile != "/var/data/conv.db" {
		t.Errorf("ConversationFile = %q, want /var/data/conv.db", cfg.Logging.ConversationFile)
	}
	if cfg.Sessions.Dir != "/var/sessions" {
		t.Errorf("Sessions.Dir = %q, want /var/sessions", cfg.Sessions.Dir)
	}
	if cfg.WelcomeFile != "/opt/welcome.md" {
		t.Errorf("WelcomeFile = %q, want /opt/welcome.md", cfg.WelcomeFile)
	}
}

func TestLoadMultiballBotsPlural(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[[agents]]
id = "clutch"
telegram_bot = "primary"
multiball_bots = ["mb1", "mb2"]

[telegram]
allowed_users = ["111"]

[telegram.bots]
primary = { token_secret = "telegram.primary" }
mb1 = { token_secret = "telegram.mb1" }
mb2 = { token_secret = "telegram.mb2" }
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(cfg.Agents[0].MultiballBots) != 2 {
		t.Fatalf("MultiballBots len = %d, want 2", len(cfg.Agents[0].MultiballBots))
	}
	if cfg.Agents[0].MultiballBots[0] != "mb1" || cfg.Agents[0].MultiballBots[1] != "mb2" {
		t.Errorf("MultiballBots = %v, want [mb1 mb2]", cfg.Agents[0].MultiballBots)
	}
}

func TestLoadMultiballBotDeprecatedAlias(t *testing.T) {
	// multiball_bot (singular) should be promoted to multiball_bots (plural) with deprecation warning
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[[agents]]
id = "clutch"
telegram_bot = "primary"
multiball_bot = "secondary"

[telegram]
allowed_users = ["111"]

[telegram.bots]
primary = { token_secret = "telegram.primary" }
secondary = { token_secret = "telegram.secondary" }
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// multiball_bot should be promoted to multiball_bots
	if len(cfg.Agents[0].MultiballBots) != 1 {
		t.Fatalf("MultiballBots len = %d, want 1 (promoted from singular)", len(cfg.Agents[0].MultiballBots))
	}
	if cfg.Agents[0].MultiballBots[0] != "secondary" {
		t.Errorf("MultiballBots[0] = %q, want secondary", cfg.Agents[0].MultiballBots[0])
	}
	// Original singular field should still be set
	if cfg.Agents[0].MultiballBot != "secondary" {
		t.Errorf("MultiballBot = %q, want secondary", cfg.Agents[0].MultiballBot)
	}
}

func TestLoadMultiballBotPluralTakesPrecedence(t *testing.T) {
	// If both multiball_bot and multiball_bots are set, plural wins
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[[agents]]
id = "clutch"
telegram_bot = "primary"
multiball_bot = "old"
multiball_bots = ["new1", "new2"]

[telegram]
allowed_users = ["111"]

[telegram.bots]
primary = { token_secret = "telegram.primary" }
old = { token_secret = "telegram.old" }
new1 = { token_secret = "telegram.new1" }
new2 = { token_secret = "telegram.new2" }
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Plural should take precedence — no promotion of singular
	if len(cfg.Agents[0].MultiballBots) != 2 {
		t.Fatalf("MultiballBots len = %d, want 2", len(cfg.Agents[0].MultiballBots))
	}
	if cfg.Agents[0].MultiballBots[0] != "new1" {
		t.Errorf("MultiballBots[0] = %q, want new1", cfg.Agents[0].MultiballBots[0])
	}
}

func TestLoadSharedMultiballBots(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[[agents]]
id = "clutch"
telegram_bot = "primary"

[telegram]
allowed_users = ["111"]
multiball_bots = ["spare1", "spare2"]

[telegram.bots]
primary = { token_secret = "telegram.primary" }
spare1 = { token_secret = "telegram.spare1" }
spare2 = { token_secret = "telegram.spare2" }
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(cfg.Telegram.MultiballBots) != 2 {
		t.Fatalf("Telegram.MultiballBots len = %d, want 2", len(cfg.Telegram.MultiballBots))
	}
	if cfg.Telegram.MultiballBots[0] != "spare1" || cfg.Telegram.MultiballBots[1] != "spare2" {
		t.Errorf("Telegram.MultiballBots = %v, want [spare1 spare2]", cfg.Telegram.MultiballBots)
	}
}

func TestValidateNewDurationFields(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			name: "invalid http_timeout",
			toml: `
[agent]
id = "test"
[anthropic]
token = "test"
http_timeout = "invalid"
`,
			wantErr: "http_timeout",
		},
		{
			name: "invalid database busy_timeout",
			toml: `
[agent]
id = "test"
[anthropic]
token = "test"
[database]
busy_timeout = "invalid"
`,
			wantErr: "busy_timeout",
		},
		{
			name: "invalid telegram long_poll_timeout",
			toml: `
[agent]
id = "test"
[anthropic]
token = "test"
[telegram]
long_poll_timeout = "invalid"
`,
			wantErr: "long_poll_timeout",
		},
		{
			name: "invalid http graceful_shutdown_timeout",
			toml: `
[agent]
id = "test"
[anthropic]
token = "test"
[http]
graceful_shutdown_timeout = "invalid"
`,
			wantErr: "graceful_shutdown_timeout",
		},
		{
			name: "invalid tools tmux_command_timeout",
			toml: `
[agent]
id = "test"
[anthropic]
token = "test"
[tools]
tmux_command_timeout = "invalid"
`,
			wantErr: "tmux_command_timeout",
		},
		{
			name: "invalid tools web_fetch_timeout",
			toml: `
[agent]
id = "test"
[anthropic]
token = "test"
[tools]
web_fetch_timeout = "invalid"
`,
			wantErr: "web_fetch_timeout",
		},
		{
			name: "invalid tools web_search_timeout",
			toml: `
[agent]
id = "test"
[anthropic]
token = "test"
[tools]
web_search_timeout = "invalid"
`,
			wantErr: "web_search_timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "foci.toml")
			os.WriteFile(path, []byte(tt.toml), 0644)

			_, err := Load(path)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func TestCompactionPreserveMessagesConfig(t *testing.T) {
	t.Run("global default", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[agent]
id = "test"
`), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Sessions.CompactionPreserveMessages != 25 {
			t.Errorf("CompactionPreserveMessages = %d, want 25", cfg.Sessions.CompactionPreserveMessages)
		}
	})

	t.Run("global explicit", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[agent]
id = "test"
[sessions]
compaction_preserve_messages = 10
`), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Sessions.CompactionPreserveMessages != 10 {
			t.Errorf("CompactionPreserveMessages = %d, want 10", cfg.Sessions.CompactionPreserveMessages)
		}
	})

	t.Run("global explicit zero", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[agent]
id = "test"
[sessions]
compaction_preserve_messages = 0
`), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Sessions.CompactionPreserveMessages != 0 {
			t.Errorf("CompactionPreserveMessages = %d, want 0", cfg.Sessions.CompactionPreserveMessages)
		}
	})

	t.Run("per-agent override", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[sessions]
compaction_preserve_messages = 10

[[agents]]
id = "a"
compaction_preserve_messages = 5

[[agents]]
id = "b"
`), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Sessions.CompactionPreserveMessages != 10 {
			t.Errorf("global = %d, want 10", cfg.Sessions.CompactionPreserveMessages)
		}
		if cfg.Agents[0].CompactionPreserveMessages == nil || *cfg.Agents[0].CompactionPreserveMessages != 5 {
			t.Errorf("agent a = %v, want 5", cfg.Agents[0].CompactionPreserveMessages)
		}
		if cfg.Agents[1].CompactionPreserveMessages != nil {
			t.Errorf("agent b should be nil, got %v", cfg.Agents[1].CompactionPreserveMessages)
		}
	})

	t.Run("negative rejected", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[agent]
id = "test"
[sessions]
compaction_preserve_messages = -1
`), 0644)

		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for negative value")
		}
		if !strings.Contains(err.Error(), "compaction_preserve_messages") {
			t.Errorf("error = %q, want mention of compaction_preserve_messages", err.Error())
		}
	})
}

func TestMessagesInLogConfig(t *testing.T) {
	t.Run("default false", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[agent]
id = "test"
`), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Logging.MessagesInLog {
			t.Error("MessagesInLog should default to false")
		}
	})

	t.Run("global explicit true", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[agent]
id = "test"
[logging]
messages_in_log = true
`), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.Logging.MessagesInLog {
			t.Error("MessagesInLog should be true")
		}
	})

	t.Run("per-agent override", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[logging]
messages_in_log = false

[[agents]]
id = "a"
messages_in_log = true

[[agents]]
id = "b"
`), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Logging.MessagesInLog {
			t.Error("global should be false")
		}
		if cfg.Agents[0].MessagesInLog == nil || !*cfg.Agents[0].MessagesInLog {
			t.Error("agent a should override to true")
		}
		if cfg.Agents[1].MessagesInLog != nil {
			t.Errorf("agent b should be nil, got %v", cfg.Agents[1].MessagesInLog)
		}
	})
}

func TestLoadThinkingConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	toml := `
[defaults]
thinking = "adaptive"

[[agents]]
id = "smart"

[[agents]]
id = "fast"
thinking = "off"
`
	os.WriteFile(path, []byte(toml), 0644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Agent "smart" should inherit "adaptive" from defaults
	if cfg.Agents[0].Thinking != "adaptive" {
		t.Errorf("agent smart: Thinking = %q, want %q", cfg.Agents[0].Thinking, "adaptive")
	}
	// Agent "fast" should keep its explicit "off"
	if cfg.Agents[1].Thinking != "off" {
		t.Errorf("agent fast: Thinking = %q, want %q", cfg.Agents[1].Thinking, "off")
	}
}

func TestLoadThinkingPerAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	toml := `
[[agents]]
id = "thinker"
thinking = "adaptive"

[[agents]]
id = "default"
`
	os.WriteFile(path, []byte(toml), 0644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agents[0].Thinking != "adaptive" {
		t.Errorf("agent thinker: Thinking = %q, want %q", cfg.Agents[0].Thinking, "adaptive")
	}
	if cfg.Agents[1].Thinking != "" {
		t.Errorf("agent default: Thinking = %q, want empty", cfg.Agents[1].Thinking)
	}
}
