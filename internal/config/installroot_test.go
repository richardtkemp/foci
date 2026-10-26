package config

import (
	"os"
	"path/filepath"
	"testing"
)

// #2580: with FOCI_HOME set (the macOS LaunchAgent), every path foci derived
// from $HOME comes from the install root instead, while HOME stays the user's.
func TestInstallRootPaths_FociHome(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "foci")
	t.Setenv("HOME", home)
	t.Setenv("FOCI_HOME", root)
	t.Setenv("FOCI_CONFIG", "")

	if got, want := ResolvePath("logs/foci.log"), filepath.Join(root, "logs/foci.log"); got != want {
		t.Errorf("ResolvePath = %q, want %q", got, want)
	}
	if got, _ := DefaultConfigPath(); got != filepath.Join(root, "config", "foci.toml") {
		t.Errorf("DefaultConfigPath = %q, want under the install root", got)
	}

	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte("[[agents]]\nid = \"cam\"\nbackend = \"claude-code\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(root, "data"); cfg.DataDir != want {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, want)
	}
	if want := filepath.Join(root, "cam"); cfg.Agents[0].Workspace != want {
		t.Errorf("Workspace = %q, want %q", cfg.Agents[0].Workspace, want)
	}
	if want := filepath.Join(root, "data", "sessions"); cfg.Sessions.Dir != want {
		t.Errorf("Sessions.Dir = %q, want %q", cfg.Sessions.Dir, want)
	}
}

// Without FOCI_HOME (every Linux install) the same paths stay under HOME.
func TestInstallRootPaths_HomeWhenUnset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FOCI_HOME", "")
	t.Setenv("FOCI_CONFIG", "")

	if got, want := ResolvePath("logs/foci.log"), filepath.Join(home, "logs/foci.log"); got != want {
		t.Errorf("ResolvePath = %q, want %q", got, want)
	}
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte("[[agents]]\nid = \"cam\"\nbackend = \"claude-code\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(home, "cam"); cfg.Agents[0].Workspace != want {
		t.Errorf("Workspace = %q, want %q", cfg.Agents[0].Workspace, want)
	}
}
