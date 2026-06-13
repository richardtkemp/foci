package main

import (
	"os"
	"path/filepath"
	"testing"
)

// validConfigTOML is a minimal config that loads and validates cleanly.
const validConfigTOML = `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "main"
`

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "foci.toml")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestRunConfigCheck_Clean(t *testing.T) {
	// A config that loads and validates with no unknown keys exits 0.
	path := writeTempConfig(t, validConfigTOML)
	if got := runConfigCheck(path); got != 0 {
		t.Errorf("runConfigCheck(clean) = %d, want 0", got)
	}
}

func TestRunConfigCheck_UnknownKey(t *testing.T) {
	// Strict policy: a config that loads fine but carries an unknown/deprecated
	// key (e.g. a renamed setting) must exit 1 — the old setting is silently
	// lost at startup, which is a real upgrade incompatibility. A renamed key
	// only becomes "unknown" relative to the binary that dropped it, so this
	// fixture uses a section that is unknown at any commit.
	path := writeTempConfig(t, validConfigTOML+"\n[bogus_section]\nfoo = \"bar\"\n")
	if got := runConfigCheck(path); got != 1 {
		t.Errorf("runConfigCheck(unknown key) = %d, want 1", got)
	}
}

func TestRunConfigCheck_ParseError(t *testing.T) {
	// A malformed TOML file fails to load and exits 1.
	path := writeTempConfig(t, "[[agents]\nid = \"main\"\n")
	if got := runConfigCheck(path); got != 1 {
		t.Errorf("runConfigCheck(parse error) = %d, want 1", got)
	}
}

func TestRunConfigCheck_ValidateError(t *testing.T) {
	// A syntactically valid TOML that fails Validate (missing required
	// [groups] powerful) exits 1.
	path := writeTempConfig(t, "[[agents]]\nid = \"main\"\n")
	if got := runConfigCheck(path); got != 1 {
		t.Errorf("runConfigCheck(validate error) = %d, want 1", got)
	}
}

func TestRunConfigCheck_MissingFile(t *testing.T) {
	if got := runConfigCheck("/nonexistent/path/foci.toml"); got != 1 {
		t.Errorf("runConfigCheck(missing) = %d, want 1", got)
	}
}
