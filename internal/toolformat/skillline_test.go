package toolformat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"foci/internal/skills"
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
		{"parent-hop SKILL.md is not a load", "Read", raw(map[string]string{"file_path": "../../SKILL.md"}), ""},
		{"write to a SKILL.md is not a load", "write", raw(map[string]string{"path": skill, "content": "x"}), ""},
		{"edit of a SKILL.md is not a load", "edit", raw(map[string]string{"path": skill, "old_string": "a", "new_string": "b"}), ""},
		{"non-read tool", "shell", raw(map[string]string{"command": "cat " + skill}), ""},
		{"nil params", "read", nil, ""},
		{"empty params", "read", map[string]json.RawMessage{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LoadedSkillLine(tt.toolName, tt.m, skills.ReadScope{}); got != tt.want {
				t.Errorf("LoadedSkillLine(%q) = %q, want %q", tt.toolName, got, tt.want)
			}
		})
	}
}

// TestLoadedSkill_Scoped proves the root-aware chip rule (#2392): with the
// agent's skill scope, a SKILL.md under a root keeps today's label and is
// openable, a sub-file gets its own chip naming the skill and the file, a
// SKILL.md under no root keeps today's chip with nothing to open, and a
// non-.md file gives none. The rule is stateless, so the same read twice is
// two chips.
func TestLoadedSkill_Scoped(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "skills")
	ws := filepath.Join(base, "ws")
	for path, body := range map[string]string{
		filepath.Join(root, "prediction", "SKILL.md"):  "---\nname: prediction\n---\n",
		filepath.Join(root, "prediction", "method.md"): "m\n",
		filepath.Join(root, "prediction", "data.json"): "{}\n",
		filepath.Join(base, "other", "x", "SKILL.md"):  "---\nname: x\n---\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	scope := skills.ReadScope{WorkDir: ws, Dirs: skills.Dirs{Shared: root}}
	tests := []struct {
		name     string
		toolName string
		m        map[string]json.RawMessage
		line     string
		openable bool
		file     string
	}{
		{"SKILL.md under a root", "Read", raw(map[string]string{"file_path": filepath.Join(root, "prediction", "SKILL.md")}), "📖 Loaded skill: prediction", true, "SKILL.md"},
		{"sub-file under a root", "Read", raw(map[string]string{"file_path": filepath.Join(root, "prediction", "method.md")}), "📖 Loaded skill file: prediction/method.md", true, "method.md"},
		{"workspace-relative sub-file (API read)", "read", raw(map[string]string{"path": "../skills/prediction/method.md"}), "📖 Loaded skill file: prediction/method.md", true, "method.md"},
		{"SKILL.md under no root keeps today's chip", "Read", raw(map[string]string{"file_path": filepath.Join(base, "other", "x", "SKILL.md")}), "📖 Loaded skill: x", false, ""},
		{"non-md file under a root", "Read", raw(map[string]string{"file_path": filepath.Join(root, "prediction", "data.json")}), "", false, ""},
		{"write to a sub-file is not a load", "write", raw(map[string]string{"path": filepath.Join(root, "prediction", "method.md"), "content": "x"}), "", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := 0; i < 2; i++ { // the same read twice: a chip each time
				line, ref, openable := LoadedSkill(tt.toolName, tt.m, scope)
				if line != tt.line || openable != tt.openable || ref.File != tt.file {
					t.Errorf("read %d: LoadedSkill = (%q, %+v, %v), want (%q, file %q, %v)", i+1, line, ref, openable, tt.line, tt.file, tt.openable)
				}
				if got := LoadedSkillLine(tt.toolName, tt.m, scope); got != tt.line {
					t.Errorf("read %d: LoadedSkillLine = %q, want %q", i+1, got, tt.line)
				}
			}
		})
	}
}
