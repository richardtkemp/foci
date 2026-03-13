package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCheckSecurityMissingFile(t *testing.T) {
	// Proves that CheckSecurity emits no warnings when the
	// store was loaded from a nonexistent path — no file means nothing to audit.
	s, _ := Load("/nonexistent/secrets.toml")
	warnings := s.CheckSecurity()
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for missing file, got: %v", warnings)
	}
}

func TestCheckSecurityEmptyPath(t *testing.T) {
	// Proves that a Store with an empty path field returns
	// no security warnings, handling the zero-value case safely.
	s := &Store{path: ""}
	warnings := s.CheckSecurity()
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for empty path, got: %v", warnings)
	}
}

func TestCheckSecurityBadPermissions(t *testing.T) {
	// Proves that CheckSecurity detects both incorrect
	// file ownership and overly permissive modes, producing warnings that mention
	// "owner"/"uid" and "permission"/"0660" respectively.
	path := writeSecrets(t, `[custom]
key = "val"
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	warnings := s.CheckSecurity()
	if len(warnings) == 0 {
		t.Error("expected warnings for non-root owned file with wrong permissions")
	}

	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "owner") && !strings.Contains(joined, "uid") {
		t.Errorf("expected owner warning in: %s", joined)
	}
	if !strings.Contains(joined, "permission") || !strings.Contains(joined, "0660") {
		t.Errorf("expected permissions warning in: %s", joined)
	}
}

func TestCheckSecurityGroupName(t *testing.T) {
	// Proves the SecurityGroupName constant has the expected
	// value "foci-secrets", which is the Unix group used to gate read access.
	if SecurityGroupName != "foci-secrets" {
		t.Errorf("SecurityGroupName = %q, want foci-secrets", SecurityGroupName)
	}
}

func TestCheckSecurityGroupNotFound(t *testing.T) {
	// Proves that CheckSecurity handles the edge case
	// where the file's GID is 0 (root group) without panicking, even when the expected
	// foci-secrets group doesn't exist on the system.
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.toml")
	os.WriteFile(path, []byte("[test]\nkey = \"val\""), 0600)

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	st, _ := os.Stat(path)
	sys := st.Sys().(*syscall.Stat_t)
	if sys.Gid == 0 {
		// GID is 0 (root), just verify no panic
		warnings := s.CheckSecurity()
		if warnings != nil {
			t.Logf("warnings for root group: %v", warnings)
		}
	}
}

func TestCheckSecurityGroupFound(t *testing.T) {
	// Proves that CheckSecurity does not panic when run
	// against a real file with 0660 permissions, regardless of what warnings it produces.
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.toml")
	os.WriteFile(path, []byte("[test]\nkey = \"val\""), 0660)

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Just verify CheckSecurity doesn't panic
	warnings := s.CheckSecurity()
	if warnings != nil {
		t.Logf("warnings: %v", warnings)
	}
}
