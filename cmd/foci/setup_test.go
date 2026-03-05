package main

import (
	"os"
	"path/filepath"
	"testing"

	"foci/internal/provision"
)

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
	if f.botToken != "123:ABC-test" {
		t.Errorf("botToken = %q, want 123:ABC-test", f.botToken)
	}
	if f.userID != "12345678" {
		t.Errorf("userID = %q, want 12345678", f.userID)
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

func TestValidationFunctions(t *testing.T) {
	// These now delegate to provision package — verify they work through the setup code path
	if !provision.IsValidBotToken("123456789:AAF-abcdefghijklmnopqrstuv") {
		t.Error("expected valid bot token")
	}
	if provision.IsValidBotToken("invalid") {
		t.Error("expected invalid bot token")
	}

	if !provision.IsValidUserID("12345678") {
		t.Error("expected valid user ID")
	}
	if provision.IsValidUserID("ab") {
		t.Error("expected invalid user ID")
	}

	if !provision.IsValidAgentID("my-agent") {
		t.Error("expected valid agent ID")
	}
	if provision.IsValidAgentID("-bad") {
		t.Error("expected invalid agent ID")
	}
}

func TestFindRepoDefaults(t *testing.T) {
	// This test just verifies the function doesn't panic
	// The actual result depends on where tests are run from
	_ = findRepoDefaults()
}
