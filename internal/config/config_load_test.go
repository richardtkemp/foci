package config

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	tomlParser "github.com/BurntSushi/toml"
)

func TestLoadFullConfig(t *testing.T) {
	// Proves that a config file with multiple sections (agent, telegram, sessions,
	// http, logging) is loaded correctly with all values parsed into their structs.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	toml := `
[[agents]]
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
	// Proves that a minimal config with only an agent ID produces correct default
	// values for model, compaction threshold, HTTP port/bind, logging level,
	// log file paths, and usage warning name.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	// Minimal config — only required fields
	toml := `
[[agents]]
id = "test"
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
	// Proves that a custom usage_warnings name and threshold list are loaded from
	// the [usage_warnings] section and override the default "mana" name.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[[agents]]
id = "test"

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
	// Proves that [[commands]] blocks are loaded into the Commands slice with all
	// fields (name, description, script, timeout) correctly parsed.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[[agents]]
id = "test"

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

func TestLoadSingleAgent(t *testing.T) {
	// Proves that a single [[agents]] entry is loaded and cfg.Agent mirrors it.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[[agents]]
id = "main"
model = "anthropic/claude-sonnet-4-6"
workspace = "/tmp/workspace"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

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
	// Proves that multiple [[agents]] entries are loaded into the Agents slice with
	// correct per-agent values, that defaults are applied to agents missing fields,
	// and that cfg.Agent mirrors the first agent.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[[agents]]
id = "clutch"
model = "anthropic/claude-sonnet-4-6"
workspace = "/tmp/foci/workspace1"

[agents.platforms.telegram]
bot = "primary"
multiball_bots = ["secondary"]

[[agents]]
id = "scout"
workspace = "/tmp/foci/workspace2"

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
	tg0 := cfg.Agents[0].GetTelegramPlatform()
	if tg0 == nil || tg0.Bot != "primary" {
		t.Errorf("Agents[0] telegram bot = %v", tg0)
	}
	if len(tg0.MultiballBots) != 1 || tg0.MultiballBots[0] != "secondary" {
		t.Errorf("Agents[0].MultiballBots = %v, want [secondary]", tg0.MultiballBots)
	}

	// Second agent — defaults applied
	if cfg.Agents[1].ID != "scout" {
		t.Errorf("Agents[1].ID = %q", cfg.Agents[1].ID)
	}
	if cfg.Agents[1].Model != "anthropic/claude-haiku-4-5-20251001" {
		t.Errorf("Agents[1].Model = %q, want default", cfg.Agents[1].Model)
	}
	tg1 := cfg.Agents[1].GetTelegramPlatform()
	if tg1 == nil || tg1.Bot != "scout" {
		t.Errorf("Agents[1] telegram bot = %v", tg1)
	}
	if tg1 != nil && len(tg1.MultiballBots) != 0 {
		t.Errorf("Agents[1].MultiballBots = %v, want empty", tg1.MultiballBots)
	}

	// cfg.Agent should mirror first agent
	if cfg.Agent.ID != "clutch" {
		t.Errorf("Agent.ID = %q, want %q", cfg.Agent.ID, "clutch")
	}
}

func TestLoadPerAgentUsageWarnings(t *testing.T) {
	// Proves that per-agent [agents.usage_warnings] overrides the global thresholds
	// for that agent, while agents without an override have an empty threshold list,
	// and the global configuration remains unaffected.
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

func TestUnknownKeysDetected(t *testing.T) {
	// Proves that UnknownKeys returns both unrecognized fields within known sections
	// and entire unknown sections, as dotted paths sorted for deterministic comparison.
	tomlData := `
[[agents]]
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
	expected := []string{"agents.bogus_field", "unknown_section", "unknown_section.foo", "unknown_section.some_key"}
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
	// Proves that Load succeeds and returns a valid config even when unknown keys
	// are present, rather than failing with an error (they are only warned about).
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	toml := `
[[agents]]
id = "main"

[unknown_section]
foo = "bar"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agents[0].ID != "main" {
		t.Errorf("Agents[0].ID = %q, want %q", cfg.Agents[0].ID, "main")
	}
}

func TestLoadMissingFile(t *testing.T) {
	// Proves that Load returns an error when the config file does not exist.
	_, err := Load("/nonexistent/path/foci.toml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadInvalidTOML(t *testing.T) {
	// Proves that Load returns an error when the file contains syntactically
	// invalid TOML rather than silently returning a zero-value config.
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")
	os.WriteFile(path, []byte("this is not valid toml [[["), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid TOML")
	}
}

func TestLoadPlatformConfigSync(t *testing.T) {
	// Proves that agent-level display fields (show_tool_calls) are synced to
	// Platforms.Telegram at load time, and that platform-specific fields are set
	// directly via [agents.platforms.telegram].
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	toml := `
[[agents]]
id = "testbot"
show_tool_calls = "preview"

[agents.platforms.telegram]
bot = "my_bot"
bot_secret = "custom.secret"
multiball_bots = ["extra1", "extra2"]
allowed_users = ["123", "456"]

[telegram]
allowed_users = ["789"]
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(cfg.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(cfg.Agents))
	}

	agent := cfg.Agents[0]

	// Platforms structure should be populated
	if agent.Platforms == nil {
		t.Fatal("Platforms is nil")
	}
	if agent.Platforms.Telegram == nil {
		t.Fatal("Platforms.Telegram is nil")
	}

	tg := agent.Platforms.Telegram
	if tg.Bot != "my_bot" {
		t.Errorf("Platforms.Telegram.Bot = %q, want %q", tg.Bot, "my_bot")
	}
	if tg.BotSecret != "custom.secret" {
		t.Errorf("Platforms.Telegram.BotSecret = %q, want %q", tg.BotSecret, "custom.secret")
	}
	if len(tg.MultiballBots) != 2 || tg.MultiballBots[0] != "extra1" {
		t.Errorf("Platforms.Telegram.MultiballBots = %v, want [extra1 extra2]", tg.MultiballBots)
	}
	if len(tg.AllowedUsers) != 2 || tg.AllowedUsers[0] != "123" {
		t.Errorf("Platforms.Telegram.AllowedUsers = %v, want [123 456]", tg.AllowedUsers)
	}
	if tg.ShowToolCalls == nil || *tg.ShowToolCalls != ToolCallPreview {
		t.Errorf("Platforms.Telegram.ShowToolCalls = %v, want preview", tg.ShowToolCalls)
	}
}

func TestLoadPlatformConfigNewStyle(t *testing.T) {
	// Proves that the new-style [agents.platforms.telegram] config block is loaded
	// directly into Platforms.Telegram without any migration, including stream_output
	// as a nullable bool.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	// New-style config with platforms section
	toml := `
[[agents]]
id = "newbot"

[agents.platforms.telegram]
bot = "new_bot"
bot_secret = "new.secret"
allowed_users = ["999"]
stream_output = false
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(cfg.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(cfg.Agents))
	}

	agent := cfg.Agents[0]

	if agent.Platforms == nil {
		t.Fatal("Platforms is nil")
	}
	if agent.Platforms.Telegram == nil {
		t.Fatal("Platforms.Telegram is nil")
	}

	tg := agent.Platforms.Telegram
	if tg.Bot != "new_bot" {
		t.Errorf("Platforms.Telegram.Bot = %q, want %q", tg.Bot, "new_bot")
	}
	if tg.BotSecret != "new.secret" {
		t.Errorf("Platforms.Telegram.BotSecret = %q, want %q", tg.BotSecret, "new.secret")
	}
	if len(tg.AllowedUsers) != 1 || tg.AllowedUsers[0] != "999" {
		t.Errorf("Platforms.Telegram.AllowedUsers = %v, want [999]", tg.AllowedUsers)
	}
	if tg.StreamOutput == nil || *tg.StreamOutput != false {
		t.Errorf("Platforms.Telegram.StreamOutput = %v, want false", tg.StreamOutput)
	}
}
