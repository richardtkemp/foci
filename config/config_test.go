package config

import (
	"os"
	"path/filepath"
	"testing"
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
	if cfg.HTTP.Port != 18790 {
		t.Errorf("default HTTP.Port = %d, want 18790", cfg.HTTP.Port)
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
