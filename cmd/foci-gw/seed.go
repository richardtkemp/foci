package main

import (
	"os"
	"path/filepath"

	"foci/internal/log"
	"foci/internal/provision"
	"foci/shared"
)

// seedSharedDefaults seeds all default resources (character files, openclaw files,
// crontab template, prompts, skills) to ~/shared/ from embedded defaults.
// Files that already exist on disk are never overwritten.
func seedSharedDefaults(fileMode os.FileMode) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	sharedDir := filepath.Join(home, "shared")

	if err := provision.SeedDefaults(shared.DefaultsFS, sharedDir, fileMode); err != nil {
		log.Warnf("main", "seed shared defaults: %v", err)
	}
	seedDefaultPrompts(filepath.Join(sharedDir, "prompts"), fileMode)
	seedDefaultSkills(filepath.Join(sharedDir, "skills"), fileMode)
}
