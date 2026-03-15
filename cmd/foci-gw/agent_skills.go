package main

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"

	"foci/internal/log"
)

//go:embed all:skills
var skillsFS embed.FS

// seedDefaultSkills walks the embedded skills/ tree and copies files to dir
// that don't already exist. Each skill is a subdirectory containing at least
// SKILL.md, plus optional references/ and scripts/ subdirectories.
// Users can edit seeded copies — files are never overwritten.
func seedDefaultSkills(dir string) {
	_ = fs.WalkDir(skillsFS, "skills", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// Strip the "skills/" prefix to get the relative path within the output dir
		rel, _ := filepath.Rel("skills", path)
		if rel == "." {
			return nil
		}
		dest := filepath.Join(dir, rel)

		if d.IsDir() {
			return nil // directories are created when we write files
		}

		// Skip if file already exists
		if _, err := os.Stat(dest); err == nil {
			return nil
		}

		data, err := skillsFS.ReadFile(path)
		if err != nil {
			log.Warnf("main", "seed skills: read embedded %s: %v", path, err)
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			log.Warnf("main", "seed skills: mkdir %s: %v", filepath.Dir(dest), err)
			return nil
		}
		if err := os.WriteFile(dest, data, 0644); err != nil {
			log.Warnf("main", "seed skills: write %s: %v", dest, err)
			return nil
		}
		log.Infof("main", "seeded default skill file: %s", dest)
		return nil
	})
}
