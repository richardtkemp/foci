package main

import (
	"os"
	"path/filepath"
	"testing"

	"foci/internal/provision"

	_ "foci/internal/telegram" // register provider for SetupProviders
)

// Verifies parseSetupFlags correctly parses all supported flags.
func TestParseSetupFlags(t *testing.T) {
	args := []string{
		"--config-dir", "/home/foci/config",
		"--non-interactive",
		"--bot-token", "123:ABC-test",
		"--user-id", "12345678",
		"--agent-id", "fotini",
		"--display-name", "Fotini",
		"--model", "opus",
		"--setup-token", "stp_test123",
		"--api-key", "sk-test456",
	}

	f := parseSetupFlags(args)

	if f.configDir != "/home/foci/config" {
		t.Errorf("configDir = %q, want /home/foci/config", f.configDir)
	}
	if !f.nonInteractive {
		t.Error("nonInteractive should be true")
	}
	if f.providerFlags["bot-token"] != "123:ABC-test" {
		t.Errorf("providerFlags[bot-token] = %q, want 123:ABC-test", f.providerFlags["bot-token"])
	}
	if f.providerFlags["user-id"] != "12345678" {
		t.Errorf("providerFlags[user-id] = %q, want 12345678", f.providerFlags["user-id"])
	}
	if f.agentID != "fotini" {
		t.Errorf("agentID = %q, want fotini", f.agentID)
	}
	if f.displayName != "Fotini" {
		t.Errorf("displayName = %q, want Fotini", f.displayName)
	}
	if f.model != "opus" {
		t.Errorf("model = %q, want opus", f.model)
	}
	if f.setupToken != "stp_test123" {
		t.Errorf("setupToken = %q, want stp_test123", f.setupToken)
	}
	if f.apiKey != "sk-test456" {
		t.Errorf("apiKey = %q, want sk-test456", f.apiKey)
	}
}

// Verifies parseSetupFlags applies sensible defaults when no flags are given.
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
	if f.agentID != "main" {
		t.Errorf("default agentID = %q, want main", f.agentID)
	}
	if f.nonInteractive {
		t.Error("default nonInteractive should be false")
	}
}

// Verifies provision.IsValidAgentID works correctly through the setup code path.
func TestValidationFunctions(t *testing.T) {
	if !provision.IsValidAgentID("my-agent") {
		t.Error("expected valid agent ID")
	}
	if provision.IsValidAgentID("-bad") {
		t.Error("expected invalid agent ID")
	}
}

// Verifies findRepoDefaults doesn't panic regardless of working directory.
func TestFindRepoDefaults(t *testing.T) {
	_ = findRepoDefaults()
}
