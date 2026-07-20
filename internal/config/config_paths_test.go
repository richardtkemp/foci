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
	// Proves that an explicit prompt-file path in [sessions] is preserved
	// exactly as configured after loading.
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	toml := `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"

[sessions]
branch_orientation_facet_prompt = "/home/foci/shared/prompts/branch-orientation-facet.md"
`
	os.WriteFile(path, []byte(toml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if DerefStr(cfg.Sessions.BranchOrientationFacetPrompt) != "/home/foci/shared/prompts/branch-orientation-facet.md" {
		t.Errorf("BranchOrientationFacetPrompt = %v", cfg.Sessions.BranchOrientationFacetPrompt)
	}
}

func TestPromptFilePathsDefaultEmpty(t *testing.T) {
	// Proves that a prompt-file path field defaults to empty string when not
	// configured, rather than some non-empty fallback.
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
	if DerefStr(cfg.Sessions.BranchOrientationFacetPrompt) != "" {
		t.Errorf("BranchOrientationFacetPrompt should default to empty, got %v", cfg.Sessions.BranchOrientationFacetPrompt)
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

	// Log files should resolve to $HOME/logs/...
	wantEventFile := filepath.Join(home, "logs/foci.log")
	if cfg.Logging.EventFile != wantEventFile {
		t.Errorf("EventFile = %q, want %q", cfg.Logging.EventFile, wantEventFile)
	}
	wantAPIFile := filepath.Join(home, "logs/api.jsonl")
	if cfg.Logging.APIFile != wantAPIFile {
		t.Errorf("APIFile = %q, want %q", cfg.Logging.APIFile, wantAPIFile)
	}

	// Conversation log should default to true
	if !DerefBool(cfg.Logging.ConversationLog) {
		t.Error("ConversationLog should default to true")
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

[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "test"

[logging]
event_file = "/var/log/foci.log"
api_file = "/var/log/api.jsonl"
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
	if cfg.Sessions.Dir != "/var/sessions" {
		t.Errorf("Sessions.Dir = %q, want /var/sessions", cfg.Sessions.Dir)
	}
	if cfg.WelcomeFile != "/opt/welcome.md" {
		t.Errorf("WelcomeFile = %q, want /opt/welcome.md", cfg.WelcomeFile)
	}
}

func TestDetectAvatar(t *testing.T) {
	// Precedence: $workspace/avatar.{ext} (in avatarExts order) beats
	// $workspace/.data/avatar.{ext}; "" when nothing matches.
	t.Run("none", func(t *testing.T) {
		if got := detectAvatar(t.TempDir()); got != "" {
			t.Errorf("detectAvatar(empty ws) = %q, want \"\"", got)
		}
		if got := detectAvatar(""); got != "" {
			t.Errorf("detectAvatar(\"\") = %q, want \"\"", got)
		}
	})

	t.Run("workspace png", func(t *testing.T) {
		ws := t.TempDir()
		want := filepath.Join(ws, "avatar.png")
		if err := os.WriteFile(want, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := detectAvatar(ws); got != want {
			t.Errorf("detectAvatar = %q, want %q", got, want)
		}
	})

	t.Run("ext precedence png over webp", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "avatar.webp"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		png := filepath.Join(ws, "avatar.png")
		if err := os.WriteFile(png, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := detectAvatar(ws); got != png {
			t.Errorf("detectAvatar = %q, want png (precedence) %q", got, png)
		}
	})

	t.Run("dir precedence workspace over .data", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, ".data"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, ".data", "avatar.png"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		// A lower-precedence ext in the workspace dir still beats .data.
		topJpg := filepath.Join(ws, "avatar.jpg")
		if err := os.WriteFile(topJpg, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := detectAvatar(ws); got != topJpg {
			t.Errorf("detectAvatar = %q, want workspace jpg %q (dir precedence)", got, topJpg)
		}
	})

	t.Run("dotdata fallback", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, ".data"), 0o755); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(ws, ".data", "avatar.jpeg")
		if err := os.WriteFile(want, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := detectAvatar(ws); got != want {
			t.Errorf("detectAvatar = %q, want .data fallback %q", got, want)
		}
	})
}
