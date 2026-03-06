package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tomlParser "github.com/BurntSushi/toml"
)

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
