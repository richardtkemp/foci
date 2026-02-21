package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkillMD(t *testing.T, dir, name, content string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadBasic(t *testing.T) {
	dir := t.TempDir()
	writeSkillMD(t, dir, "reheat", `---
name: reheat
description: Clear API cooldowns
command: /reheat
script: reheat.sh
---

Run the script.
`)

	// Also write a script file so path resolution can be verified
	os.WriteFile(filepath.Join(dir, "reheat", "reheat.sh"), []byte("#!/bin/sh\necho ok"), 0755)

	reg := Load([]string{dir})
	if reg.Len() != 1 {
		t.Fatalf("expected 1 skill, got %d", reg.Len())
	}

	s := reg.All()[0]
	if s.Name != "reheat" {
		t.Errorf("name = %q, want reheat", s.Name)
	}
	if s.Description != "Clear API cooldowns" {
		t.Errorf("description = %q", s.Description)
	}
	if s.Command != "/reheat" {
		t.Errorf("command = %q, want /reheat", s.Command)
	}
	if s.Script != filepath.Join(dir, "reheat", "reheat.sh") {
		t.Errorf("script = %q", s.Script)
	}
	if s.Dir != filepath.Join(dir, "reheat") {
		t.Errorf("dir = %q", s.Dir)
	}
	if s.Path != filepath.Join(dir, "reheat", "SKILL.md") {
		t.Errorf("path = %q", s.Path)
	}
}

func TestLoadMultipleDirs(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	writeSkillMD(t, dir1, "alpha", "---\nname: alpha\ndescription: First\n---\n")
	writeSkillMD(t, dir2, "beta", "---\nname: beta\ndescription: Second\n---\n")

	reg := Load([]string{dir1, dir2})
	if reg.Len() != 2 {
		t.Fatalf("expected 2 skills, got %d", reg.Len())
	}
}

func TestLoadSkipsMissingName(t *testing.T) {
	dir := t.TempDir()
	writeSkillMD(t, dir, "bad", "---\ndescription: No name field\n---\n")

	reg := Load([]string{dir})
	if reg.Len() != 0 {
		t.Fatalf("expected 0 skills, got %d", reg.Len())
	}
}

func TestLoadSkipsNoFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeSkillMD(t, dir, "plain", "Just some markdown without frontmatter.")

	reg := Load([]string{dir})
	if reg.Len() != 0 {
		t.Fatalf("expected 0 skills, got %d", reg.Len())
	}
}

func TestLoadSkipsEmptyFile(t *testing.T) {
	dir := t.TempDir()
	writeSkillMD(t, dir, "empty", "")

	reg := Load([]string{dir})
	if reg.Len() != 0 {
		t.Fatalf("expected 0 skills, got %d", reg.Len())
	}
}

func TestLoadSkipsMissingDir(t *testing.T) {
	reg := Load([]string{"/nonexistent/path"})
	if reg.Len() != 0 {
		t.Fatalf("expected 0 skills, got %d", reg.Len())
	}
}

func TestLoadSkipsFiles(t *testing.T) {
	dir := t.TempDir()
	// Write a regular file (not a directory) — should be skipped
	os.WriteFile(filepath.Join(dir, "notadir.md"), []byte("hello"), 0644)
	writeSkillMD(t, dir, "valid", "---\nname: valid\ndescription: OK\n---\n")

	reg := Load([]string{dir})
	if reg.Len() != 1 {
		t.Fatalf("expected 1 skill, got %d", reg.Len())
	}
}

func TestLoadQuotedValues(t *testing.T) {
	dir := t.TempDir()
	writeSkillMD(t, dir, "quoted", `---
name: quoted
description: "A description with: colons and spaces"
---
`)

	reg := Load([]string{dir})
	if reg.Len() != 1 {
		t.Fatalf("expected 1 skill, got %d", reg.Len())
	}
	if reg.All()[0].Description != "A description with: colons and spaces" {
		t.Errorf("description = %q", reg.All()[0].Description)
	}
}

func TestSystemBlock(t *testing.T) {
	dir := t.TempDir()
	writeSkillMD(t, dir, "reheat", "---\nname: reheat\ndescription: Clear cooldowns\n---\n")
	writeSkillMD(t, dir, "research", "---\nname: research\ndescription: Web research\n---\n")

	reg := Load([]string{dir})
	block := reg.SystemBlock()

	if !strings.Contains(block, "# Available Skills") {
		t.Error("missing header")
	}
	if !strings.Contains(block, "reheat") {
		t.Error("missing reheat")
	}
	if !strings.Contains(block, "research") {
		t.Error("missing research")
	}
	if !strings.Contains(block, "SKILL.md") {
		t.Error("missing SKILL.md path")
	}
}

func TestSystemBlockEmpty(t *testing.T) {
	reg := Load([]string{})
	if block := reg.SystemBlock(); block != "" {
		t.Errorf("expected empty string, got %q", block)
	}
}

func TestNoScript(t *testing.T) {
	dir := t.TempDir()
	writeSkillMD(t, dir, "simple", "---\nname: simple\ndescription: No script\n---\n")

	reg := Load([]string{dir})
	s := reg.All()[0]
	if s.Script != "" {
		t.Errorf("script = %q, want empty", s.Script)
	}
	if s.Command != "" {
		t.Errorf("command = %q, want empty", s.Command)
	}
}
