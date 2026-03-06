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
