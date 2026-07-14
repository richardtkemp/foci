package main

import (
	"os"
	"path/filepath"

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

	// Seed editable copies of the messaging-platform guidance files. Seeded
	// unconditionally (platform-type detection happens later at InitMessaging);
	// an unused copy is harmless since the block only renders for the active one.
	for _, platform := range []string{"telegram", "app", "discord"} {
		promptFiles["platform-"+platform+".md"] = func() string { return prompts.Platform(platform) }
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		mainLog.Warnf("seed prompts: mkdir %s: %v", dir, err)
		return
	}

	for name, fn := range promptFiles {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			continue // already exists
		}
		if err := os.WriteFile(path, []byte(fn()+"\n"), fileMode); err != nil {
			mainLog.Warnf("seed prompts: write %s: %v", path, err)
			continue
		}
		mainLog.Infof("seeded default prompt: %s", path)
	}
}
