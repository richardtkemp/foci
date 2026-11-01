package toolformat

import (
	"encoding/json"

	"foci/internal/skills"
)

// LoadedSkill returns the chat chip for a tool call that loads a skill file,
// or "" when the call is not one. This is the one rule every platform shares
// (#1299, #2392): it resolves the read call's path and decides —
//
//   - a markdown file inside a skill directory under one of scope's roots
//     (scope.FileFromReadPath) is a chip, and ref names the file so the app can
//     fetch its current text (openable = true). Its SKILL.md reads
//     "📖 Loaded skill: <name>"; any other file reads
//     "📖 Loaded skill file: <name>/<file>".
//   - otherwise a SKILL.md anywhere (skills.NameFromReadPath, root-less) is
//     still today's "📖 Loaded skill: <name>" chip, with nothing to open.
//
// Every call gets its own chip: the same file read twice in a turn is two
// chips, one per read, so the transcript shows each read where it happened.
func LoadedSkill(toolName string, m map[string]json.RawMessage, scope skills.ReadScope) (line string, ref skills.FileRef, openable bool) {
	path := readToolPath(toolName, m)
	if ref, ok := scope.FileFromReadPath(path); ok {
		if ref.IsSkillMD() {
			return "📖 Loaded skill: " + ref.Skill, ref, true
		}
		return "📖 Loaded skill file: " + ref.Skill + "/" + ref.File, ref, true
	}
	if name, ok := skills.NameFromReadPath(path); ok {
		return "📖 Loaded skill: " + name, skills.FileRef{}, false
	}
	return "", skills.FileRef{}, false
}

// LoadedSkillLine is LoadedSkill's chip text alone, for the platforms that
// show the chip but cannot open it (Telegram, Discord).
func LoadedSkillLine(toolName string, m map[string]json.RawMessage, scope skills.ReadScope) string {
	line, _, _ := LoadedSkill(toolName, m, scope)
	return line
}

// readToolPath resolves the file path a read tool call targets, across the
// tool names and param keys the transports use: Claude Code's Read carries
// file_path, foci's own read tool (API transport) carries path, and
// opencode's read carries filePath (that key is unverified in this checkout —
// no fixture carries a read part's input; if it is wrong the branch is simply
// dead and the generic summary shows, never a false chip). Non-read tools
// return "" — including a write or edit aimed at a SKILL.md, and the exec
// bridge's foci_read, which reaches the tracker as a shell call, never as a
// read.
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
