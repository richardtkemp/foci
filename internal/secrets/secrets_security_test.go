package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestCheckSecurityMissingFile verifies no warnings for nonexistent file.
func TestCheckSecurityMissingFile(t *testing.T) {
	s, _ := Load("/nonexistent/secrets.toml")
	warnings := s.CheckSecurity()
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for missing file, got: %v", warnings)
	}
}

// TestCheckSecurityEmptyPath verifies no warnings for empty path.
func TestCheckSecurityEmptyPath(t *testing.T) {
	s := &Store{path: ""}
	warnings := s.CheckSecurity()
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for empty path, got: %v", warnings)
	}
}

// TestCheckSecurityBadPermissions verifies warnings for wrong permissions.
func TestCheckSecurityBadPermissions(t *testing.T) {
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

// TestCheckSecurityGroupName verifies security group constant.
func TestCheckSecurityGroupName(t *testing.T) {
	if SecurityGroupName != "foci-secrets" {
		t.Errorf("SecurityGroupName = %q, want foci-secrets", SecurityGroupName)
	}
}

// TestCheckSecurityStatError verifies handling of stat errors.
func TestCheckSecurityStatError(t *testing.T) {
	s := &Store{path: "/nonexistent/does/not/exist.toml"}
	warnings := s.CheckSecurity()
	// No error, just skip non-existent file
	if len(warnings) > 0 {
		t.Errorf("should not warn for nonexistent file: %v", warnings)
	}
}

// TestCheckSecurityGroupNotFound verifies handling when group is not found.
func TestCheckSecurityGroupNotFound(t *testing.T) {
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

// TestCheckSecurityGroupFound verifies checking existing valid group.
func TestCheckSecurityGroupFound(t *testing.T) {
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
