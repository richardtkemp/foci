package secrets

import (
	"testing"
)

// TestIsBlockedPath verifies sensitive paths are blocked.
func TestIsBlockedPath(t *testing.T) {
	path := writeSecrets(t, `[custom]
key = "val"
`)
	s, _ := Load(path)

	if !s.IsBlockedPath("secrets.toml") {
		t.Error("secrets.toml should be blocked")
	}
	if !s.IsBlockedPath("/home/user/secrets.toml") {
		t.Error("full path to secrets.toml should be blocked")
	}
	if !s.IsBlockedPath("/proc/self/environ") {
		t.Error("/proc/self/environ should be blocked")
	}
	if s.IsBlockedPath("/home/user/code.go") {
		t.Error("code.go should not be blocked")
	}
}

// TestIsBlockedCommand verifies commands referencing blocked paths are blocked.
func TestIsBlockedCommand(t *testing.T) {
	path := writeSecrets(t, `[custom]
key = "val"
`)
	s, _ := Load(path)

	if !s.IsBlockedCommand("cat secrets.toml") {
		t.Error("cat secrets.toml should be blocked")
	}
	if !s.IsBlockedCommand("cat /proc/self/environ") {
		t.Error("cat /proc/self/environ should be blocked")
	}
	if s.IsBlockedCommand("echo hello") {
		t.Error("echo hello should not be blocked")
	}
}

// TestAddBlockedPaths verifies custom paths can be blocked.
func TestAddBlockedPaths(t *testing.T) {
	s, _ := Load("/nonexistent")
	s.AddBlockedPaths([]string{".env", "credentials.json"})

	if !s.IsBlockedPath(".env") {
		t.Error(".env should be blocked after adding")
	}
	if !s.IsBlockedPath("credentials.json") {
		t.Error("credentials.json should be blocked after adding")
	}
}

// TestIsBlockedPathDefault verifies default blocked paths include secrets file.
func TestIsBlockedPathDefault(t *testing.T) {
	s, _ := Load("/nonexistent")
	if !s.IsBlockedPath("secrets.toml") {
		t.Error("secrets.toml should be blocked by default")
	}
	if !s.IsBlockedPath("/proc/self/environ") {
		t.Error("/proc/self/environ should be blocked by default")
	}
}

// TestAddAndCheckBlockedPaths verifies adding and checking paths together.
func TestAddAndCheckBlockedPaths(t *testing.T) {
	s, _ := Load("/nonexistent")
	originalLen := len(s.blockedPaths)

	s.AddBlockedPaths([]string{".aws/credentials", ".ssh/id_rsa"})

	if !s.IsBlockedPath(".aws/credentials") {
		t.Error(".aws/credentials should be blocked")
	}
	if !s.IsBlockedPath(".ssh/id_rsa") {
		t.Error(".ssh/id_rsa should be blocked")
	}
	if len(s.blockedPaths) != originalLen+2 {
		t.Errorf("expected %d blocked paths, got %d", originalLen+2, len(s.blockedPaths))
	}
}
