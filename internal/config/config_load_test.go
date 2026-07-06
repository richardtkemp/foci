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
	// Proves that a config file with multiple sections (agent, telegram, sessions,
	// http, logging) is loaded correctly with all values parsed into their structs.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "main"
workspace = "/tmp/workspace"


[[platforms]]
id = "telegram"
[platforms.access]
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

	if cfg.Agents[0].ID != "main" {
		t.Errorf("Agents[0].ID = %q, want %q", cfg.Agents[0].ID, "main")
	}
	if cfg.Agents[0].Workspace != "/tmp/workspace" {
		t.Errorf("Agents[0].Workspace = %q", cfg.Agents[0].Workspace)
	}
	tgPlat := cfg.Platform("telegram")
	if tgPlat == nil || len(tgPlat.Access.AllowedUsers) != 2 || tgPlat.Access.AllowedUsers[0] != "111" {
		t.Errorf("Platform(telegram).AllowedUsers = %v", tgPlat)
	}
	if cfg.Sessions.Dir != "/tmp/sessions" {
		t.Errorf("Sessions.Dir = %q", cfg.Sessions.Dir)
	}
	if DerefFloat(cfg.Sessions.CompactionThreshold) != 0.7 {
		t.Errorf("Sessions.CompactionThreshold = %v, want 0.7", cfg.Sessions.CompactionThreshold)
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
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Sessions.CompactionThreshold != nil {
		t.Errorf("default CompactionThreshold should be nil (unset = non-linear curve), got %v", *cfg.Sessions.CompactionThreshold)
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
}

func TestLoadCustomCommands(t *testing.T) {
	// Proves that [[commands]] blocks are loaded into the Commands slice with all
	// fields (name, description, script, timeout) correctly parsed.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

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
	// Proves that a single [[agents]] entry is loaded into cfg.Agents.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "main"
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
	if cfg.Agents[0].Workspace != "/tmp/workspace" {
		t.Errorf("Agents[0].Workspace = %q, want %q", cfg.Agents[0].Workspace, "/tmp/workspace")
	}

}

func TestLoadMultiAgent(t *testing.T) {
	// Proves that multiple [[agents]] entries are loaded into the Agents slice with
	// correct per-agent values, and that defaults are applied to agents missing fields.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "clutch"
workspace = "/tmp/foci/workspace1"

[[agents.platforms]]
id = "telegram"
bot = "primary"
facet_bots = ["secondary"]

[[agents]]
id = "scout"
workspace = "/tmp/foci/workspace2"

[[agents.platforms]]
id = "telegram"
bot = "scout"

[[platforms]]
id = "telegram"
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
	tg0 := cfg.Agents[0].Platform("telegram")
	if tg0 == nil || tg0.Bot != "primary" {
		t.Errorf("Agents[0] telegram bot = %v", tg0)
	}
	if tg0 == nil || len(tg0.FacetBots) != 1 || tg0.FacetBots[0] != "secondary" {
		t.Errorf("Agents[0].FacetBots = %v, want [secondary]", tg0.FacetBots)
	}

	// Second agent — defaults applied
	if cfg.Agents[1].ID != "scout" {
		t.Errorf("Agents[1].ID = %q", cfg.Agents[1].ID)
	}
	tg1 := cfg.Agents[1].Platform("telegram")
	if tg1 == nil || tg1.Bot != "scout" {
		t.Errorf("Agents[1] telegram bot = %v", tg1)
	}
	if tg1 != nil && len(tg1.FacetBots) != 0 {
		t.Errorf("Agents[1].FacetBots = %v, want empty", tg1.FacetBots)
	}

}

func TestUnknownKeysDetected(t *testing.T) {
	// Proves that UnknownKeys returns both unrecognized fields within known sections
	// and entire unknown sections, as dotted paths sorted for deterministic comparison.
	tomlData := `
[[agents]]
id = "main"
bogus_field = "oops"

[agents.backend_config.env]
FOO = "bar"

[unknown_section]
foo = "bar"
some_key = "value"
`
	var cfg Config
	md, err := tomlParser.Decode(tomlData, &cfg)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	keys := UnknownKeys(md, nil)
	if len(keys) == 0 {
		t.Fatal("expected unknown keys, got none")
	}

	// backend_config.* descendants are free-form and must NOT be flagged.
	for _, k := range keys {
		if strings.Contains(k, "backend_config") {
			t.Errorf("backend_config descendant %q should be skipped, not flagged", k)
		}
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

func TestLoadPopulatesUndefinedKeys(t *testing.T) {
	// Proves that Load succeeds and populates UndefinedKeys when unknown TOML
	// keys are present, so the caller can log them after logging is ready.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

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
	if len(cfg.UndefinedKeys) == 0 {
		t.Error("expected UndefinedKeys to be populated for unknown TOML keys")
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
	// Proves that agent-level platform fields are loaded from the [[agents.platforms]]
	// array and that show_tool_calls is set at the agent level.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "testbot"
show_tool_calls = "preview"

[[agents.platforms]]
id = "telegram"
bot = "my_bot"
bot_secret = "custom.secret"
facet_bots = ["extra1", "extra2"]
[agents.platforms.access]
allowed_users = ["123", "456"]

[[platforms]]
id = "telegram"
[platforms.access]
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

	tg := agent.Platform("telegram")
	if tg == nil {
		t.Fatal("Platform(telegram) is nil")
	}
	if tg.Bot != "my_bot" {
		t.Errorf("Platform(telegram).Bot = %q, want %q", tg.Bot, "my_bot")
	}
	if tg.BotSecret != "custom.secret" {
		t.Errorf("Platform(telegram).BotSecret = %q, want %q", tg.BotSecret, "custom.secret")
	}
	if len(tg.FacetBots) != 2 || tg.FacetBots[0] != "extra1" {
		t.Errorf("Platform(telegram).FacetBots = %v, want [extra1 extra2]", tg.FacetBots)
	}
	if len(tg.Access.AllowedUsers) != 2 || tg.Access.AllowedUsers[0] != "123" {
		t.Errorf("Platform(telegram).AllowedUsers = %v, want [123 456]", tg.Access.AllowedUsers)
	}
}

func TestLoadPlatformConfigNewStyle(t *testing.T) {
	// Proves that the [[agents.platforms]] config block is loaded correctly,
	// including stream_output as a nullable bool.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")

	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "newbot"

[[agents.platforms]]
id = "telegram"
bot = "new_bot"
bot_secret = "new.secret"
[agents.platforms.access]
allowed_users = ["999"]
[agents.platforms.display]
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

	tg := agent.Platform("telegram")
	if tg == nil {
		t.Fatal("Platform(telegram) is nil")
	}
	if tg.Bot != "new_bot" {
		t.Errorf("Platform(telegram).Bot = %q, want %q", tg.Bot, "new_bot")
	}
	if tg.BotSecret != "new.secret" {
		t.Errorf("Platform(telegram).BotSecret = %q, want %q", tg.BotSecret, "new.secret")
	}
	if len(tg.Access.AllowedUsers) != 1 || tg.Access.AllowedUsers[0] != "999" {
		t.Errorf("Platform(telegram).AllowedUsers = %v, want [999]", tg.Access.AllowedUsers)
	}
	if tg.Display.StreamOutput == nil || *tg.Display.StreamOutput != false {
		t.Errorf("Platform(telegram).StreamOutput = %v, want false", tg.Display.StreamOutput)
	}
}

// TestExtractGroupNames_FastCheapDefaultToPowerful pins the load-time
// defaulting in extractGroupNames: when [groups] powerful is set but fast
// and/or cheap are omitted, the missing ones inherit powerful's value. This
// is the only layer that performs the defaulting — the GroupResolver tests
// construct a GroupsConfig map directly and bypass it, and the behaviour is
// API-backend-only (delegated backends take their model from backend_config),
// so a unit test here is the correct and complete coverage for the logic.
func TestExtractGroupNames_FastCheapDefaultToPowerful(t *testing.T) {
	tests := []struct {
		name string
		toml string
		want map[string]string
	}{
		{
			name: "fast and cheap omitted default to powerful",
			toml: "[groups]\npowerful = \"anthropic/opus\"\n",
			want: map[string]string{"powerful": "anthropic/opus", "fast": "anthropic/opus", "cheap": "anthropic/opus"},
		},
		{
			name: "all three explicit are left untouched",
			toml: "[groups]\npowerful = \"anthropic/opus\"\nfast = \"anthropic/sonnet\"\ncheap = \"anthropic/haiku\"\n",
			want: map[string]string{"powerful": "anthropic/opus", "fast": "anthropic/sonnet", "cheap": "anthropic/haiku"},
		},
		{
			name: "only cheap inherits when fast is explicit",
			toml: "[groups]\npowerful = \"anthropic/opus\"\nfast = \"anthropic/sonnet\"\n",
			want: map[string]string{"powerful": "anthropic/opus", "fast": "anthropic/sonnet", "cheap": "anthropic/opus"},
		},
		{
			name: "no powerful means no defaulting",
			toml: "[groups]\nfast = \"anthropic/sonnet\"\n",
			want: map[string]string{"fast": "anthropic/sonnet"},
		},
		{
			name: "no groups section yields empty map",
			toml: "name = \"x\"\n",
			want: map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractGroupNames([]byte(tt.toml))
			if len(got) != len(tt.want) {
				t.Fatalf("extractGroupNames(%q) = %v, want %v", tt.toml, got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("group %q = %q, want %q (full: %v)", k, got[k], v, got)
				}
			}
		})
	}
}
