package main

import (
	"io/fs"
	"os"
	"path/filepath"

	"foci/internal/log"
	skills "foci/shared/skills"
)

// seedDefaultSkills walks the embedded skills/ tree and copies files to dir
// that don't already exist. Each skill is a subdirectory containing at least
// SKILL.md, plus optional references/ subdirectories.
// Users can edit seeded copies — files are never overwritten.
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

		// Skip if file already exists
		if _, err := os.Stat(dest); err == nil {
			return nil
		}

		data, err := skills.FS.ReadFile(path)
		if err != nil {
			log.Warnf("main", "seed skills: read embedded %s: %v", path, err)
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			log.Warnf("main", "seed skills: mkdir %s: %v", filepath.Dir(dest), err)
			return nil
		}
		if err := os.WriteFile(dest, data, fileMode); err != nil {
			log.Warnf("main", "seed skills: write %s: %v", dest, err)
			return nil
		}
		log.Infof("main", "seeded default skill file: %s", dest)
		return nil
	})
}
