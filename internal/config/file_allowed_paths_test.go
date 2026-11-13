package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestFileAllowedPaths_UnionAndPathResolution (#3076): tools.file_allowed_paths
// decodes globally and per agent, the resolved list is the union (global
// first, duplicates dropped), and ~ expands to $HOME. Unset is empty.
func TestFileAllowedPaths_UnionAndPathResolution(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	rt := resolvedToolsFor(t, `file_allowed_paths = ["/srv/shared", "~/notes"]`,
		`file_allowed_paths = ["/srv/shared", "/data/agent"]`)
	want := []string{"/srv/shared", filepath.Join(home, "notes"), "/data/agent"}
	if !slices.Equal(rt.FileAllowedPaths, want) {
		t.Errorf("FileAllowedPaths = %v, want %v", rt.FileAllowedPaths, want)
	}

	if rt := resolvedToolsFor(t, ``, ``); len(rt.FileAllowedPaths) != 0 {
		t.Errorf("default FileAllowedPaths = %v, want empty", rt.FileAllowedPaths)
	}
}
