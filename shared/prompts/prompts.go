// Package prompts provides embedded default prompt files for foci.
// All defaults are loaded at build time via //go:embed.
// Config file overrides still take precedence at runtime.
package prompts

import (
	"embed"
	"os"
	"path/filepath"
	"strings"

	"foci/internal/log"
)

//go:embed *.md
var fs embed.FS

func read(name string) string {
	data, err := fs.ReadFile(name)
	if err != nil {
		panic("prompts: missing embedded file: " + name)
	}
	return strings.TrimSpace(string(data))
}

// Backend returns the embedded backend-<name>.md notes for a backend name
// (e.g. "claude-code", "opencode", "api"), or "" if no dedicated file exists.
// Used as the embedded default for the environment block's Backend section.
func Backend(name string) string {
	data, err := fs.ReadFile("backend-" + name + ".md")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Platform returns the embedded platform-<name>.md guidance for a messaging
// platform (e.g. "telegram", "app", "discord"), or "" if none exists. Used as
// the embedded default for the environment block's Platform section.
func Platform(name string) string {
	data, err := fs.ReadFile("platform-" + name + ".md")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// BranchOrientationHeadless returns the default orientation for headless branches
// (heartbeat, cron, spawn). Template vars: {branch_key}, {parent_key}, {branch_type}.
func BranchOrientationHeadless() string { return read("branch-orientation-headless.md") }

// BranchOrientationFacet returns the default orientation for user-attached
// facet branches. Template vars: {branch_key}, {parent_key}, {branch_type}.
func BranchOrientationFacet() string { return read("branch-orientation-facet.md") }

// CompactionSummary returns the default compaction summary prompt.
func CompactionSummary() string { return read("compaction-summary.md") }

// CompactionHandoff returns the default post-compaction handoff message.
func CompactionHandoff() string { return read("compaction-handoff.md") }

// Keepalive returns the default keepalive ping prompt.
func Keepalive() string { return read("keepalive.md") }

// Background returns the default background work prompt.
func Background() string { return read("background.md") }

// Reflection returns the default reflection pass prompt, which covers both
// memory formation (factual capture) and skill formation (procedural capture).
func Reflection() string { return read("reflection.md") }

// MemoryConsolidation returns the default memory consolidation (MEMORY.md review) prompt.
func MemoryConsolidation() string { return read("memory-consolidation.md") }

// FirstRun returns the onboarding prompt injected on an agent's first session.
func FirstRun() string { return read("first-run.md") }

// WeeklyCharacterReview returns the default weekly character review prompt.
func WeeklyCharacterReview() string { return read("weekly-character-review.md") }

// ResolvePrompt implements prompt resolution with directory search:
//
//   - path absent/unset ("" or "default"): searches searchDirs for filename,
//     then returns embeddedDefault if not found
//   - path = "none": returns "" (explicitly disabled)
//   - path = "/path/to/file": reads file; on error logs warning + returns embeddedDefault
func ResolvePrompt(path, filename, embeddedDefault string, searchDirs ...string) string {
	if path == "" || path == "default" {
		for _, dir := range searchDirs {
			fp := filepath.Join(dir, filename)
			data, err := os.ReadFile(fp)
			if err == nil {
				return strings.TrimSpace(string(data))
			}
		}
		return embeddedDefault
	}
	if path == "none" {
		return ""
	}
	// Expand ~ to home directory
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			path = home + path[1:]
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Warnf("prompts", "%s: read %s: %v — using embedded default", filename, path, err)
		return embeddedDefault
	}
	return strings.TrimSpace(string(data))
}

// ResolveOrientationTemplate loads the branch orientation template without
// substituting {branch_key}, {parent_key}, or {branch_type} — those are
// resolved later by session.CreateBranchWithOptions.
func ResolveOrientationTemplate(promptPath string, directChat bool, searchDirs ...string) string {
	var filename, embedded string
	if directChat {
		filename = "branch-orientation-facet.md"
		embedded = BranchOrientationFacet()
	} else {
		filename = "branch-orientation-headless.md"
		embedded = BranchOrientationHeadless()
	}
	return ResolvePrompt(promptPath, filename, embedded, searchDirs...)
}
