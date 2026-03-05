package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
model = "anthropic/claude-haiku-4-5"
workspace = "/tmp/workspace"


[telegram]
allowed_users = ["111", "222"]

[sessions]
dir = "/tmp/sessions"
compaction_threshold = 0.7

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
	if cfg.Agent.Model != "anthropic/claude-haiku-4-5" {
		t.Errorf("Agent.Model = %q, want %q", cfg.Agent.Model, "anthropic/claude-haiku-4-5")
	}
	if cfg.Agent.Workspace != "/tmp/workspace" {
		t.Errorf("Agent.Workspace = %q", cfg.Agent.Workspace)
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

	if cfg.Agent.Model != "anthropic/claude-haiku-4-5-20251001" {
		t.Errorf("default Model = %q, want %q", cfg.Agent.Model, "anthropic/claude-haiku-4-5-20251001")
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
model = "anthropic/claude-sonnet-4-6"
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
	if cfg.Agents[0].Model != "anthropic/claude-sonnet-4-6" {
		t.Errorf("Agents[0].Model = %q, want %q", cfg.Agents[0].Model, "anthropic/claude-sonnet-4-6")
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
model = "anthropic/claude-sonnet-4-6"
workspace = "/home/rich/workspace1"

telegram_bot = "primary"
multiball_bots = ["secondary"]

[[agents]]
id = "scout"
workspace = "/home/rich/workspace2"
telegram_bot = "scout"

[telegram]
allowed_users = ["111"]
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
	if cfg.Agents[0].Model != "anthropic/claude-sonnet-4-6" {
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
	if cfg.Agents[1].Model != "anthropic/claude-haiku-4-5-20251001" {
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
	// Verify that multi-agent config produces correct session key namespaces
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[[agents]]
id = "clutch"
model = "anthropic/claude-sonnet-4-6"
workspace = "/tmp/ws1"
telegram_bot = "primary"
multiball_bots = ["secondary"]

[[agents]]
id = "scout"
workspace = "/tmp/ws2"
telegram_bot = "scout"

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
	clutchToken := ResolveBotToken(cfg.Agents[0].TelegramBot, cfg.Agents[0].BotSecret, secrets)
	scoutToken := ResolveBotToken(cfg.Agents[1].TelegramBot, cfg.Agents[1].BotSecret, secrets)

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
	mbToken := ResolveBotToken(cfg.Agents[0].MultiballBots[0], "", secrets)
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
max_summary_chars = 500000
tmux_command_timeout = "10s"
web_fetch_timeout = "45s"
web_fetch_max_bytes = 2097152
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
	if cfg.Tools.MaxSummaryChars != 500000 {
		t.Errorf("Tools.MaxSummaryChars = %d, want 500000", cfg.Tools.MaxSummaryChars)
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
	if cfg.Tools.MaxSummaryChars != 300000 {
		t.Errorf("default Tools.MaxSummaryChars = %d, want 300000", cfg.Tools.MaxSummaryChars)
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
compaction_summary_prompt = "/home/foci/shared/prompts/compaction-summary.md"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
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
[anthropic]
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

	// Provider-section default should be set
	if cfg.Anthropic.Thinking != "adaptive" {
		t.Errorf("anthropic: Thinking = %q, want %q", cfg.Anthropic.Thinking, "adaptive")
	}
	// Agent "smart" has no per-agent override — empty until ApplyProviderDefaults
	if cfg.Agents[0].Thinking != "" {
		t.Errorf("agent smart: Thinking = %q, want %q (empty before ApplyProviderDefaults)", cfg.Agents[0].Thinking, "")
	}
	// Agent "fast" should keep its explicit "off"
	if cfg.Agents[1].Thinking != "off" {
		t.Errorf("agent fast: Thinking = %q, want %q", cfg.Agents[1].Thinking, "off")
	}

	// Simulate main.go calling ApplyProviderDefaults for an Anthropic agent
	ApplyProviderDefaults(&cfg.Agents[0], "anthropic", cfg)
	if cfg.Agents[0].Thinking != "adaptive" {
		t.Errorf("agent smart after ApplyProviderDefaults: Thinking = %q, want %q", cfg.Agents[0].Thinking, "adaptive")
	}
	// Agent "fast" already has explicit "off" — ApplyProviderDefaults should not change it
	ApplyProviderDefaults(&cfg.Agents[1], "anthropic", cfg)
	if cfg.Agents[1].Thinking != "off" {
		t.Errorf("agent fast after ApplyProviderDefaults: Thinking = %q, want %q", cfg.Agents[1].Thinking, "off")
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
	// Agent "default" has no per-agent override — empty after Load()
	// Defaults come from provider section via ApplyProviderDefaults in main.go
	if cfg.Agents[1].Thinking != "" {
		t.Errorf("agent default: Thinking = %q, want %q (empty before ApplyProviderDefaults)", cfg.Agents[1].Thinking, "")
	}
}

func TestApplyProviderDefaults(t *testing.T) {
	cfg := &Config{
		Anthropic: AnthropicConfig{Effort: "low", Thinking: "adaptive"},
		Gemini:    GeminiConfig{Thinking: "adaptive"},
	}

	// Anthropic agent gets both effort and thinking
	agent := AgentConfig{}
	ApplyProviderDefaults(&agent, "anthropic", cfg)
	if agent.Effort != "low" {
		t.Errorf("anthropic effort = %q, want %q", agent.Effort, "low")
	}
	if agent.Thinking != "adaptive" {
		t.Errorf("anthropic thinking = %q, want %q", agent.Thinking, "adaptive")
	}

	// Gemini agent gets thinking but not effort
	agent2 := AgentConfig{}
	ApplyProviderDefaults(&agent2, "gemini", cfg)
	if agent2.Effort != "" {
		t.Errorf("gemini effort = %q, want %q", agent2.Effort, "")
	}
	if agent2.Thinking != "adaptive" {
		t.Errorf("gemini thinking = %q, want %q", agent2.Thinking, "adaptive")
	}

	// OpenAI agent gets neither
	agent3 := AgentConfig{}
	ApplyProviderDefaults(&agent3, "openai", cfg)
	if agent3.Effort != "" {
		t.Errorf("openai effort = %q, want %q", agent3.Effort, "")
	}
	if agent3.Thinking != "" {
		t.Errorf("openai thinking = %q, want %q", agent3.Thinking, "")
	}

	// Per-agent override is preserved
	agent4 := AgentConfig{Effort: "high", Thinking: "off"}
	ApplyProviderDefaults(&agent4, "anthropic", cfg)
	if agent4.Effort != "high" {
		t.Errorf("override effort = %q, want %q", agent4.Effort, "high")
	}
	if agent4.Thinking != "off" {
		t.Errorf("override thinking = %q, want %q", agent4.Thinking, "off")
	}
}

func TestShowToolCallsDisplay(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		want    ToolCallDisplay
		wantErr bool
	}{
		{"bool true", `show_tool_calls = true`, ToolCallPreview, false},
		{"bool false", `show_tool_calls = false`, ToolCallOff, false},
		{"string off", `show_tool_calls = "off"`, ToolCallOff, false},
		{"string preview", `show_tool_calls = "preview"`, ToolCallPreview, false},
		{"string full", `show_tool_calls = "full"`, ToolCallFull, false},
		{"invalid string", `show_tool_calls = "banana"`, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out struct {
				ShowToolCalls ToolCallDisplay `toml:"show_tool_calls"`
			}
			_, err := tomlParser.Decode(tt.toml, &out)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tt.toml)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out.ShowToolCalls != tt.want {
				t.Errorf("ShowToolCalls = %q, want %q", out.ShowToolCalls, tt.want)
			}
		})
	}

	// Per-agent *ToolCallDisplay: non-nil when set, nil when not set.
	t.Run("per-agent set", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`
[[agents]]
id = "a"
show_tool_calls = "full"

[[agents]]
id = "b"
`), 0644)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Agents[0].ShowToolCalls == nil {
			t.Fatal("agent a: ShowToolCalls should be non-nil")
		}
		if *cfg.Agents[0].ShowToolCalls != ToolCallFull {
			t.Errorf("agent a: ShowToolCalls = %q, want %q", *cfg.Agents[0].ShowToolCalls, ToolCallFull)
		}
		// Agent b inherits ShowToolCalls from [defaults] (ToolCallOff)
		if cfg.Agents[1].ShowToolCalls == nil {
			t.Fatal("agent b: ShowToolCalls should be non-nil (inherited from defaults)")
		}
		if *cfg.Agents[1].ShowToolCalls != ToolCallOff {
			t.Errorf("agent b: ShowToolCalls = %q, want %q (inherited from defaults)", *cfg.Agents[1].ShowToolCalls, ToolCallOff)
		}
	})

	// Per-agent with bool backwards compat
	t.Run("per-agent bool compat", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`
[[agents]]
id = "a"
show_tool_calls = true
`), 0644)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Agents[0].ShowToolCalls == nil {
			t.Fatal("agent a: ShowToolCalls should be non-nil")
		}
		if *cfg.Agents[0].ShowToolCalls != ToolCallPreview {
			t.Errorf("agent a: ShowToolCalls = %q, want %q", *cfg.Agents[0].ShowToolCalls, ToolCallPreview)
		}
	})

	// Defaults section
	t.Run("defaults string", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`
[defaults]
show_tool_calls = "full"
`), 0644)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Defaults.ShowToolCalls == nil || *cfg.Defaults.ShowToolCalls != ToolCallFull {
			t.Errorf("Defaults.ShowToolCalls = %v, want %q", cfg.Defaults.ShowToolCalls, ToolCallFull)
		}
	})

	// Global default (not set) — defaults to ToolCallOff
	t.Run("defaults default", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(``), 0644)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Defaults.ShowToolCalls == nil || *cfg.Defaults.ShowToolCalls != ToolCallOff {
			t.Errorf("Defaults.ShowToolCalls = %v, want %q", cfg.Defaults.ShowToolCalls, ToolCallOff)
		}
	})

}

func TestNormalizeBoolStrings(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"on to true", `enabled = "on"`, `enabled = true`},
		{"off to false", `enabled = "off"`, `enabled = false`},
		{"true string to bool", `enabled = "true"`, `enabled = true`},
		{"false string to bool", `enabled = "false"`, `enabled = false`},
		{"case insensitive", `enabled = "ON"`, `enabled = true`},
		{"native bool unchanged", `enabled = true`, `enabled = true`},
		{"with comment", `enabled = "on" # turn on`, `enabled = true # turn on`},
		{"non-bool key preserved", `thinking = "off"`, `thinking = "off"`},
		{"non-bool key on preserved", `thinking = "on"`, `thinking = "on"`},
		{"string value preserved", `name = "hello"`, `name = "hello"`},
		{"url preserved", `endpoint = "https://on.example.com"`, `endpoint = "https://on.example.com"`},
		{"non-bool string preserved", `mode = "preview"`, `mode = "preview"`},
		{"multiline bool keys", "enabled = \"on\"\nlog_rotation = \"off\"\ncache_bust_detect = true", "enabled = true\nlog_rotation = false\ncache_bust_detect = true"},
		{"mixed bool and string keys", "enabled = \"on\"\nthinking = \"off\"", "enabled = true\nthinking = \"off\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeBoolStrings(tt.input)
			if got != tt.want {
				t.Errorf("normalizeBoolStrings(%q)\n  got  %q\n  want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBoolStringConfigLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
[telegram]
enable_stop_aliases = "on"
enable_startup_notify = "off"

[environment]
enabled = "true"

[logging]
log_rotation = "false"
`), 0644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Telegram.EnableStopAliases {
		t.Error("EnableStopAliases should be true (from \"on\")")
	}
	if cfg.Telegram.EnableStartupNotify {
		t.Error("EnableStartupNotify should be false (from \"off\")")
	}
	if !cfg.Environment.Enabled {
		t.Error("Environment.Enabled should be true (from \"true\")")
	}
	if cfg.Logging.LogRotation {
		t.Error("Logging.LogRotation should be false (from \"false\")")
	}
}

func TestDataDirDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
[agent]
id = "test"
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "data")
	if cfg.DataDir != want {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, want)
	}
}

func TestDataDirExplicitNotOverridden(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
data_dir = "/opt/foci/data"

[agent]
id = "test"
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.DataDir != "/opt/foci/data" {
		t.Errorf("DataDir = %q, want /opt/foci/data", cfg.DataDir)
	}
}

func TestAgentNameDefault(t *testing.T) {
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
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
[agent]
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
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
[defaults]
braindead_threshold = 0

[agent]
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

func TestApplyDefaultsReflect(t *testing.T) {
	// Verify that the reflect-based waterfall copies all DefaultsConfig fields.
	// Note: effort and thinking are now in provider sections, not [defaults].
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
[defaults]
model = "anthropic/claude-opus-4-6"
max_tool_loops = 50
max_output_tokens = 16384
braindead_threshold = 20
braindead_prompt = "watch it"
duplicate_messages = true
inject_agent_warnings = true
compaction_effort = "low"
system_files = ["A.md", "B.md"]

[anthropic]
effort = "high"
thinking = "adaptive"

[[agents]]
id = "bare"

[[agents]]
id = "override"
model = "anthropic/claude-haiku-4-5"
effort = "low"
`), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	bare := cfg.Agents[0]
	if bare.Model != "anthropic/claude-opus-4-6" {
		t.Errorf("bare Model = %q", bare.Model)
	}
	if bare.MaxToolLoops != 50 {
		t.Errorf("bare MaxToolLoops = %d", bare.MaxToolLoops)
	}
	if bare.MaxOutputTokens != 16384 {
		t.Errorf("bare MaxOutputTokens = %d", bare.MaxOutputTokens)
	}
	if bare.BraindeadThreshold != 20 {
		t.Errorf("bare BraindeadThreshold = %d", bare.BraindeadThreshold)
	}
	if bare.BraindeadPrompt != "watch it" {
		t.Errorf("bare BraindeadPrompt = %q", bare.BraindeadPrompt)
	}
	// Effort/thinking come from provider sections via ApplyProviderDefaults, not [defaults]
	if bare.Effort != "" {
		t.Errorf("bare Effort = %q, want empty (set via ApplyProviderDefaults)", bare.Effort)
	}
	if bare.Thinking != "" {
		t.Errorf("bare Thinking = %q, want empty (set via ApplyProviderDefaults)", bare.Thinking)
	}
	// Verify ApplyProviderDefaults fills them in
	ApplyProviderDefaults(&bare, "anthropic", cfg)
	if bare.Effort != "high" {
		t.Errorf("bare Effort after ApplyProviderDefaults = %q, want high", bare.Effort)
	}
	if bare.Thinking != "adaptive" {
		t.Errorf("bare Thinking after ApplyProviderDefaults = %q, want adaptive", bare.Thinking)
	}
	if !bare.DuplicateMessages {
		t.Error("bare DuplicateMessages should be true")
	}
	if !bare.InjectAgentWarnings {
		t.Error("bare InjectAgentWarnings should be true")
	}
	if bare.CompactionEffort != "low" {
		t.Errorf("bare CompactionEffort = %q", bare.CompactionEffort)
	}
	if len(bare.SystemFiles) != 2 || bare.SystemFiles[0] != "A.md" {
		t.Errorf("bare SystemFiles = %v", bare.SystemFiles)
	}

	// Override agent keeps its own values
	override := cfg.Agents[1]
	if override.Model != "anthropic/claude-haiku-4-5" {
		t.Errorf("override Model = %q, want anthropic/claude-haiku-4-5", override.Model)
	}
	if override.Effort != "low" {
		t.Errorf("override Effort = %q, want low", override.Effort)
	}
	// But inherits defaults for fields it didn't set
	if override.MaxToolLoops != 50 {
		t.Errorf("override MaxToolLoops = %d, want 50 (from defaults)", override.MaxToolLoops)
	}
}

func TestExampleConfigKeysValid(t *testing.T) {
	// Validates that foci.toml.example contains exactly the right config keys.
	// If you add a new config field, this test will fail until you either:
	//   - Add it to foci.toml.example (if users should know about it)
	//   - Add it to exampleSkipPrefixes below (if it's internal or a duplicate section)

	examplePath := filepath.Join("..", "foci.toml.example")
	raw, err := os.ReadFile(examplePath)
	if err != nil {
		t.Skipf("foci.toml.example not found: %v", err)
	}

	// Uncomment config lines to get the full key set.
	// Only uncomment lines that look like TOML key=value or section headers.
	tomlKeyValue := regexp.MustCompile(`^[a-z][a-z0-9_]*\s*=`)
	tomlSection := regexp.MustCompile(`^\[+[a-z][a-z0-9_.]*\]+$`)
	var uncommented strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		stripped := ""
		if strings.HasPrefix(trimmed, "# ") {
			stripped = strings.TrimSpace(trimmed[2:])
		} else if strings.HasPrefix(trimmed, "#") && len(trimmed) > 1 {
			stripped = strings.TrimSpace(trimmed[1:])
		}
		if stripped != "" {
			if tomlKeyValue.MatchString(stripped) || tomlSection.MatchString(stripped) {
				uncommented.WriteString(stripped)
			} else {
				uncommented.WriteString(line)
			}
		} else {
			uncommented.WriteString(line)
		}
		uncommented.WriteString("\n")
	}

	// Parse the uncommented example — every key must decode into Config.
	var cfg Config
	meta, err := tomlParser.Decode(uncommented.String(), &cfg)
	if err != nil {
		t.Fatalf("foci.toml.example has invalid TOML after uncommenting: %v", err)
	}

	// Check 1: no unknown keys in the example.
	undecoded := meta.Undecoded()
	if len(undecoded) > 0 {
		var keys []string
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		t.Errorf("foci.toml.example contains keys that don't match any Config field:\n  %s\n"+
			"Fix the key names in the example or add the fields to config structs.",
			strings.Join(keys, "\n  "))
	}

	// Check 2: every Config struct field appears in the example.
	structKeys := collectTOMLKeys(reflect.TypeOf(Config{}), "")

	// The legacy [agent] (singular) section has the same fields as [[agents]].
	// The example only shows [[agents]]; skip all agent.* paths.
	exampleSkipPrefixes := []string{"agent."}

	exampleText := string(raw)
	var missing []string
	for _, key := range structKeys {
		skip := false
		for _, prefix := range exampleSkipPrefixes {
			if strings.HasPrefix(key, prefix) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		// Check the leaf key name appears in the raw file (commented or not).
		parts := strings.Split(key, ".")
		leaf := parts[len(parts)-1]
		if !strings.Contains(exampleText, leaf) {
			missing = append(missing, key)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("Config fields missing from foci.toml.example:\n  %s\n"+
			"Add them to the example file, or add to exampleSkipKeys with a reason.",
			strings.Join(missing, "\n  "))
	}
}

// collectTOMLKeys walks a struct type recursively and returns all leaf TOML key paths.
func collectTOMLKeys(t reflect.Type, prefix string) []string {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() == reflect.Slice {
		t = t.Elem()
		if t.Kind() == reflect.Ptr {
			t = t.Elem()
		}
	}
	if t.Kind() != reflect.Struct {
		return nil
	}

	var keys []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		if idx := strings.Index(tag, ","); idx != -1 {
			tag = tag[:idx]
		}

		fullKey := tag
		if prefix != "" {
			fullKey = prefix + "." + tag
		}

		ft := f.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}

		switch {
		case ft.Kind() == reflect.Map:
			// Dynamic keys — include the map itself but not contents
			keys = append(keys, fullKey)
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
			// Slice of structs — recurse into element type
			keys = append(keys, collectTOMLKeys(ft.Elem(), fullKey)...)
		case ft.Kind() == reflect.Struct:
			if ft.Implements(reflect.TypeOf((*tomlParser.Unmarshaler)(nil)).Elem()) ||
				reflect.PointerTo(ft).Implements(reflect.TypeOf((*tomlParser.Unmarshaler)(nil)).Elem()) {
				// Custom unmarshaler (e.g. ToolCallDisplay) — leaf key
				keys = append(keys, fullKey)
			} else {
				keys = append(keys, collectTOMLKeys(ft, fullKey)...)
			}
		default:
			keys = append(keys, fullKey)
		}
	}
	return keys
}

func TestNoSecretsInConfig(t *testing.T) {
	// Config structs must not contain credential fields.
	// Secrets belong in secrets.toml, resolved via the secrets store at runtime.
	secretPatterns := []*regexp.Regexp{
		regexp.MustCompile(`_token$`),  // api_token, setup_token — but not max_output_tokens
		regexp.MustCompile(`_key$`),    // api_key, brave_api_key
		regexp.MustCompile(`password`), // password, password_hash
		regexp.MustCompile(`^key$`),    // bare "key"
		regexp.MustCompile(`^token$`),  // bare "token"
	}

	keys := collectTOMLKeys(reflect.TypeOf(Config{}), "")
	for _, key := range keys {
		parts := strings.Split(key, ".")
		leaf := strings.ToLower(parts[len(parts)-1])
		for _, pat := range secretPatterns {
			if pat.MatchString(leaf) {
				t.Errorf("config field %q matches %s — secrets belong in secrets.toml", key, pat)
			}
		}
	}
}

func TestMemorySourcesInheritance(t *testing.T) {
	t.Run("global sources prepended to agent default", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		toml := `
[[memory.sources]]
name = "shared"
dir = "/shared/memory"
weight = 0.5

[[agents]]
id = "clutch"
workspace = "/ws/clutch"
`
		os.WriteFile(path, []byte(toml), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		sources := cfg.Agents[0].Memory.Sources
		if len(sources) != 2 {
			t.Fatalf("sources len = %d, want 2", len(sources))
		}
		if sources[0].Name != "shared" || sources[0].Dir != "/shared/memory" || sources[0].Weight != 0.5 {
			t.Errorf("sources[0] = %+v, want shared source", sources[0])
		}
		if sources[1].Name != "clutch" || sources[1].Weight != 1.0 {
			t.Errorf("sources[1] = %+v, want agent default source", sources[1])
		}
	})

	t.Run("global sources prepended to agent explicit", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		toml := `
[[memory.sources]]
name = "shared"
dir = "/shared/memory"
weight = 0.5

[[agents]]
id = "clutch"

[[agents.memory.sources]]
name = "custom"
dir = "/custom/memory"
weight = 0.8
`
		os.WriteFile(path, []byte(toml), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		sources := cfg.Agents[0].Memory.Sources
		if len(sources) != 2 {
			t.Fatalf("sources len = %d, want 2", len(sources))
		}
		if sources[0].Name != "shared" {
			t.Errorf("sources[0].Name = %q, want shared", sources[0].Name)
		}
		if sources[1].Name != "custom" || sources[1].Weight != 0.8 {
			t.Errorf("sources[1] = %+v, want custom source", sources[1])
		}
	})

	t.Run("no global sources only agent default", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		toml := `
[[agents]]
id = "clutch"
workspace = "/ws/clutch"
`
		os.WriteFile(path, []byte(toml), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		sources := cfg.Agents[0].Memory.Sources
		if len(sources) != 1 {
			t.Fatalf("sources len = %d, want 1", len(sources))
		}
		if sources[0].Name != "clutch" {
			t.Errorf("sources[0].Name = %q, want clutch", sources[0].Name)
		}
	})
}

func TestSplitDeveloperModelLegacy(t *testing.T) {
	tests := []struct {
		input         string
		wantDeveloper string
		wantModel     string
	}{
		{"anthropic/claude-haiku-4-5", "anthropic", "claude-haiku-4-5"},
		{"google/gemini-2.5-flash", "google", "gemini-2.5-flash"},
		{"openai/gpt-4o", "openai", "gpt-4o"},
		// Bare model name returns empty developer
		{"claude-haiku-4-5", "", "claude-haiku-4-5"},
		// Whitespace trimming
		{"  anthropic/claude-haiku-4-5  ", "anthropic", "claude-haiku-4-5"},
		// Empty input
		{"", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			dev, model := SplitDeveloperModel(tt.input)
			if dev != tt.wantDeveloper {
				t.Errorf("developer = %q, want %q", dev, tt.wantDeveloper)
			}
			if model != tt.wantModel {
				t.Errorf("model = %q, want %q", model, tt.wantModel)
			}
		})
	}
}

func TestInferFormat(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"claude-haiku-4-5", "anthropic"},
		{"claude-opus-4-6", "anthropic"},
		{"claude-sonnet-4-5-20250929", "anthropic"},
		{"gemini-2.5-flash", "gemini"},
		{"gemini-2.5-pro", "gemini"},
		{"gpt-4o", "openai"},
		{"gpt-4o-mini", "openai"},
		{"o3", "openai"},
		{"o3-mini", "openai"},
		{"o4-mini", "openai"},
		{"o1", "openai"},
		{"chatgpt-4o-latest", "openai"},
		// Unknown model falls back to openai
		{"llama-3-70b", "openai"},
		{"mistral-large", "openai"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := InferFormat(tt.model)
			if got != tt.want {
				t.Errorf("InferFormat(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

func TestEndpointConfig_SupportsFormat(t *testing.T) {
	// Single-format endpoint
	single := EndpointConfig{Format: "anthropic"}
	if !single.SupportsFormat("anthropic") {
		t.Error("single-format endpoint should support its format")
	}
	if single.SupportsFormat("openai") {
		t.Error("single-format endpoint should not support other formats")
	}

	// Multi-format endpoint (like openrouter)
	multi := EndpointConfig{
		AnthropicURL: "https://openrouter.ai/api/v1",
		OpenAIURL:    "https://openrouter.ai/api/v1",
	}
	if !multi.SupportsFormat("anthropic") {
		t.Error("multi-format endpoint should support anthropic")
	}
	if !multi.SupportsFormat("openai") {
		t.Error("multi-format endpoint should support openai")
	}
	if multi.SupportsFormat("gemini") {
		t.Error("multi-format endpoint without gemini_url should not support gemini")
	}
}

func TestEndpointConfig_URLForFormat(t *testing.T) {
	ep := EndpointConfig{
		URL:          "https://default.example.com",
		AnthropicURL: "https://anthropic.example.com",
	}

	if got := ep.URLForFormat("anthropic"); got != "https://anthropic.example.com" {
		t.Errorf("URLForFormat(anthropic) = %q, want anthropic URL", got)
	}
	if got := ep.URLForFormat("openai"); got != "https://default.example.com" {
		t.Errorf("URLForFormat(openai) = %q, want fallback URL", got)
	}

	// No format-specific URL and no default
	empty := EndpointConfig{}
	if got := empty.URLForFormat("anthropic"); got != "" {
		t.Errorf("URLForFormat on empty = %q, want empty", got)
	}
}

func TestEndpointDefaults(t *testing.T) {
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

	// Built-in endpoints must be populated
	for _, name := range []string{"anthropic", "gemini", "openai", "openrouter"} {
		if _, ok := cfg.Endpoints[name]; !ok {
			t.Errorf("missing default endpoint %q", name)
		}
	}

	// Check format fields
	if cfg.Endpoints["anthropic"].Format != "anthropic" {
		t.Errorf("anthropic endpoint format = %q", cfg.Endpoints["anthropic"].Format)
	}
	if cfg.Endpoints["gemini"].Format != "gemini" {
		t.Errorf("gemini endpoint format = %q", cfg.Endpoints["gemini"].Format)
	}
	if cfg.Endpoints["openai"].Format != "openai" {
		t.Errorf("openai endpoint format = %q", cfg.Endpoints["openai"].Format)
	}

	// OpenRouter should have multi-format URLs
	or := cfg.Endpoints["openrouter"]
	if or.AnthropicURL == "" {
		t.Error("openrouter missing anthropic_url")
	}
	if or.OpenAIURL == "" {
		t.Error("openrouter missing openai_url")
	}
}

func TestEndpointUserOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"
[anthropic]
token = "test-token"

[endpoints.local]
format = "openai"
url = "http://localhost:8080/v1"
api_key = "local.api_key"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	local, ok := cfg.Endpoints["local"]
	if !ok {
		t.Fatal("missing user-defined endpoint 'local'")
	}
	if local.Format != "openai" {
		t.Errorf("local format = %q, want openai", local.Format)
	}
	if local.URL != "http://localhost:8080/v1" {
		t.Errorf("local url = %q", local.URL)
	}
	if local.APIKey != "local.api_key" {
		t.Errorf("local api_key = %q", local.APIKey)
	}

	// Built-in defaults should still exist
	if _, ok := cfg.Endpoints["anthropic"]; !ok {
		t.Error("built-in anthropic endpoint missing after user override")
	}
}

func TestModelMigrationAddsEndpointPrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	// Bare model name (no endpoint prefix) should be auto-migrated
	toml := `
[agent]
id = "test"
model = "anthropic/claude-opus-4-6"
[anthropic]
token = "test-token"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agent.Model != "anthropic/claude-opus-4-6" {
		t.Errorf("Agent.Model = %q, want %q (should be migrated)", cfg.Agent.Model, "anthropic/claude-opus-4-6")
	}
}

func TestModelValidationRejectsColonFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"
model = "anthropic:claude-haiku-4-5"
[anthropic]
token = "test-token"
`
	os.WriteFile(path, []byte(toml), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for colon format, got nil")
	}
	if !strings.Contains(err.Error(), "developer/model_id") {
		t.Errorf("error = %q, want to contain 'developer/model_id'", err.Error())
	}
	// Should suggest the corrected format
	if !strings.Contains(err.Error(), "anthropic/claude-haiku-4-5") {
		t.Errorf("error = %q, want to contain suggested format 'anthropic/claude-haiku-4-5'", err.Error())
	}
}

func TestModelValidationRejectsInvalidFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[agent]
id = "test"
[anthropic]
token = "test-token"

[endpoints.bad]
format = "grpc"
`
	os.WriteFile(path, []byte(toml), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid format, got nil")
	}
	if !strings.Contains(err.Error(), "format") {
		t.Errorf("error = %q, want to contain 'format'", err.Error())
	}
}

func TestAliasDefaultsIncludeEndpointPrefix(t *testing.T) {
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

	// Default aliases should include endpoint prefixes
	tests := []struct {
		alias string
		want  string
	}{
		{"opus", "anthropic/claude-opus-4-6"},
		{"sonnet", "anthropic/claude-sonnet-4-6"},
		{"haiku", "anthropic/claude-haiku-4-5-20251001"},
		{"flash", "google/gemini-2.5-flash"},
		{"pro", "google/gemini-2.5-pro"},
	}
	for _, tt := range tests {
		got := cfg.Models.Aliases[tt.alias]
		if got != tt.want {
			t.Errorf("alias %q = %q, want %q", tt.alias, got, tt.want)
		}
	}
}

// TestHasBackend tests MemoryConfig.HasBackend method
func TestHasBackend(t *testing.T) {
	tests := []struct {
		name     string
		backends []string
		search   string
		want     bool
	}{
		{"found", []string{"milvus", "sqlite"}, "milvus", true},
		{"not found", []string{"milvus", "sqlite"}, "pgvector", false},
		{"empty list", []string{}, "milvus", false},
		{"case sensitive", []string{"Milvus"}, "milvus", false},
		{"multiple matches", []string{"a", "b", "c"}, "b", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := MemoryConfig{SearchBackends: tt.backends}
			got := cfg.HasBackend(tt.search)
			if got != tt.want {
				t.Errorf("HasBackend(%q) = %v, want %v", tt.search, got, tt.want)
			}
		})
	}
}

// TestValidateMemoryThreshold tests ValidateMemoryThreshold function
func TestValidateMemoryThreshold(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		errMsg  string
	}{
		// Valid percentage
		{"valid percent 50", "50%", false, ""},
		{"valid percent 1", "1%", false, ""},
		{"valid percent 100", "100%", false, ""},
		{"valid percent decimal", "50.5%", false, ""},
		{"valid percent with spaces", "  50%  ", false, ""},
		// Valid MB
		{"valid mb", "512mb", false, ""},
		{"valid mb decimal", "512.5mb", false, ""},
		{"valid mb uppercase", "512MB", false, ""},
		// Valid GB
		{"valid gb", "2gb", false, ""},
		{"valid gb decimal", "2.5gb", false, ""},
		{"valid gb uppercase", "2GB", false, ""},
		// Invalid
		{"empty string", "", true, "empty"},
		{"invalid percent 0", "0%", true, "between 0 and 100"},
		{"invalid percent 101", "101%", true, "between 0 and 100"},
		{"invalid percent negative", "-50%", true, "between 0 and 100"},
		{"invalid percent not number", "abc%", true, "invalid percentage"},
		{"invalid mb 0", "0mb", true, "must be positive"},
		{"invalid mb negative", "-512mb", true, "must be positive"},
		{"invalid mb not number", "abcmb", true, "invalid megabytes"},
		{"invalid gb 0", "0gb", true, "must be positive"},
		{"invalid gb negative", "-2gb", true, "must be positive"},
		{"invalid gb not number", "abcgb", true, "invalid gigabytes"},
		{"invalid format kb", "512kb", true, "unknown format"},
		{"invalid format plain number", "512", true, "unknown format"},
		{"invalid format no unit", "512", true, "unknown format"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMemoryThreshold(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateMemoryThreshold(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if err != nil && tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("ValidateMemoryThreshold(%q) error = %q, want to contain %q", tt.input, err.Error(), tt.errMsg)
			}
		})
	}
}


// TestURLForFormat tests EndpointConfig.URLForFormat method
func TestURLForFormat(t *testing.T) {
	tests := []struct {
		name     string
		endpoint EndpointConfig
		format   string
		want     string
	}{
		{
			name: "anthropic url set",
			endpoint: EndpointConfig{
				URL:           "https://default.com",
				AnthropicURL:  "https://anthropic.com",
				OpenAIURL:     "https://openai.com",
			},
			format: "anthropic",
			want:   "https://anthropic.com",
		},
		{
			name: "anthropic no specific url fallback",
			endpoint: EndpointConfig{
				URL: "https://default.com",
			},
			format: "anthropic",
			want:   "https://default.com",
		},
		{
			name: "openai url set",
			endpoint: EndpointConfig{
				URL:       "https://default.com",
				OpenAIURL: "https://openai.com",
			},
			format: "openai",
			want:   "https://openai.com",
		},
		{
			name: "gemini url set",
			endpoint: EndpointConfig{
				URL:       "https://default.com",
				GeminiURL: "https://gemini.com",
			},
			format: "gemini",
			want:   "https://gemini.com",
		},
		{
			name: "unknown format returns default",
			endpoint: EndpointConfig{
				URL: "https://default.com",
			},
			format: "unknown",
			want:   "https://default.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.endpoint.URLForFormat(tt.format)
			if got != tt.want {
				t.Errorf("URLForFormat(%q) = %q, want %q", tt.format, got, tt.want)
			}
		})
	}
}

// TestSupportsFormat tests EndpointConfig.SupportsFormat method
func TestSupportsFormat(t *testing.T) {
	tests := []struct {
		name     string
		endpoint EndpointConfig
		format   string
		want     bool
	}{
		{
			name: "anthropic via explicit url",
			endpoint: EndpointConfig{
				AnthropicURL: "https://anthropic.com",
			},
			format: "anthropic",
			want:   true,
		},
		{
			name: "anthropic via format field",
			endpoint: EndpointConfig{
				Format: "anthropic",
			},
			format: "anthropic",
			want:   true,
		},
		{
			name: "openai via explicit url",
			endpoint: EndpointConfig{
				OpenAIURL: "https://openai.com",
			},
			format: "openai",
			want:   true,
		},
		{
			name: "openai via format field",
			endpoint: EndpointConfig{
				Format: "openai",
			},
			format: "openai",
			want:   true,
		},
		{
			name: "gemini via explicit url",
			endpoint: EndpointConfig{
				GeminiURL: "https://gemini.com",
			},
			format: "gemini",
			want:   true,
		},
		{
			name: "gemini via format field",
			endpoint: EndpointConfig{
				Format: "gemini",
			},
			format: "gemini",
			want:   true,
		},
		{
			name: "format not supported",
			endpoint: EndpointConfig{
				Format: "anthropic",
			},
			format: "openai",
			want:   false,
		},
		{
			name: "unknown format",
			endpoint: EndpointConfig{
				Format: "anthropic",
			},
			format: "unknown",
			want:   false,
		},
		{
			name: "empty endpoint",
			endpoint: EndpointConfig{},
			format:   "anthropic",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.endpoint.SupportsFormat(tt.format)
			if got != tt.want {
				t.Errorf("SupportsFormat(%q) = %v, want %v", tt.format, got, tt.want)
			}
		})
	}
}

// TestParseByteSize tests ParseByteSize function
func TestParseByteSize(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{"plain number", "100", 100, false},
		{"kilobytes", "1KB", 1024, false},
		{"kilobytes lowercase", "1kb", 1024, false},
		{"megabytes", "1MB", 1024 * 1024, false},
		{"megabytes lowercase", "1mb", 1024 * 1024, false},
		{"gigabytes", "1GB", 1024 * 1024 * 1024, false},
		{"gigabytes lowercase", "1gb", 1024 * 1024 * 1024, false},
		{"with spaces", "  100  ", 100, false},
		{"64MB example", "64MB", 64 * 1024 * 1024, false},
		{"empty string", "", 0, true},
		{"invalid format", "abc", 0, true},
		{"zero bytes", "0", 0, true},
		{"negative bytes", "-10", 0, true},
		{"decimal kb not supported", "1.5KB", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseByteSize(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseByteSize(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if err == nil && got != tt.want {
				t.Errorf("ParseByteSize(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

