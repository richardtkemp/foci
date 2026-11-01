package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSeedDefaultPrompts_BackendGating(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedDefaultPrompts(dir, 0o644, map[string]bool{"opencode": true, "no-default-backend": true})

	if _, err := os.Stat(filepath.Join(dir, "backend-opencode.md")); err != nil {
		t.Errorf("backend-opencode.md should be seeded for a live opencode backend: %v", err)
	}
	// Not a live backend here → not seeded.
	if _, err := os.Stat(filepath.Join(dir, "backend-claude-code.md")); err == nil {
		t.Error("backend-claude-code.md should NOT be seeded when claude-code isn't live")
	}
	// Live but has no embedded default → not seeded.
	if _, err := os.Stat(filepath.Join(dir, "backend-no-default-backend.md")); err == nil {
		t.Error("backend-no-default-backend.md should NOT be seeded (no embedded default)")
	}
}

func TestSeedDefaultPrompts_DoesNotOverwrite(t *testing.T) {
	t.Parallel()
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

// TestSeedDefaultPrompts_SeedsSlackPlatformPrompt proves the Slack platform
// guidance is seeded as an editable copy like telegram/app/discord (#2459).
func TestSeedDefaultPrompts_SeedsSlackPlatformPrompt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedDefaultPrompts(dir, 0o644, nil)
	for _, name := range []string{"platform-telegram.md", "platform-app.md", "platform-discord.md", "platform-slack.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s not seeded: %v", name, err)
		}
	}
}
