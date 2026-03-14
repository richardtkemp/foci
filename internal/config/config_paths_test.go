package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePath(t *testing.T) {
	// Proves that ResolvePath returns absolute paths unchanged and resolves relative
	// paths against the user's home directory.
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

func TestAgentDataPath(t *testing.T) {
	// Proves that AgentDataPath constructs the correct <workspace>/.data/<filename>
	// path for workspace-scoped per-agent data files.
	tests := []struct {
		workspace string
		filename  string
		want      string
	}{
		{"/home/foci/clutch", "reminders.db", "/home/foci/clutch/.data/reminders.db"},
		{"/opt/agents/otto", "conversation.db", "/opt/agents/otto/.data/conversation.db"},
		{"/ws", "search.bleve", "/ws/.data/search.bleve"},
	}
	for _, tt := range tests {
		got := AgentDataPath(tt.workspace, tt.filename)
		if got != tt.want {
			t.Errorf("AgentDataPath(%q, %q) = %q, want %q", tt.workspace, tt.filename, got, tt.want)
		}
	}
}

func TestDataPathAbsoluteDataDir(t *testing.T) {
	// Proves that DataPath correctly joins an absolute data_dir with the filename.
	cfg := &Config{DataDir: "/opt/foci/data"}
	got := cfg.DataPath("memory.db")
	want := "/opt/foci/data/memory.db"
	if got != want {
		t.Errorf("DataPath() = %q, want %q", got, want)
	}
}

func TestDataPathRelativeDataDir(t *testing.T) {
	// Proves that DataPath resolves a relative data_dir against the home directory
	// before joining with the filename.
	home, _ := os.UserHomeDir()
	cfg := &Config{DataDir: "mydata"}
	got := cfg.DataPath("state.json")
	want := filepath.Join(home, "mydata", "state.json")
	if got != want {
		t.Errorf("DataPath() = %q, want %q", got, want)
	}
}

func TestDataPathDefault(t *testing.T) {
	// Proves that DataPath falls back to ~/data when data_dir is empty.
	home, _ := os.UserHomeDir()
	cfg := &Config{DataDir: ""}
	got := cfg.DataPath("memory.db")
	want := filepath.Join(home, "data", "memory.db")
	if got != want {
		t.Errorf("DataPath() = %q, want %q", got, want)
	}
}

func TestDataPathLoadsFromConfig(t *testing.T) {
	// Proves that data_dir loaded from a TOML file is used by DataPath to construct
	// the correct absolute path to a named data file.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
data_dir = "/opt/foci/data"

[[agents]]
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
	// Proves that an explicit compaction_summary_prompt path in [sessions] is
	// preserved exactly as configured after loading.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[[agents]]
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
	// Proves that compaction_summary_prompt defaults to empty string when not
	// configured, rather than some non-empty fallback.
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
	if cfg.Sessions.CompactionSummaryPrompt != "" {
		t.Errorf("CompactionSummaryPrompt should default to empty, got %q", cfg.Sessions.CompactionSummaryPrompt)
	}
}

func TestResolveAllPaths(t *testing.T) {
	// Proves that all default path fields (log files, sessions dir, conversation db,
	// welcome file) resolve to the expected home-relative locations when not
	// explicitly configured.
	home, _ := os.UserHomeDir()

	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	// Minimal config with no path overrides — all defaults
	toml := `
[[agents]]
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
	// Proves that absolute path overrides in the config file are preserved exactly
	// and are not re-resolved against the home directory.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
welcome_file = "/opt/welcome.md"

[[agents]]
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
