package toolformat

import (
	"encoding/json"
	"testing"
)

// TestLoadedSkillLine proves the shared chip rule: every read-tool form that
// reaches the formatter (CC's Read/file_path, the API transport's read/path,
// opencode's read/filePath) maps a SKILL.md path to the chip line, and every
// other call — an ordinary read, a sub-file inside a skill dir, a write or
// edit aimed at a SKILL.md, a non-read tool, a call with no params — yields
// "" so the caller keeps its generic summary (#1299).
func TestLoadedSkillLine(t *testing.T) {
	const skill = "/home/foci/shared/skills/writing-code/SKILL.md"
	tests := []struct {
		name     string
		toolName string
		m        map[string]json.RawMessage
		want     string
	}{
		{"ccstream Read form", "Read", raw(map[string]string{"file_path": skill}), "📖 Loaded skill: writing-code"},
		{"API read form", "read", raw(map[string]string{"path": skill}), "📖 Loaded skill: writing-code"},
		{"opencode read form", "read", raw(map[string]string{"filePath": skill}), "📖 Loaded skill: writing-code"},

		{"ordinary read keeps summary", "read", raw(map[string]string{"path": "/home/foci/notes.txt"}), ""},
		{"ordinary CC Read keeps summary", "Read", raw(map[string]string{"file_path": "/home/foci/notes.txt"}), ""},
		{"sub-file inside skill dir", "read", raw(map[string]string{"path": "/home/foci/shared/skills/writing-code/go.md"}), ""},
		{"write to a SKILL.md is not a load", "write", raw(map[string]string{"path": skill, "content": "x"}), ""},
		{"edit of a SKILL.md is not a load", "edit", raw(map[string]string{"path": skill, "old_string": "a", "new_string": "b"}), ""},
		{"non-read tool", "shell", raw(map[string]string{"command": "cat " + skill}), ""},
		{"nil params", "read", nil, ""},
		{"empty params", "read", map[string]json.RawMessage{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LoadedSkillLine(tt.toolName, tt.m); got != tt.want {
				t.Errorf("LoadedSkillLine(%q) = %q, want %q", tt.toolName, got, tt.want)
			}
		})
	}
}
