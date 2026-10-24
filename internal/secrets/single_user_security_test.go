package secrets

import (
	"os"
	"strings"
	"testing"
)

// checkSingleUserSecurity is the macOS secrets-file check: owned by this user,
// mode 0600. It is called directly here so it is exercised on Linux too.
func TestCheckSingleUserSecurity(t *testing.T) {
	self := uint32(os.Getuid()) //nolint:gosec // test uid fits
	if w := checkSingleUserSecurity("/x/secrets.toml", self, 0o600); len(w) != 0 {
		t.Errorf("own file, 0600: want no warnings, got %v", w)
	}
	w := checkSingleUserSecurity("/x/secrets.toml", self, 0o644)
	if len(w) != 1 || !strings.Contains(w[0], "0644") || !strings.Contains(w[0], "chmod 0600") {
		t.Errorf("0644: want one chmod warning, got %v", w)
	}
	w = checkSingleUserSecurity("/x/secrets.toml", self+1, 0o600)
	if len(w) != 1 || !strings.Contains(w[0], "owner is uid") {
		t.Errorf("other owner: want one owner warning, got %v", w)
	}
}
