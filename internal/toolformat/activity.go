package toolformat

import (
	"encoding/json"
	"strings"
)

const activityDetailMax = 60

// ActivityLabel returns the running-tool caption for a tool call. It is the
// tool name, except for a shell call, which names what the shell is running
// (#1110). A delegated agent runs every foci tool through its shell, so a
// bare "Bash" says nothing: the caption names the foci_* commands in the
// command line, else the call's own description (CC's Bash and opencode's bash
// both carry one), else falls back to the tool name.
//
// args is the call's JSON input, or the raw command line for codex, whose
// shell calls carry no JSON.
func ActivityLabel(name string, args []byte) string {
	if !isShellTool(name) {
		return name
	}
	command, description := shellArgs(args)
	if cmds := FociCommands(command); len(cmds) > 0 {
		return name + ": " + strings.Join(cmds, ", ")
	}
	if description = strings.TrimSpace(description); description != "" {
		return name + ": " + Truncate(description, activityDetailMax)
	}
	return name
}

// isShellTool reports whether name is a shell tool: CC's "Bash", codex's and
// opencode's "bash", or the API transport's "shell".
func isShellTool(name string) bool {
	switch name {
	case "Bash", "bash", "shell":
		return true
	}
	return false
}

func shellArgs(args []byte) (command, description string) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return string(args), ""
	}
	return str(m, "command"), str(m, "description")
}

// FociCommands returns the distinct foci_* commands invoked in a shell command
// line, in order of first appearance. The shell exposes each foci tool as a
// function named "foci_" + the tool's name (tools/registry.go). A name counts
// only at the start of a word, so a path, a variable or FOCI_SOCK does not.
func FociCommands(command string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; ; {
		j := strings.Index(command[i:], "foci_")
		if j < 0 {
			return out
		}
		start := i + j
		end := start + len("foci_")
		for end < len(command) && isIdentByte(command[end]) {
			end++
		}
		i = end
		if end == start+len("foci_") || (start > 0 && !isCommandBoundary(command[start-1])) {
			continue
		}
		if name := command[start:end]; !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// isCommandBoundary reports whether c can precede a command word: whitespace,
// a shell operator or grouping, or a quote.
func isCommandBoundary(c byte) bool {
	return strings.IndexByte(" \t\n;|&(){`\"'", c) >= 0
}
