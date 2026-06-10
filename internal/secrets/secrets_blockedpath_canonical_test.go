package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIsBlockedPathNoSubstringFalsePositive proves the blocked-path check
// matches whole path components, not raw substrings. A file whose name merely
// contains "secrets.toml" as a substring (e.g. "mysecrets.toml") must NOT be
// blocked — the old substring match (strings.Contains) wrongly flagged it.
func TestIsBlockedPathNoSubstringFalsePositive(t *testing.T) {
	s, _ := Load("/nonexistent")

	if s.IsBlockedPath("/home/user/mysecrets.toml") {
		t.Error("mysecrets.toml should NOT be blocked (substring false positive)")
	}
	if s.IsBlockedPath("/home/user/secrets.tomlx") {
		t.Error("secrets.tomlx should NOT be blocked (substring false positive)")
	}
	// The real component is still blocked.
	if !s.IsBlockedPath("/home/user/secrets.toml") {
		t.Error("secrets.toml should still be blocked")
	}
}

// TestIsBlockedPathSymlinkResolved proves the check resolves symlinks before
// matching. A symlink that points at the secrets file — even with an innocuous
// name that shares no substring with any blocked entry — must be blocked,
// because os.ReadFile would follow it. The old substring match missed this
// (the P1-1 symlink bypass).
func TestIsBlockedPathSymlinkResolved(t *testing.T) {
	path := writeSecrets(t, `[custom]
key = "val"
`)
	s, _ := Load(path) // Load adds the absolute secrets path to the blocklist

	link := filepath.Join(filepath.Dir(path), "harmless-name")
	if err := os.Symlink(path, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if !s.IsBlockedPath(link) {
		t.Error("a symlink resolving to the secrets file should be blocked")
	}
}
