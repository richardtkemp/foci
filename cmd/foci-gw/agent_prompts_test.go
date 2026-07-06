package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSeedDefaultPrompts_BackendGating(t *testing.T) {
	dir := t.TempDir()
	seedDefaultPrompts(dir, 0o644, map[string]bool{"opencode": true, "claude-code-tmux": true})

	if _, err := os.Stat(filepath.Join(dir, "backend-opencode.md")); err != nil {
		t.Errorf("backend-opencode.md should be seeded for a live opencode backend: %v", err)
	}
	// Not a live backend here → not seeded.
	if _, err := os.Stat(filepath.Join(dir, "backend-claude-code.md")); err == nil {
		t.Error("backend-claude-code.md should NOT be seeded when claude-code isn't live")
	}
	// Live but has no embedded default → not seeded.
	if _, err := os.Stat(filepath.Join(dir, "backend-claude-code-tmux.md")); err == nil {
		t.Error("backend-claude-code-tmux.md should NOT be seeded (no embedded default)")
	}
}

func TestSeedDefaultPrompts_DoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "backend-api.md")
	if err := os.WriteFile(p, []byte("USER EDIT"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedDefaultPrompts(dir, 0o644, map[string]bool{"api": true})
	data, _ := os.ReadFile(p)
	if string(data) != "USER EDIT" {
		t.Errorf("seed overwrote a user-edited backend file: %q", data)
	}
}
