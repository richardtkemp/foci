package skills

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A skill directory reached through a SYMLINK must load exactly like a real one.
//
// os.ReadDir describes the entry, not its target, so DirEntry.IsDir() is false for
// a symlink to a directory. The old `if !entry.IsDir() { continue }` dropped such
// skills BEFORE the SKILL.md parse, so nothing was logged and the loss was silent.
func TestLoadFollowsSymlinkedSkillDir(t *testing.T) {
	real := t.TempDir() // where the skill actually lives
	dir := t.TempDir()  // the scanned skills directory
	writeSkillMD(t, real, "linked-skill", `---
name: linked-skill
description: reached via a symlink
---

Body.
`)
	if err := os.Symlink(filepath.Join(real, "linked-skill"), filepath.Join(dir, "linked-skill")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeSkillMD(t, dir, "plain-skill", `---
name: plain-skill
description: an ordinary directory
---

Body.
`)

	reg := Load([]string{dir})
	if reg.Len() != 2 {
		t.Fatalf("expected 2 skills (plain + symlinked), got %d", reg.Len())
	}
	seen := map[string]bool{}
	for _, sk := range reg.All() {
		seen[sk.Name] = true
	}
	if !seen["linked-skill"] {
		t.Error("symlinked skill did not load")
	}
	if !seen["plain-skill"] {
		t.Error("plain skill did not load")
	}
}

// A symlink that does not resolve to a directory must still be rejected.
func TestLoadRejectsNonDirSymlinks(t *testing.T) {
	real := t.TempDir()
	dir := t.TempDir()
	file := filepath.Join(real, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, filepath.Join(dir, "file-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(real, "gone"), filepath.Join(dir, "dangling")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if n := Load([]string{dir}).Len(); n != 0 {
		t.Fatalf("expected 0 skills from a file-link and a dangling link, got %d", n)
	}
}

// Snapshot must see through the link too: WalkDir does not follow a symlinked
// ROOT, so an unresolved walk records the link itself as one file and never
// notices the real contents changing.
func TestSnapshotFollowsSymlinkedSkillDir(t *testing.T) {
	real := t.TempDir()
	dir := t.TempDir()
	writeSkillMD(t, real, "linked-skill", "---\nname: linked-skill\ndescription: d\n---\n")
	inner := filepath.Join(real, "linked-skill", "reference.md")
	if err := os.WriteFile(inner, []byte("v1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "linked-skill"), filepath.Join(dir, "linked-skill")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	snap := Snapshot([]string{dir})
	files, ok := snap[filepath.Join(dir, "linked-skill")]
	if !ok {
		t.Fatal("symlinked skill absent from snapshot")
	}
	if len(files) != 2 {
		t.Fatalf("expected SKILL.md + reference.md through the link, got %d: %v", len(files), files)
	}

	// A change behind the link must move the snapshot.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(inner, []byte("v2-longer"), 0644); err != nil {
		t.Fatal(err)
	}
	if after := Snapshot([]string{dir})[filepath.Join(dir, "linked-skill")]; after["reference.md"].Equal(files["reference.md"]) {
		t.Error("change behind the symlink did not register in the snapshot")
	}
}
