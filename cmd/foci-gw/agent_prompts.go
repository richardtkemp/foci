package main

import (
	"os"
	"path/filepath"

	"foci/internal/log"
	"foci/shared/prompts"
)

// seedDefaultPrompts writes embedded prompt files to dir if they don't already
// exist. This gives users editable copies they can customise.
func seedDefaultPrompts(dir string, fileMode os.FileMode, liveBackends map[string]bool) {
	promptFiles := map[string]func() string{
		"keepalive.md":                   prompts.Keepalive,
		"background.md":                  prompts.Background,
		"reflection.md":                  prompts.Reflection,
		"memory-consolidation.md":        prompts.MemoryConsolidation,
		"compaction-summary.md":          prompts.CompactionSummary,
		"compaction-handoff.md":          prompts.CompactionHandoff,
		"branch-orientation-headless.md": prompts.BranchOrientationHeadless,
		"branch-orientation-facet.md":    prompts.BranchOrientationFacet,
		"weekly-character-review.md":     prompts.WeeklyCharacterReview,
	}

	// Seed a backend-<name>.md only for backends actually in use, and only
	// when there's an embedded default for it (skips e.g. claude-code-tmux).
	for backend := range liveBackends {
		if prompts.Backend(backend) == "" {
			continue
		}
		promptFiles["backend-"+backend+".md"] = func() string { return prompts.Backend(backend) }
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Warnf("main", "seed prompts: mkdir %s: %v", dir, err)
		return
	}

	for name, fn := range promptFiles {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			continue // already exists
		}
		if err := os.WriteFile(path, []byte(fn()+"\n"), fileMode); err != nil {
			log.Warnf("main", "seed prompts: write %s: %v", path, err)
			continue
		}
		log.Infof("main", "seeded default prompt: %s", path)
	}
}
