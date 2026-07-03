package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSeedDefaultSkills_TwoTier proves the seed policy: SKILL.md is seed-if-missing
// (a user may override the entry point), every other golden file is overwritten on
// each seed (foci's shipped content wins), and files the user adds that aren't in
// the embed are left alone. Uses the real embedded foci-usage skill, which ships a
// SKILL.md plus sibling content files.
func TestSeedDefaultSkills_TwoTier(t *testing.T) {
	dir := t.TempDir()
	mode := os.FileMode(0o644)

	// First seed: entry point + a golden sibling both land.
	seedDefaultSkills(dir, mode)
	skillMD := filepath.Join(dir, "foci-usage", "SKILL.md")
	sibling := filepath.Join(dir, "foci-usage", "config.md")
	if _, err := os.Stat(skillMD); err != nil {
		t.Fatalf("SKILL.md not seeded: %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("sibling config.md not seeded: %v", err)
	}
	goldenSibling, _ := os.ReadFile(sibling)

	// User overrides SKILL.md, edits a golden sibling, and adds their own file.
	mustWrite(t, skillMD, "USER OVERRIDE")
	mustWrite(t, sibling, "USER EDIT")
	userFile := filepath.Join(dir, "foci-usage", "my-notes.md")
	mustWrite(t, userFile, "MINE")

	// Second seed.
	seedDefaultSkills(dir, mode)

	if got, _ := os.ReadFile(skillMD); string(got) != "USER OVERRIDE" {
		t.Errorf("SKILL.md was overwritten; want the user override preserved (seed-if-missing), got %q", got)
	}
	if got, _ := os.ReadFile(sibling); string(got) != string(goldenSibling) {
		t.Errorf("golden sibling config.md was NOT restored to golden on re-seed; got %q", got)
	}
	if got, _ := os.ReadFile(userFile); string(got) != "MINE" {
		t.Errorf("user-added file was touched; got %q", got)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
