package main

import (
	"io/fs"
	"os"
	"path/filepath"

	skills "foci/shared/skills"
)

// seedDefaultSkills walks the embedded skills/ tree and copies files to dir.
//
// Two-tier policy, so foci's shipped skill content stays authoritative while an
// install can still customise:
//   - SKILL.md is a skill's entry point (a brief directory of the skill's other
//     files) — seeded ONLY IF MISSING, so a user may override it and point at
//     their own sibling files.
//   - Every OTHER embedded file (the golden content the SKILL.md refers to) is
//     OVERWRITTEN on each seed, so foci's fixes propagate to existing installs
//     on restart rather than being stranded behind a stale seeded copy.
//   - Files a user adds that aren't in the embed are never touched — the walk
//     only covers golden files, and nothing here deletes.
func seedDefaultSkills(dir string, fileMode os.FileMode) {
	_ = fs.WalkDir(skills.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == "." {
			return nil
		}
		// Skip the embed.go file itself
		if filepath.Base(path) == "embed.go" {
			return nil
		}
		if d.IsDir() {
			return nil
		}

		dest := filepath.Join(dir, path)

		// SKILL.md: seed-if-missing (the user-customisable entry point). Every
		// other golden file falls through and is overwritten below.
		if filepath.Base(path) == "SKILL.md" {
			if _, err := os.Stat(dest); err == nil {
				return nil
			}
		}

		data, err := skills.FS.ReadFile(path)
		if err != nil {
			mainLog.Warnf("seed skills: read embedded %s: %v", path, err)
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			mainLog.Warnf("seed skills: mkdir %s: %v", filepath.Dir(dest), err)
			return nil
		}
		if err := os.WriteFile(dest, data, fileMode); err != nil {
			mainLog.Warnf("seed skills: write %s: %v", dest, err)
			return nil
		}
		mainLog.Infof("seeded skill file: %s", dest)
		return nil
	})
}
