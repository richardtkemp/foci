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
	t.Parallel()
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

// TestSeedDefaultSkills_ScriptExecutable proves a "#!" file is seeded executable
// and a plain file is not, both on first seed and when overwriting an existing
// copy that has lost its executable bit. Uses the embedded agent-avatar skill.
func TestSeedDefaultSkills_ScriptExecutable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mode := os.FileMode(0o640)
	script := filepath.Join(dir, "agent-avatar", "avatar-gate.py")
	plain := filepath.Join(dir, "foci-usage", "config.md")

	check := func(when string) {
		t.Helper()
		st, err := os.Stat(script)
		if err != nil {
			t.Fatalf("%s: script not seeded: %v", when, err)
		}
		if got := st.Mode().Perm(); got != 0o750 {
			t.Errorf("%s: script mode = %o, want 750", when, got)
		}
		st, err = os.Stat(plain)
		if err != nil {
			t.Fatalf("%s: plain file not seeded: %v", when, err)
		}
		if got := st.Mode().Perm(); got != 0o640 {
			t.Errorf("%s: plain file mode = %o, want 640", when, got)
		}
	}

	seedDefaultSkills(dir, mode)
	check("first seed")

	// An existing install holds a non-executable copy from an older seeder.
	if err := os.Chmod(script, 0o640); err != nil {
		t.Fatal(err)
	}
	seedDefaultSkills(dir, mode)
	check("re-seed")
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
