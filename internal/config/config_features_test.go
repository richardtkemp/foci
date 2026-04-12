package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tomlParser "github.com/BurntSushi/toml"
)

func TestLoadTelegramToggleDefaults(t *testing.T) {
	// Proves that enable_stop_aliases and startup_notify are nil when not set
	// (code defaults apply at use time, not load time).
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
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
	if cfg.Behavior.EnableStopAliases == nil || !*cfg.Behavior.EnableStopAliases {
		t.Errorf("EnableStopAliases should be true (via tag default), got %v", cfg.Behavior.EnableStopAliases)
	}
}

func TestLoadTelegramTogglesExplicitFalse(t *testing.T) {
	// Proves that explicitly setting enable_stop_aliases (in defaults) and
	// startup_notify (in platforms) to false correctly disables both toggles.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"

[behavior]
enable_stop_aliases = false

[[platforms]]
id = "telegram"
[platforms.notify]
startup_notify = false
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if DerefBool(cfg.Behavior.EnableStopAliases) {
		t.Error("EnableStopAliases should be false when explicitly set")
	}
	tgPlat := cfg.Platform("telegram")
	if tgPlat == nil {
		t.Fatal("Platform(telegram) = nil")
	}
	if tgPlat.Notify.StartupNotify == nil || *tgPlat.Notify.StartupNotify {
		t.Error("StartupNotify should be false when explicitly set")
	}
}

func TestAgentStartupNotification(t *testing.T) {
	// Proves that per-agent startup_notify is nil when unset (falls back to global),
	// and correctly stores true or false as a non-nil pointer when explicitly configured.
	t.Run("defaults to nil", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
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
		if cfg.Agents[0].Notify.StartupNotify != nil {
			t.Error("StartupNotify should default to nil (use global)")
		}
	})

	t.Run("explicit true", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"
[agents.notify]
startup_notify = true
`
		os.WriteFile(path, []byte(toml), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Agents[0].Notify.StartupNotify == nil || !*cfg.Agents[0].Notify.StartupNotify {
			t.Error("StartupNotify should be true")
		}
	})

	t.Run("explicit false", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"
[agents.notify]
startup_notify = false
`
		os.WriteFile(path, []byte(toml), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Agents[0].Notify.StartupNotify == nil || *cfg.Agents[0].Notify.StartupNotify {
			t.Error("StartupNotify should be false")
		}
	})
}

// TestLoadThinkingConfig and TestLoadThinkingPerAgent were removed:
// Thinking/effort settings are now per-agent in AgentConfig fields,
// not in global [anthropic] or per-agent [agents.anthropic] sections.
// ApplyProviderDefaults was deleted as part of the Per-Model Config refactor.

func TestShowToolCallsDisplay(t *testing.T) {
	// Proves that ToolCallDisplay accepts canonical strings, aliases (false/medium/true),
	// bools, rejects invalid strings, and works both at the defaults level and per-agent
	// with nil meaning unset.
	tests := []struct {
		name    string
		toml    string
		want    ToolCallDisplay
		wantErr bool
	}{
		{"string off", `show_tool_calls = "off"`, ToolCallOff, false},
		{"string preview", `show_tool_calls = "preview"`, ToolCallPreview, false},
		{"string full", `show_tool_calls = "full"`, ToolCallFull, false},
		{"false alias", `show_tool_calls = "false"`, ToolCallOff, false},
		{"medium alias", `show_tool_calls = "medium"`, ToolCallPreview, false},
		{"true alias", `show_tool_calls = "true"`, ToolCallFull, false},
		{"bool true", `show_tool_calls = true`, ToolCallFull, false},
		{"bool false", `show_tool_calls = false`, ToolCallOff, false},
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
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "a"
[agents.display]
show_tool_calls = "full"

[[agents]]
id = "b"
`), 0644)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Agents[0].Display.ShowToolCalls == nil {
			t.Fatal("agent a: ShowToolCalls should be non-nil")
		}
		if *cfg.Agents[0].Display.ShowToolCalls != ToolCallFull {
			t.Errorf("agent a: ShowToolCalls = %q, want %q", *cfg.Agents[0].Display.ShowToolCalls, ToolCallFull)
		}
		// Agent b has no show_tool_calls set — should be nil (resolved at runtime via telegram config)
		if cfg.Agents[1].Display.ShowToolCalls != nil {
			t.Errorf("agent b: ShowToolCalls = %q, want nil (not set)", *cfg.Agents[1].Display.ShowToolCalls)
		}
	})

	// Platform section
	t.Run("platform string", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[platforms]]
id = "telegram"
[platforms.display]
show_tool_calls = "full"
`), 0644)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		tgPlat := cfg.Platform("telegram")
		if tgPlat == nil || tgPlat.Display.ShowToolCalls == nil || *tgPlat.Display.ShowToolCalls != ToolCallFull {
			t.Errorf("Platform(telegram).ShowToolCalls want %q", ToolCallFull)
		}
	})

	// Global default (not set) — ShowToolCalls is now provider-driven, nil in Load
	t.Run("platform default nil", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"
`), 0644)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		// Without ApplyProviderDefaults, there are no platform entries
		if tg := cfg.Platform("telegram"); tg != nil && tg.Display.ShowToolCalls != nil {
			t.Errorf("Platform(telegram).ShowToolCalls should be nil without provider defaults, got %v", *tg.Display.ShowToolCalls)
		}
	})

}

func TestNormalizeBoolStrings(t *testing.T) {
	// Proves that normalizeBoolStrings converts "on"/"true" to bare true and
	// "off"/"false" to bare false only for known bool-typed config keys, leaving
	// non-bool keys (like thinking = "off") and other string values unchanged.
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
	// Proves that bool-typed config fields accept string values "on"/"off"/"true"/"false"
	// and are correctly decoded to their boolean equivalents during Load.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	os.WriteFile(path, []byte(`
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[behavior]
enable_stop_aliases = "on"

[[platforms]]
id = "telegram"
[platforms.notify]
startup_notify = "off"

[environment]
enabled = "true"

[logging]
log_rotation = "false"
`), 0644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !DerefBool(cfg.Behavior.EnableStopAliases) {
		t.Error("EnableStopAliases should be true (from \"on\")")
	}
	tgPlat := cfg.Platform("telegram")
	if tgPlat == nil || tgPlat.Notify.StartupNotify == nil || *tgPlat.Notify.StartupNotify {
		t.Error("StartupNotify should be false (from \"off\")")
	}
	if !DerefBool(cfg.Environment.Enabled) {
		t.Error("Environment.Enabled should be true (from \"true\")")
	}
	if DerefBool(cfg.Logging.LogRotation) {
		t.Error("Logging.LogRotation should be false (from \"false\")")
	}
}

func TestLoadFacetBotsPlural(t *testing.T) {
	// Proves that a per-agent facet_bots list is correctly loaded with all
	// configured bot names in order.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "clutch"

[[agents.platforms]]
id = "telegram"
bot = "primary"
facet_bots = ["mb1", "mb2"]

[[platforms]]
id = "telegram"
allowed_users = ["111"]
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tg := cfg.Agents[0].Platform("telegram")
	if tg == nil {
		t.Fatal("Platform(telegram) = nil")
	}
	if len(tg.FacetBots) != 2 {
		t.Fatalf("FacetBots len = %d, want 2", len(tg.FacetBots))
	}
	if tg.FacetBots[0] != "mb1" || tg.FacetBots[1] != "mb2" {
		t.Errorf("FacetBots = %v, want [mb1 mb2]", tg.FacetBots)
	}
}

func TestLoadSharedFacetBots(t *testing.T) {
	// Proves that the global [[platforms]] facet_bots list is correctly loaded
	// into the PlatformConfig and made available for shared cross-agent routing.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "clutch"

[[agents.platforms]]
id = "telegram"
bot = "primary"

[[platforms]]
id = "telegram"
allowed_users = ["111"]
facet_bots = ["spare1", "spare2"]
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tgPlat := cfg.Platform("telegram")
	if tgPlat == nil {
		t.Fatal("Platform(telegram) = nil")
	}
	if len(tgPlat.FacetBots) != 2 {
		t.Fatalf("Platform(telegram).FacetBots len = %d, want 2", len(tgPlat.FacetBots))
	}
	if tgPlat.FacetBots[0] != "spare1" || tgPlat.FacetBots[1] != "spare2" {
		t.Errorf("Platform(telegram).FacetBots = %v, want [spare1 spare2]", tgPlat.FacetBots)
	}
}

func TestCompactionPreserveMessagesConfig(t *testing.T) {
	// Proves that compaction_preserve_messages defaults to 25, can be overridden
	// globally or to zero, supports per-agent override (nil when unset), and
	// rejects negative values with an error.
	// global default subtest removed — CompactionPreserveMessages is nil when unset,
	// code default (25) applied at use time.

	t.Run("global explicit", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"
[sessions]
compaction_preserve_messages = 10
`), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if DerefInt(cfg.Sessions.CompactionPreserveMessages) != 10 {
			t.Errorf("CompactionPreserveMessages = %d, want 10", cfg.Sessions.CompactionPreserveMessages)
		}
	})

	t.Run("global explicit zero", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"
[sessions]
compaction_preserve_messages = 0
`), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if DerefInt(cfg.Sessions.CompactionPreserveMessages) != 0 {
			t.Errorf("CompactionPreserveMessages = %d, want 0", cfg.Sessions.CompactionPreserveMessages)
		}
	})

	t.Run("per-agent override", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[sessions]
compaction_preserve_messages = 10

[[agents]]
id = "a"
[agents.sessions]
compaction_preserve_messages = 5

[[agents]]
id = "b"
`), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if DerefInt(cfg.Sessions.CompactionPreserveMessages) != 10 {
			t.Errorf("global = %d, want 10", cfg.Sessions.CompactionPreserveMessages)
		}
		if cfg.Agents[0].Sessions.CompactionPreserveMessages == nil || *cfg.Agents[0].Sessions.CompactionPreserveMessages != 5 {
			t.Errorf("agent a = %v, want 5", cfg.Agents[0].Sessions.CompactionPreserveMessages)
		}
		if cfg.Agents[1].Sessions.CompactionPreserveMessages != nil {
			t.Errorf("agent b should be nil, got %v", cfg.Agents[1].Sessions.CompactionPreserveMessages)
		}
	})

	t.Run("negative rejected", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
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
	// Proves that messages_in_log defaults to false, can be enabled globally, and
	// supports per-agent override via a nullable bool (nil = unset, inherits global).
	t.Run("default false", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"
`), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if DerefBool(cfg.Debug.MessagesInLog) {
			t.Error("MessagesInLog should default to false")
		}
	})

	t.Run("global explicit true", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"
[debug]
messages_in_log = true
`), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !DerefBool(cfg.Debug.MessagesInLog) {
			t.Error("MessagesInLog should be true")
		}
	})

	t.Run("per-agent override", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[debug]
messages_in_log = false

[[agents]]
id = "a"
[agents.debug]
messages_in_log = true

[[agents]]
id = "b"
`), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if DerefBool(cfg.Debug.MessagesInLog) {
			t.Error("global should be false")
		}
		if cfg.Agents[0].Debug.MessagesInLog == nil || !*cfg.Agents[0].Debug.MessagesInLog {
			t.Error("agent a should override to true")
		}
		if cfg.Agents[1].Debug.MessagesInLog != nil {
			t.Errorf("agent b should be nil, got %v", cfg.Agents[1].Debug.MessagesInLog)
		}
	})
}

func TestDebugSection(t *testing.T) {
	// Proves that [debug] fields are loaded directly, that legacy [sessions] debug
	// keys migrate to [debug] for backward compatibility, and that [debug] takes
	// precedence when both sections are present.
	t.Run("direct", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "foci.toml"), []byte(`
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "a"

[debug]
log_api_key_suffix = true

[notify]
compaction_debug = true
`), 0644)
		cfg, err := Load(filepath.Join(dir, "foci.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !DerefBool(cfg.Debug.LogAPIKeySuffix) {
			t.Error("expected log_api_key_suffix = true")
		}
		if !cfg.Notify.CompactionDebugEnabled() {
			t.Error("expected compaction_debug = true (via defaults NotifyConfig)")
		}
	})

	// Test defaults: both fields should be false when unset.
	t.Run("defaults", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "foci.toml"), []byte(`
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "a"
`), 0644)
		cfg, err := Load(filepath.Join(dir, "foci.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if DerefBool(cfg.Debug.LogAPIKeySuffix) {
			t.Error("expected log_api_key_suffix default false")
		}
		if cfg.Notify.CompactionDebugEnabled() {
			t.Error("expected compaction_debug default false")
		}
	})
}

func TestFacetNoCompactConfig(t *testing.T) {
	// Proves that facet_no_compact is nil when unset (semantically true), and
	// stores an explicit non-nil pointer when set to true or false in the config.
	t.Run("defaults to nil", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"
`), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Agents[0].Sessions.FacetNoCompact != nil {
			t.Error("FacetNoCompact should default to nil (treated as true)")
		}
	})

	t.Run("explicit true", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"
[agents.sessions]
facet_no_compact = true
`), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Agents[0].Sessions.FacetNoCompact == nil || !*cfg.Agents[0].Sessions.FacetNoCompact {
			t.Error("FacetNoCompact should be true")
		}
	})

	t.Run("explicit false", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "foci.toml")
		os.WriteFile(path, []byte(`
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"
[agents.sessions]
facet_no_compact = false
`), 0644)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Agents[0].Sessions.FacetNoCompact == nil || *cfg.Agents[0].Sessions.FacetNoCompact {
			t.Error("FacetNoCompact should be false")
		}
	})

	// "inherited from defaults" subtest removed — facet_no_compact moved to
	// CompactionConfig (SessionsConfig + AgentConfig). Defaults cascade via Merge at use time.
}
