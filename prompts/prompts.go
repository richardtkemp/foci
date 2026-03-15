// Package prompts provides embedded default prompt files for foci.
// All defaults are loaded at build time via //go:embed.
// Config file overrides still take precedence at runtime.
package prompts

import (
	"embed"
	"fmt"
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

// MemoryFormation returns the default memory formation prompt.
func MemoryFormation() string { return read("memory-formation.md") }

// MemoryConsolidation returns the default memory consolidation (MEMORY.md review) prompt.
func MemoryConsolidation() string { return read("memory-consolidation.md") }

// FirstRun returns the onboarding prompt injected on an agent's first session.
func FirstRun() string { return read("first-run.md") }

// KeepaliveCron returns the default cron keepalive prompt for autonomous check-ins.
func KeepaliveCron() string { return read("keepalive-cron.md") }

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

// BuildBranchOrientation constructs orientation text for a branch session.
// Resolves the prompt through ResolvePrompt: explicit path → search dirs → embedded default.
// Template variables: {branch_key}, {parent_key}, {branch_type}, {direct_chat}.
func BuildBranchOrientation(promptPath, branchKey, parentKey, branchType string, directChat bool, searchDirs []string) string {
	var filename, embedded string
	if directChat {
		filename = "branch-orientation-facet.md"
		embedded = BranchOrientationFacet()
	} else {
		filename = "branch-orientation-headless.md"
		embedded = BranchOrientationHeadless()
	}
	text := ResolvePrompt(promptPath, filename, embedded, searchDirs...)
	return ReplaceVars(text, map[string]string{
		"branch_key":  branchKey,
		"parent_key":  parentKey,
		"branch_type": branchType,
		"direct_chat": fmt.Sprintf("%v", directChat),
	})
}

// ResolveOrientPath picks the first non-empty value: agent-level then global.
func ResolveOrientPath(agentLevel, global string) string {
	if agentLevel != "" {
		return agentLevel
	}
	return global
}

// ReplaceVars performs template variable substitution on text.
// Variables use {key} syntax. Only variables present in vars are replaced.
func ReplaceVars(text string, vars map[string]string) string {
	oldnew := make([]string, 0, len(vars)*2)
	for k, v := range vars {
		oldnew = append(oldnew, "{"+k+"}", v)
	}
	return strings.NewReplacer(oldnew...).Replace(text)
}
