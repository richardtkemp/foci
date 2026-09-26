package nudge

import (
	"encoding/json"
	"regexp"
	"strings"
)

// shellTools are the tools whose input_pattern is matched against the command
// text rather than only the raw tool_input JSON (#2038): CC's Bash tool and the
// API transport's shell tool, both of which carry the script in "command".
// Every other tool is matched against its raw tool_input JSON — no single field
// is the obvious subject there (Read's file_path, Grep's pattern AND path, …).
var shellTools = map[string]bool{"Bash": true, "shell": true}

// hookTruncationMarker is the suffix cmd/foci-cc-hook's truncate appends when a
// tool_input exceeds its maxFieldBytes cap, leaving invalid JSON behind.
const hookTruncationMarker = "...[truncated]"

// commandFieldRe finds the opening quote of the "command" string value.
var commandFieldRe = regexp.MustCompile(`"command"\s*:\s*"`)

// shellCommand returns the decoded command text of a shell tool call, or ""
// for any other tool or an input with no command field.
func shellCommand(tool, input string) string {
	if !shellTools[tool] {
		return ""
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(input), &in) == nil {
		return in.Command
	}
	return truncatedCommand(input)
}

// truncatedCommand recovers the command from a tool_input the CC hook cut off
// mid-JSON. A long heredoc is the usual victim, and its start — the part a
// command-position rule is about — survives the cut.
func truncatedCommand(input string) string {
	loc := commandFieldRe.FindStringIndex(input)
	if loc == nil {
		return ""
	}
	body := input[loc[1]:]
	end := len(body)
	for i := 0; i < len(body); i++ {
		if body[i] == '\\' {
			i++ // skip the escaped byte: \" does not close the string
			continue
		}
		if body[i] == '"' {
			end = i
			break
		}
	}
	lit := body[:end]
	if end == len(body) {
		lit = strings.TrimSuffix(lit, hookTruncationMarker)
	}
	// The cut can land inside an escape (a lone `\`, or `\u00` of `é`);
	// drop trailing bytes until the literal decodes. An escape is at most 6.
	for drop := 0; drop <= 6 && drop <= len(lit); drop++ {
		var s string
		if json.Unmarshal([]byte(`"`+lit[:len(lit)-drop]+`"`), &s) == nil {
			return s
		}
	}
	return ""
}
