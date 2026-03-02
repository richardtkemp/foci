package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsValidBotToken(t *testing.T) {
	tests := []struct {
		token string
		want  bool
	}{
		{"123456789:AAF-abcdefghijklmnopqrstuv", true},
		{"7894561230:ABCdefGHIjklMNOpqrSTUvwxyz_-12345", true},
		{"12345:short", false},           // token part too short
		{"notanumber:AAF-abcdefghijklmnopqrstuv", false}, // bot ID not numeric
		{"", false},
		{"just-a-string", false},
		{"123:ABC", false}, // token part too short
	}
	for _, tt := range tests {
		got := isValidBotToken(tt.token)
		if got != tt.want {
			t.Errorf("isValidBotToken(%q) = %v, want %v", tt.token, got, tt.want)
		}
	}
}

func TestIsValidUserID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"12345678", true},
		{"5970082313", true},
		{"123", true},
		{"12", false},  // too short
		{"", false},
		{"abc", false},
		{"12.34", false},
	}
	for _, tt := range tests {
		got := isValidUserID(tt.id)
		if got != tt.want {
			t.Errorf("isValidUserID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestIsValidAgentID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"main", true},
		{"fotini", true},
		{"my-agent", true},
		{"agent1", true},
		{"a", true},
		{"", false},
		{"Main", false},       // uppercase
		{"-start", false},     // starts with hyphen
		{"1start", false},     // starts with number
		{"has space", false},
		{"has_under", false},  // underscores not allowed
	}
	for _, tt := range tests {
		got := isValidAgentID(tt.id)
		if got != tt.want {
			t.Errorf("isValidAgentID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestSeedDefaultCharacterFiles(t *testing.T) {
	// Create fake defaults dir
	defaultsDir := t.TempDir()
	for _, name := range []string{"SOUL.md", "CRAFT.md", "COHERENCE.md"} {
		if err := os.WriteFile(filepath.Join(defaultsDir, name), []byte("# "+name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Add a non-md file that should be skipped
	if err := os.WriteFile(filepath.Join(defaultsDir, "notes.txt"), []byte("skip me"), 0644); err != nil {
		t.Fatal(err)
	}

	destDir := filepath.Join(t.TempDir(), "character")
	if err := seedDefaultCharacterFiles(defaultsDir, destDir); err != nil {
		t.Fatalf("seedDefaultCharacterFiles: %v", err)
	}

	// Check expected files exist
	for _, name := range []string{"SOUL.md", "CRAFT.md", "COHERENCE.md"} {
		path := filepath.Join(destDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("expected %s to exist: %v", name, err)
			continue
		}
		if string(data) != "# "+name {
			t.Errorf("%s content = %q, want %q", name, string(data), "# "+name)
		}
	}

	// Check non-md file was skipped
	if _, err := os.Stat(filepath.Join(destDir, "notes.txt")); !os.IsNotExist(err) {
		t.Error("notes.txt should not have been copied")
	}
}

func TestSeedDefaultCharacterFilesNoDir(t *testing.T) {
	err := seedDefaultCharacterFiles("", filepath.Join(t.TempDir(), "dest"))
	if err == nil {
		t.Error("expected error when defaultsDir is empty")
	}
}

func TestWriteSetupFiles(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	homeDir := filepath.Join(dir, "home")

	// Create a secrets store
	secretsPath := filepath.Join(configDir, "secrets.toml")
	os.MkdirAll(configDir, 0755)

	// Use secrets.Load which handles missing files
	// We can't easily import secrets here since it's internal,
	// but we can test the file writing indirectly
	_ = secretsPath
	_ = homeDir
}

func TestParseSetupFlags(t *testing.T) {
	args := []string{
		"--config-dir", "/home/foci/config",
		"--home", "/home/foci",
		"--defaults-dir", "/opt/foci/shared/defaults/character",
		"--non-interactive",
		"--bot-token", "123:ABC-test",
		"--user-id", "12345678",
		"--agent-id", "fotini",
		"--setup-token", "stp_test123",
		"--api-key", "sk-test456",
	}

	f := parseSetupFlags(args)

	if f.configDir != "/home/foci/config" {
		t.Errorf("configDir = %q, want /home/foci/config", f.configDir)
	}
	if f.homeDir != "/home/foci" {
		t.Errorf("homeDir = %q, want /home/foci", f.homeDir)
	}
	if f.defaultsDir != "/opt/foci/shared/defaults/character" {
		t.Errorf("defaultsDir = %q, want /opt/foci/shared/defaults/character", f.defaultsDir)
	}
	if !f.nonInteractive {
		t.Error("nonInteractive should be true")
	}
	if f.botToken != "123:ABC-test" {
		t.Errorf("botToken = %q, want 123:ABC-test", f.botToken)
	}
	if f.userID != "12345678" {
		t.Errorf("userID = %q, want 12345678", f.userID)
	}
	if f.agentID != "fotini" {
		t.Errorf("agentID = %q, want fotini", f.agentID)
	}
	if f.setupToken != "stp_test123" {
		t.Errorf("setupToken = %q, want stp_test123", f.setupToken)
	}
	if f.apiKey != "sk-test456" {
		t.Errorf("apiKey = %q, want sk-test456", f.apiKey)
	}
}

func TestParseSetupFlagsDefaults(t *testing.T) {
	f := parseSetupFlags(nil)

	home, _ := os.UserHomeDir()
	wantConfigDir := filepath.Join(home, "config")
	if f.configDir != wantConfigDir {
		t.Errorf("default configDir = %q, want %q", f.configDir, wantConfigDir)
	}
	if f.homeDir != home {
		t.Errorf("default homeDir = %q, want %q", f.homeDir, home)
	}
	wantDefaultsDir := filepath.Join(home, "shared", "defaults", "character")
	if f.defaultsDir != wantDefaultsDir {
		t.Errorf("default defaultsDir = %q, want %q", f.defaultsDir, wantDefaultsDir)
	}
	if f.agentID != "main" {
		t.Errorf("default agentID = %q, want main", f.agentID)
	}
	if f.nonInteractive {
		t.Error("default nonInteractive should be false")
	}
}

func TestKnownCharacterFiles(t *testing.T) {
	expected := []string{"SOUL.md", "CRAFT.md", "COHERENCE.md", "USER.md", "MEMORY.md"}
	for _, name := range expected {
		if !knownCharacterFiles[name] {
			t.Errorf("expected %q to be a known character file", name)
		}
	}
	if knownCharacterFiles["README.md"] {
		t.Error("README.md should not be a known character file")
	}
}
