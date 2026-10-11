package skills

import "testing"

// TestNameFromReadPath proves the SKILL.md read detector: a match yields the
// containing directory's name, and everything that is not exactly a
// <dir>/SKILL.md — case variants, SKILLS.md, sub-files, nameless containing
// directories, empty paths — is rejected (#1299).
func TestNameFromReadPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
		ok   bool
	}{
		{"normal shared skills path", "/home/foci/shared/skills/writing-code/SKILL.md", "writing-code", true},
		{"nested skill dir", "/srv/fabro/projects/foci-factory/opencode/skills/writing-code/SKILL.md", "writing-code", true},
		{"SKILL.md directly inside a named root dir", "/home/foci/foci/SKILL.md", "foci", true},
		{"workspace-relative path", "shared/skills/writing-code/SKILL.md", "writing-code", true},
		{"dot-slash prefix", "./shared/skills/writing-code/SKILL.md", "writing-code", true},
		{"trailing slash", "/home/foci/shared/skills/writing-code/SKILL.md/", "writing-code", true},
		{"dot-dot segments", "/home/foci/shared/skills/../skills/writing-code/SKILL.md", "writing-code", true},

		{"sub-file inside a skill dir", "/home/foci/shared/skills/writing-code/go.md", "", false},
		{"lowercase skill.md", "/home/foci/shared/skills/writing-code/skill.md", "", false},
		{"case variant Skill.md", "/home/foci/shared/skills/writing-code/Skill.md", "", false},
		{"SKILLS.md", "/home/foci/shared/skills/writing-code/SKILLS.md", "", false},
		{"plain file", "/home/foci/notes.txt", "", false},
		{"bare SKILL.md has no dir name", "SKILL.md", "", false},
		{"root SKILL.md has no dir name", "/SKILL.md", "", false},
		{"dot-dot SKILL.md has no dir name", "../SKILL.md", "", false},
		{"empty path", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NameFromReadPath(tt.path)
			if ok != tt.ok || got != tt.want {
				t.Errorf("NameFromReadPath(%q) = (%q, %v), want (%q, %v)", tt.path, got, ok, tt.want, tt.ok)
			}
		})
	}
}
