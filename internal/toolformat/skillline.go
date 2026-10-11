package toolformat

import (
	"encoding/json"

	"foci/internal/skills"
)

// LoadedSkillLine returns the chat chip for a tool call that loads a skill —
// a read of a skill's SKILL.md — or "" when the call is not one. This is the
// one rule every platform's compact formatter shares (#1299): it resolves the
// read call's path and lets skills.NameFromReadPath decide, so Telegram,
// Discord and any future platform show the identical line.
func LoadedSkillLine(toolName string, m map[string]json.RawMessage) string {
	name, ok := skills.NameFromReadPath(readToolPath(toolName, m))
	if !ok {
		return ""
	}
	return "📖 Loaded skill: " + name
}

// readToolPath resolves the file path a read tool call targets, across the
// tool names and param keys the transports use: Claude Code's Read carries
// file_path, foci's own read tool (API transport, and the exec bridge's
// foci_read) carries path, and opencode's read carries filePath (that key is
// unverified in this checkout — no fixture carries a read part's input; if it
// is wrong the branch is simply dead and the generic summary shows, never a
// false chip). Non-read tools — including a write or edit aimed at a
// SKILL.md — return "".
func readToolPath(toolName string, m map[string]json.RawMessage) string {
	switch toolName {
	case "Read":
		return str(m, "file_path")
	case "read":
		if p := str(m, "path"); p != "" {
			return p
		}
		return str(m, "filePath")
	}
	return ""
}
