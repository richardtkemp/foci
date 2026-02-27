// Package prompts provides embedded default prompt files for clod.
// All defaults are loaded at build time via //go:embed.
// Config file overrides still take precedence at runtime.
package prompts

import (
	"embed"
	"strings"
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

// ReplaceVars performs template variable substitution on text.
// Variables use {key} syntax. Only variables present in vars are replaced.
func ReplaceVars(text string, vars map[string]string) string {
	oldnew := make([]string, 0, len(vars)*2)
	for k, v := range vars {
		oldnew = append(oldnew, "{"+k+"}", v)
	}
	return strings.NewReplacer(oldnew...).Replace(text)
}
