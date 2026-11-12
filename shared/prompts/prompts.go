// Package prompts provides embedded default prompt files for foci.
// All defaults are loaded at build time via //go:embed.
// Config file overrides still take precedence at runtime.
package prompts

import (
	"embed"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"foci/internal/fociroot"
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
// (heartbeat, cron, spawn). Template vars: {branch_key}, {parent_key}, {branch_type},
// {report_rule} (how to report back — depends on whether the branch type may
// use send_to_session; see session.reportRule).
func BranchOrientationHeadless() string { return read("branch-orientation-headless.md") }

// BranchOrientationFacet returns the default orientation for user-attached
// facet branches. Template vars: {branch_key}, {parent_key}, {branch_type}.
func BranchOrientationFacet() string { return read("branch-orientation-facet.md") }

// BranchOrientationDepth returns the default orientation for depth-mode leaf
// branches (#2588). Template vars: {branch_key}, {parent_key}, {branch_type},
// {report_rule}, which the depth manager fills with the leaf's own
// orientation text (the report format and focus its fan-out asked for), and
// {opening_rule}, how the leaf opened: its question to the user, or a task to
// it (#2998).
func BranchOrientationDepth() string { return read("branch-orientation-depth.md") }

// ResolveDepthOrientationTemplate resolves the depth-mode leaf orientation
// template: promptPath when set, else branch-orientation-depth.md from
// searchDirs, else the embedded default.
func ResolveDepthOrientationTemplate(promptPath string, searchDirs ...string) string {
	return ResolvePrompt(promptPath, "branch-orientation-depth.md", BranchOrientationDepth(), searchDirs...)
}

// CompactionSummary returns the default compaction summary prompt.
func CompactionSummary() string { return read("compaction-summary.md") }

// CompactionHandoff returns the default post-compaction handoff message.
func CompactionHandoff() string { return read("compaction-handoff.md") }

// Keepalive returns the default keepalive ping prompt.
func Keepalive() string { return read("keepalive.md") }

// Background returns the default background work prompt.
func Background() string { return read("background.md") }

// Reflection returns the default reflection pass prompt, covering memory
// formation (factual capture), todo filing (friction fixable at source), and
// skill formation (procedural capture).
func Reflection() string { return read("reflection.md") }

// MemoryConsolidation returns the default memory consolidation (MEMORY.md review) prompt.
func MemoryConsolidation() string { return read("memory-consolidation.md") }

// FirstRun returns the onboarding prompt injected on an agent's first session.
func FirstRun() string { return read("first-run.md") }

// FirstRunImport returns the instructions appended to the onboarding of the
// first agent on a new install (#3049): find the human's rules in other AI
// tools and offer them through one ask. askTool is the ask tool's name as this
// agent sees it ("ask" on API agents, "foci_ask" on delegated ones).
func FirstRunImport(askTool string) string {
	return strings.ReplaceAll(read("first-run-import.md"), "{{ask_tool}}", askTool)
}

// WeeklyCharacterReview returns the default weekly character review prompt.
func WeeklyCharacterReview() string { return read("weekly-character-review.md") }

// NudgePreamble returns the default background-nudge preamble that frames an
// injected nudge as a private note to the agent rather than user input.
func NudgePreamble() string { return read("nudge-preamble.md") }

// NudgeReplyInstruction returns the default silence-vs-reply instruction
// appended to standalone nudges. The [[NO_RESPONSE]] sentinel is embedded as a
// literal here (this package cannot import internal/agent); it must stay in
// sync with agent.NoResponseSentinel.
func NudgeReplyInstruction() string { return read("nudge-reply-instruction.md") }

// NudgeUserBoundary returns the default delimiter that closes the background
// nudge region before a real user message bundled into the same turn.
func NudgeUserBoundary() string { return read("nudge-user-boundary.md") }

// ResolvePrompt implements prompt resolution with directory search:
//
//   - path absent/unset ("" or "default"): searches searchDirs for filename,
//     then returns embeddedDefault if not found
//   - path = "none": returns "" (explicitly disabled)
//   - path = "/path/to/file": reads file; on error logs warning + returns embeddedDefault
//
// ResolvePrompt never runs a script: only the keepalive prompt can be one
// (ResolveKeepaliveSource, #1351).
func ResolvePrompt(path, filename, embeddedDefault string, searchDirs ...string) string {
	return resolveSource(path, filename, "", embeddedDefault, searchDirs...).Text
}

// Source is where a prompt comes from. Exactly one of Script and Text applies:
// when Script is set, the prompt is that executable's stdout, produced per use
// by the caller; otherwise Text is the prompt (from Path, or the embedded
// default when Path is ""). Disabled ("none") is Text "" with Disabled set.
type Source struct {
	Script   string // executable whose stdout is the prompt; "" = Text applies
	Text     string // the prompt text when Script == ""
	Path     string // the file Text was read from; "" = embedded default or disabled
	Disabled bool   // configured "none"
}

// KeepaliveScriptName is the script a prompt search dir may hold in place of
// keepalive.md (#1351).
const KeepaliveScriptName = "keepalive.sh"

// ResolveKeepaliveSource resolves the keepalive prompt (#1351). It follows
// ResolvePrompt's rules, with one addition: the prompt may be an executable
// script. In a search dir, a runnable keepalive.sh beats keepalive.md in the
// SAME dir (search order across dirs is unchanged); a keepalive.sh that is
// not executable is ignored. An explicit path that is an executable regular
// file is a script. A script is never read as a prompt file — its mode may
// even forbid reading (0711) — so no read failure is logged for one.
func ResolveKeepaliveSource(path string, searchDirs ...string) Source {
	return resolveSource(path, "keepalive.md", KeepaliveScriptName, Keepalive(), searchDirs...)
}

// resolveSource is the one implementation of prompt resolution. scriptName ""
// disables scripts entirely, which is how every prompt but keepalive resolves.
func resolveSource(path, filename, scriptName, embeddedDefault string, searchDirs ...string) Source {
	if path == "" || path == "default" {
		for _, dir := range searchDirs {
			if scriptName != "" {
				if sp := filepath.Join(dir, scriptName); isRunnable(sp) {
					return Source{Script: sp}
				}
			}
			fp := filepath.Join(dir, filename)
			data, err := os.ReadFile(fp)
			if err == nil {
				return Source{Text: strings.TrimSpace(string(data)), Path: fp}
			}
		}
		return Source{Text: embeddedDefault}
	}
	if path == "none" {
		return Source{Disabled: true}
	}
	// Expand ~ to foci's install root: a prompt path is a config value, and
	// config paths are install-relative ($FOCI_HOME, else $HOME; #2580).
	if strings.HasPrefix(path, "~/") {
		if home, err := fociroot.Dir(); err == nil && home != "" {
			path = home + path[1:]
		}
	}
	if scriptName != "" && isRunnable(path) {
		return Source{Script: path}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		promptsLog.Warnf("%s: read %s: %v — using embedded default", filename, path, err)
		return Source{Text: embeddedDefault}
	}
	return Source{Text: strings.TrimSpace(string(data)), Path: path}
}

// isRunnable reports whether path (symlinks followed) is a regular file this
// process may execute. A directory is X_OK too, hence the regular-file check.
func isRunnable(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	return unix.Access(path, unix.X_OK) == nil
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
