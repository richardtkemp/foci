package config

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	tomlParser "github.com/BurntSushi/toml"
)

func TestLoadFullConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clod.toml")

	toml := `
[agent]
id = "main"
model = "claude-haiku-4-5"
workspace = "/tmp/workspace"
heartbeat_interval = "30m"

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
event_file = "/tmp/clod.log"
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
	if cfg.Agent.HeartbeatInterval != "30m" {
		t.Errorf("Agent.HeartbeatInterval = %q, want %q", cfg.Agent.HeartbeatInterval, "30m")
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
	if cfg.Logging.EventFile != "/tmp/clod.log" {
		t.Errorf("Logging.EventFile = %q", cfg.Logging.EventFile)
	}
	if cfg.Logging.APIFile != "/tmp/api.jsonl" {
		t.Errorf("Logging.APIFile = %q", cfg.Logging.APIFile)
	}
}

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clod.toml")

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
	if cfg.Agent.HeartbeatInterval != "45m" {
		t.Errorf("default HeartbeatInterval = %q, want %q", cfg.Agent.HeartbeatInterval, "45m")
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
	if cfg.Logging.EventFile != "clod.log" {
		t.Errorf("default Logging.EventFile = %q, want %q", cfg.Logging.EventFile, "clod.log")
	}
	if cfg.Logging.APIFile != "api.jsonl" {
		t.Errorf("default Logging.APIFile = %q, want %q", cfg.Logging.APIFile, "api.jsonl")
	}
}

func TestLoadCustomCommands(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clod.toml")
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
	path := filepath.Join(dir, "clod.toml")
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
	path := filepath.Join(dir, "clod.toml")
	toml := `
[[agents]]
id = "clutch"
model = "claude-sonnet-4-6"
workspace = "/home/rich/workspace1"
heartbeat_interval = "30m"
telegram_bot = "primary"
multiball_bot = "secondary"

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
	if cfg.Agents[0].HeartbeatInterval != "30m" {
		t.Errorf("Agents[0].HeartbeatInterval = %q", cfg.Agents[0].HeartbeatInterval)
	}
	if cfg.Agents[0].TelegramBot != "primary" {
		t.Errorf("Agents[0].TelegramBot = %q", cfg.Agents[0].TelegramBot)
	}
	if cfg.Agents[0].MultiballBot != "secondary" {
		t.Errorf("Agents[0].MultiballBot = %q", cfg.Agents[0].MultiballBot)
	}

	// Second agent — defaults applied
	if cfg.Agents[1].ID != "scout" {
		t.Errorf("Agents[1].ID = %q", cfg.Agents[1].ID)
	}
	if cfg.Agents[1].Model != "claude-haiku-4-5" {
		t.Errorf("Agents[1].Model = %q, want default", cfg.Agents[1].Model)
	}
	if cfg.Agents[1].HeartbeatInterval != "45m" {
		t.Errorf("Agents[1].HeartbeatInterval = %q, want default", cfg.Agents[1].HeartbeatInterval)
	}
	if cfg.Agents[1].TelegramBot != "scout" {
		t.Errorf("Agents[1].TelegramBot = %q", cfg.Agents[1].TelegramBot)
	}
	if cfg.Agents[1].MultiballBot != "" {
		t.Errorf("Agents[1].MultiballBot = %q, want empty", cfg.Agents[1].MultiballBot)
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

func TestLoadAgentsIgnoresLegacyWhenBothPresent(t *testing.T) {
	// If both [agent] and [[agents]] are present, [[agents]] wins
	dir := t.TempDir()
	path := filepath.Join(dir, "clod.toml")
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
	path := filepath.Join(dir, "clod.toml")
	toml := `
[[agents]]
id = "clutch"
model = "claude-sonnet-4-6"
workspace = "/tmp/ws1"
telegram_bot = "primary"
multiball_bot = "secondary"

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
			if acfg.MultiballBot != "secondary" {
				t.Errorf("clutch MultiballBot = %q, want secondary", acfg.MultiballBot)
			}
		}
		if acfg.ID == "scout" {
			if mainKey != "agent:scout:main" {
				t.Errorf("scout mainKey = %q", mainKey)
			}
			if acfg.MultiballBot != "" {
				t.Errorf("scout MultiballBot = %q, want empty", acfg.MultiballBot)
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
	mbToken := cfg.ResolveBotToken(cfg.Agents[0].MultiballBot, secrets)
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
	path := filepath.Join(dir, "clod.toml")

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

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/clod.toml")
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
