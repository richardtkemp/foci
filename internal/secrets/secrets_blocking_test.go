package secrets

import (
	"testing"
)

func TestIsBlockedPath(t *testing.T) {
	// Proves that well-known sensitive paths (the secrets file itself
	// and /proc/self/environ) are blocked by default, while ordinary code files are not.
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

func TestIsBlockedCommand(t *testing.T) {
	// Proves that a shell command is blocked whenever it references
	// a blocked path, regardless of which shell tool is used, while harmless commands pass.
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

func TestAddBlockedPaths(t *testing.T) {
	// Proves that callers can extend the block list at runtime
	// and that newly added paths are immediately rejected by IsBlockedPath.
	s, _ := Load("/nonexistent")
	s.AddBlockedPaths([]string{".env", "credentials.json"})

	if !s.IsBlockedPath(".env") {
		t.Error(".env should be blocked after adding")
	}
	if !s.IsBlockedPath("credentials.json") {
		t.Error("credentials.json should be blocked after adding")
	}
}

func TestIsBlockedPathDefault(t *testing.T) {
	// Proves that even a store loaded from a nonexistent file
	// still enforces the built-in block list, so the defaults are always active.
	s, _ := Load("/nonexistent")
	if !s.IsBlockedPath("secrets.toml") {
		t.Error("secrets.toml should be blocked by default")
	}
	if !s.IsBlockedPath("/proc/self/environ") {
		t.Error("/proc/self/environ should be blocked by default")
	}
}

func TestAddAndCheckBlockedPaths(t *testing.T) {
	// Proves that AddBlockedPaths appends entries without
	// removing existing ones, and that the count grows by exactly the number added.
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
