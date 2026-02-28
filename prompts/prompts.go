// Package prompts provides embedded default prompt files for foci.
// All defaults are loaded at build time via //go:embed.
// Config file overrides still take precedence at runtime.
package prompts

import (
	"embed"
	"os"
	"strings"

	"foci/log"
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

// BranchOrientationMultiball returns the default orientation for user-attached
// multiball branches. Template vars: {branch_key}, {parent_key}, {branch_type}.
func BranchOrientationMultiball() string { return read("branch-orientation-multiball.md") }

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

// ResolvePrompt implements 3-state prompt resolution:
//   - path absent/unset ("" or "default"): returns embeddedDefault
//   - path = "none": returns "" (explicitly disabled)
//   - path = "/path/to/file": reads file; on error logs warning + returns embeddedDefault
func ResolvePrompt(path, label, embeddedDefault string) string {
	if path == "" || path == "default" {
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
		log.Warnf("prompts", "%s: read %s: %v — using embedded default", label, path, err)
		return embeddedDefault
	}
	return strings.TrimSpace(string(data))
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
