package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSkillFile writes a file inside a skill directory, creating parent dirs.
func writeSkillFile(t *testing.T, base, skill, name, content string) {
	t.Helper()
	dir := filepath.Join(base, skill)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func skillFrontmatter(name, desc string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n# " + name + "\n"
}

func TestSnapshotEmptyDir(t *testing.T) {
	dir := t.TempDir()
	snap := Snapshot([]string{dir})
	if len(snap) != 0 {
		t.Fatalf("expected empty snapshot, got %d entries", len(snap))
	}
}

func TestSnapshotMissingDir(t *testing.T) {
	snap := Snapshot([]string{"/nonexistent/path/xyz"})
	if len(snap) != 0 {
		t.Fatalf("expected empty snapshot for missing dir, got %d", len(snap))
	}
}

func TestSnapshotSkillWithFiles(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, dir, "baking", "SKILL.md", skillFrontmatter("baking", "How to bake"))
	writeSkillFile(t, dir, "baking", "script.sh", "#!/bin/bash\necho cake")

	snap := Snapshot([]string{dir})
	skillDir := filepath.Join(dir, "baking")
	files, ok := snap[skillDir]
	if !ok {
		t.Fatalf("expected skill %q in snapshot", skillDir)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if _, ok := files["SKILL.md"]; !ok {
		t.Errorf("expected SKILL.md in snapshot")
	}
	if _, ok := files["script.sh"]; !ok {
		t.Errorf("expected script.sh in snapshot")
	}
}

func TestSnapshotMultipleDirs(t *testing.T) {
	shared := t.TempDir()
	agent := t.TempDir()
	writeSkillFile(t, shared, "common", "SKILL.md", skillFrontmatter("common", "Shared"))
	writeSkillFile(t, agent, "custom", "SKILL.md", skillFrontmatter("custom", "Agent"))

	snap := Snapshot([]string{shared, agent})
	if len(snap) != 2 {
		t.Fatalf("expected 2 skills across dirs, got %d", len(snap))
	}
}

func TestDiffNewSkill(t *testing.T) {
	dir := t.TempDir()
	before := Snapshot([]string{dir})

	writeSkillFile(t, dir, "new-skill", "SKILL.md", skillFrontmatter("new-skill", "Brand new"))
	writeSkillFile(t, dir, "new-skill", "helper.py", "print('hi')")

	after := Snapshot([]string{dir})
	changes := Diff(before, after)
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	c := changes[0]
	if !c.IsNew {
		t.Errorf("expected IsNew=true")
	}
	if c.Name != "new-skill" {
		t.Errorf("expected name 'new-skill', got %q", c.Name)
	}
	if c.Description != "Brand new" {
		t.Errorf("expected description 'Brand new', got %q", c.Description)
	}
	if len(c.CreatedFiles) != 2 {
		t.Fatalf("expected 2 created files, got %d", len(c.CreatedFiles))
	}
	if len(c.ChangedFiles) != 0 {
		t.Errorf("expected 0 changed files, got %d", len(c.ChangedFiles))
	}
}

func TestDiffUpdatedSkill(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, dir, "existing", "SKILL.md", skillFrontmatter("existing", "Desc"))
	time.Sleep(10 * time.Millisecond) // ensure mtime advances

	before := Snapshot([]string{dir})

	// Modify existing file + add new file.
	writeSkillFile(t, dir, "existing", "SKILL.md", skillFrontmatter("existing", "Updated desc"))
	writeSkillFile(t, dir, "existing", "new-file.md", "# stuff")

	after := Snapshot([]string{dir})
	changes := Diff(before, after)
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	c := changes[0]
	if c.IsNew {
		t.Errorf("expected IsNew=false for existing skill")
	}
	if len(c.CreatedFiles) != 1 {
		t.Fatalf("expected 1 created file, got %d: %v", len(c.CreatedFiles), c.CreatedFiles)
	}
	if c.CreatedFiles[0] != "new-file.md" {
		t.Errorf("expected created file 'new-file.md', got %q", c.CreatedFiles[0])
	}
	if len(c.ChangedFiles) != 1 {
		t.Fatalf("expected 1 changed file, got %d: %v", len(c.ChangedFiles), c.ChangedFiles)
	}
	if c.ChangedFiles[0] != "SKILL.md" {
		t.Errorf("expected changed file 'SKILL.md', got %q", c.ChangedFiles[0])
	}
}

func TestDiffNoChanges(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, dir, "stable", "SKILL.md", skillFrontmatter("stable", "Desc"))

	before := Snapshot([]string{dir})
	after := Snapshot([]string{dir})
	changes := Diff(before, after)
	if len(changes) != 0 {
		t.Fatalf("expected 0 changes, got %d", len(changes))
	}
}

func TestDiffDeletedSkillNotReported(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, dir, "doomed", "SKILL.md", skillFrontmatter("doomed", "Desc"))

	before := Snapshot([]string{dir})
	after := Snapshot([]string{dir})
	// Manually remove the skill from after to simulate deletion
	delete(after, filepath.Join(dir, "doomed"))

	changes := Diff(before, after)
	if len(changes) != 0 {
		t.Fatalf("expected 0 changes for deleted skill, got %d", len(changes))
	}
}

func TestDiffUnchangedFileNotReported(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, dir, "skill", "SKILL.md", skillFrontmatter("skill", "Desc"))

	before := Snapshot([]string{dir})
	// Don't modify anything — same mtime
	after := Snapshot([]string{dir})
	changes := Diff(before, after)
	if len(changes) != 0 {
		t.Fatalf("expected 0 changes, got %d", len(changes))
	}
}

func TestFormatChangesEmpty(t *testing.T) {
	if msg := FormatChanges(nil); msg != "" {
		t.Fatalf("expected empty string for nil changes, got %q", msg)
	}
}

func TestFormatChangesNewSkill(t *testing.T) {
	changes := []SkillChange{
		{Name: "baking", Description: "How to bake bread", IsNew: true, CreatedFiles: []string{"SKILL.md", "script.sh"}},
	}
	msg := FormatChanges(changes)
	if msg == "" {
		t.Fatal("expected non-empty message")
	}
	if !strings.Contains(msg, "New skill created: baking") {
		t.Errorf("expected 'New skill created: baking' in message, got:\n%s", msg)
	}
	if !strings.Contains(msg, "How to bake bread") {
		t.Errorf("expected description in message, got:\n%s", msg)
	}
}

func TestFormatChangesUpdatedSkill(t *testing.T) {
	changes := []SkillChange{
		{
			Name:         "baking",
			IsNew:        false,
			CreatedFiles: []string{"new-helper.py"},
			ChangedFiles: []string{"SKILL.md"},
		},
	}
	msg := FormatChanges(changes)
	if msg == "" {
		t.Fatal("expected non-empty message")
	}
	if !strings.Contains(msg, "Skill updated: baking") {
		t.Errorf("expected 'Skill updated: baking' in message, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Files created:") {
		t.Errorf("expected 'Files created:' in message, got:\n%s", msg)
	}
	if !strings.Contains(msg, "new-helper.py") {
		t.Errorf("expected 'new-helper.py' in message, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Files changed:") {
		t.Errorf("expected 'Files changed:' in message, got:\n%s", msg)
	}
	if !strings.Contains(msg, "SKILL.md") {
		t.Errorf("expected 'SKILL.md' in message, got:\n%s", msg)
	}
}

func TestFormatChangesMultiple(t *testing.T) {
	changes := []SkillChange{
		{Name: "new-thing", Description: "A new thing", IsNew: true},
		{Name: "old-thing", Description: "", IsNew: false, ChangedFiles: []string{"SKILL.md"}},
	}
	msg := FormatChanges(changes)
	if !strings.Contains(msg, "New skill created: new-thing") {
		t.Errorf("missing new skill in:\n%s", msg)
	}
	if !strings.Contains(msg, "Skill updated: old-thing") {
		t.Errorf("missing updated skill in:\n%s", msg)
	}
}
