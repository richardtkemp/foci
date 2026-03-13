package main

import (
	"crypto/md5" // #nosec G501 - used for content checksums, not security
	"os"
	"path/filepath"

	"foci/internal/command"
	"foci/internal/log"
	"foci/prompts"
)

// isDefaultPrompt compares resolved text to the embedded default via MD5.
func isDefaultPrompt(resolved, embeddedDefault string) bool {
	return md5.Sum([]byte(resolved)) == md5.Sum([]byte(embeddedDefault)) // #nosec G401 - content comparison, not security
}

// seedDefaultPrompts writes embedded prompt files to dir if they don't already
// exist. This gives users editable copies they can customise.
func seedDefaultPrompts(dir string) {
	promptFiles := map[string]func() string{
		"keepalive.md":                    prompts.Keepalive,
		"background.md":                   prompts.Background,
		"memory-formation.md":             prompts.MemoryFormation,
		"memory-consolidation.md":         prompts.MemoryConsolidation,
		"compaction-summary.md":           prompts.CompactionSummary,
		"compaction-handoff.md":           prompts.CompactionHandoff,
		"branch-orientation-headless.md":  prompts.BranchOrientationHeadless,
		"branch-orientation-multiball.md": prompts.BranchOrientationMultiball,
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
		if err := os.WriteFile(path, []byte(fn()+"\n"), 0644); err != nil {
			log.Warnf("main", "seed prompts: write %s: %v", path, err)
			continue
		}
		log.Infof("main", "seeded default prompt: %s", path)
	}
}

// buildBranchOrientation constructs orientation text for a branch session.
// Resolves the prompt through ResolvePrompt: explicit path → search dirs → embedded default.
// Template variables: {branch_key}, {parent_key}, {branch_type}, {direct_chat}.
func buildBranchOrientation(promptPath, branchKey, parentKey, branchType string, searchDirs []string) string {
	filename := "branch-orientation-headless.md"
	embedded := prompts.BranchOrientationHeadless()
	text := prompts.ResolvePrompt(promptPath, filename, embedded, searchDirs...)
	return prompts.ReplaceVars(text, map[string]string{
		"branch_key":  branchKey,
		"parent_key":  parentKey,
		"branch_type": branchType,
		"direct_chat": "false",
	})
}

// resolvePromptInfo builds a PromptInfo for a file-path-based prompt, comparing
// the resolved text against the embedded default via md5 to detect customisation.
func resolvePromptInfo(label, configPath, filename, embeddedDefault string, searchDirs []string) command.PromptInfo {
	if configPath == "none" {
		return command.PromptInfo{Label: label, Filename: filename, Disabled: true}
	}

	resolved := prompts.ResolvePrompt(configPath, filename, embeddedDefault, searchDirs...)
	def := isDefaultPrompt(resolved, embeddedDefault)

	// Find the actual file path being used
	path := configPath
	if path == "" || path == "default" {
		// Search dirs — find which file was used
		for _, dir := range searchDirs {
			fp := filepath.Join(dir, filename)
			if _, err := os.Stat(fp); err == nil {
				path = fp
				break
			}
		}
	}

	if path == "" || path == "default" {
		// Using embedded default, no file on disk
		return command.PromptInfo{Label: label, Filename: filename, Default: def}
	}

	_, err := os.Stat(path)
	return command.PromptInfo{Label: label, Path: path, Filename: filename, Exists: err == nil, Default: def}
}

// inlinePromptInfo builds a PromptInfo for an inline prompt value,
// comparing against the embedded default via md5.
func inlinePromptInfo(label, value, embeddedDefault string) command.PromptInfo {
	if value == "" {
		return command.PromptInfo{Label: label, Inline: embeddedDefault, Default: true}
	}
	if value == "none" {
		return command.PromptInfo{Label: label, Disabled: true}
	}
	return command.PromptInfo{Label: label, Inline: value, Default: isDefaultPrompt(value, embeddedDefault)}
}
