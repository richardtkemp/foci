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
	block := reg.SystemBlock("")

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
	if block := reg.SystemBlock(""); block != "" {
		t.Errorf("expected empty string, got %q", block)
	}
}

func TestSystemBlockShortPaths(t *testing.T) {
	// Create skills in /tmp/.../shared/skills/reheat/
	dir := t.TempDir() // e.g. /tmp/TestXXX/shared
	skillsDir := filepath.Join(dir, "shared", "skills")
	writeSkillMD(t, skillsDir, "reheat", "---\nname: reheat\ndescription: Clear cooldowns\n---\n")

	reg := Load([]string{skillsDir})

	// Workspace is a sibling of shared — relative path should be shorter
	workspace := filepath.Join(dir, "workspace")
	block := reg.SystemBlock(workspace)

	absPath := filepath.Join(skillsDir, "reheat", "SKILL.md")
	if strings.Contains(block, absPath) {
		t.Errorf("expected relative path, but found absolute path %q in block", absPath)
	}
	if !strings.Contains(block, "../shared/skills/reheat/SKILL.md") {
		t.Errorf("expected relative path ../shared/skills/reheat/SKILL.md in block:\n%s", block)
	}
}

func TestShortPath(t *testing.T) {
	tests := []struct {
		name     string
		absPath  string
		baseDir  string
		expected string
	}{
		{
			name:     "empty base returns abs",
			absPath:  "/home/foci/shared/skills/reheat/SKILL.md",
			baseDir:  "",
			expected: "/home/foci/shared/skills/reheat/SKILL.md",
		},
		{
			name:     "relative shorter",
			absPath:  "/home/foci/shared/skills/reheat/SKILL.md",
			baseDir:  "/home/foci/clutch",
			expected: "../shared/skills/reheat/SKILL.md",
		},
		{
			name:     "abs shorter when deep base",
			absPath:  "/a/b",
			baseDir:  "/x/y/z/w/v",
			expected: "/a/b",
		},
		{
			name:     "same dir",
			absPath:  "/home/foci/workspace/SKILL.md",
			baseDir:  "/home/foci/workspace",
			expected: "./SKILL.md",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shortPath(tt.absPath, tt.baseDir)
			if got != tt.expected {
				t.Errorf("shortPath(%q, %q) = %q, want %q", tt.absPath, tt.baseDir, got, tt.expected)
			}
		})
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

func TestLoadCollisionOverride(t *testing.T) {
	// Verifies that when two directories contain a skill with the same name,
	// the skill from the later directory replaces the earlier one — this is
	// the mechanism for per-agent skills overriding shared skills.
	shared := t.TempDir()
	agent := t.TempDir()
	writeSkillMD(t, shared, "camera", "---\nname: camera\ndescription: Shared camera\n---\n")
	writeSkillMD(t, agent, "camera", "---\nname: camera\ndescription: Agent camera\n---\n")

	reg := Load([]string{shared, agent})
	if reg.Len() != 1 {
		t.Fatalf("expected 1 skill after collision, got %d", reg.Len())
	}
	if reg.All()[0].Description != "Agent camera" {
		t.Errorf("expected agent override, got description = %q", reg.All()[0].Description)
	}
	if reg.All()[0].Dir != filepath.Join(agent, "camera") {
		t.Errorf("expected agent dir, got %q", reg.All()[0].Dir)
	}
}

func TestLoadCollisionPreservesOrder(t *testing.T) {
	// Verifies that when a later directory overrides a skill, it stays in
	// the original position in the list rather than being appended again.
	shared := t.TempDir()
	agent := t.TempDir()
	writeSkillMD(t, shared, "alpha", "---\nname: alpha\ndescription: Shared alpha\n---\n")
	writeSkillMD(t, shared, "beta", "---\nname: beta\ndescription: Shared beta\n---\n")
	writeSkillMD(t, agent, "alpha", "---\nname: alpha\ndescription: Agent alpha\n---\n")

	reg := Load([]string{shared, agent})
	if reg.Len() != 2 {
		t.Fatalf("expected 2 skills, got %d", reg.Len())
	}
	// alpha should still be first (overridden in place), beta second
	if reg.All()[0].Name != "alpha" {
		t.Errorf("first skill = %q, want alpha", reg.All()[0].Name)
	}
	if reg.All()[0].Description != "Agent alpha" {
		t.Errorf("alpha description = %q, want Agent alpha", reg.All()[0].Description)
	}
	if reg.All()[1].Name != "beta" {
		t.Errorf("second skill = %q, want beta", reg.All()[1].Name)
	}
}

// TestShortPathComparison tests shortPath returns the shorter of absolute or relative
func TestShortPathComparison(t *testing.T) {
	tests := []struct {
		name      string
		absPath   string
		baseDir   string
		expectRel bool
	}{
		{
			"relative shorter",
			"/tmp/foci/workspace/test/SKILL.md",
			"/tmp/foci/workspace",
			true, // rel would be "test/SKILL.md"
		},
		{
			"absolute shorter",
			"/tmp/SKILL.md",
			"/very/long/base/directory/path",
			false, // absolute path is shorter
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shortPath(tt.absPath, tt.baseDir)
			if tt.expectRel {
				if strings.HasPrefix(result, "/") {
					t.Errorf("expected relative path, got %q", result)
				}
			} else {
				if result != tt.absPath {
					t.Errorf("shortPath = %q, want %q", result, tt.absPath)
				}
			}
		})
	}
}

// TestShortPathEmptyBase tests shortPath with empty baseDir
func TestShortPathEmptyBase(t *testing.T) {
	absPath := "/absolute/path/to/skill"
	result := shortPath(absPath, "")
	if result != absPath {
		t.Errorf("shortPath with empty baseDir = %q, want %q", result, absPath)
	}
}

// TestShortPathRelError tests shortPath when filepath.Rel fails
func TestShortPathRelError(t *testing.T) {
	// This is hard to test since filepath.Rel rarely fails in practice
	// Test with paths that might cause issues
	absPath := "/abs/path"
	baseDir := "relative/base"
	result := shortPath(absPath, baseDir)
	// Should return the original path when Rel fails
	if result != absPath {
		t.Errorf("shortPath with relative baseDir = %q", result)
	}
}

// TestParseFrontmatterMissing tests parseFrontmatter with missing frontmatter
func TestParseFrontmatterMissing(t *testing.T) {
	content := "no frontmatter here\njust content"
	f := tmpFile(t, content)
	defer f.Close()
	_, err := parseFrontmatter(f)
	if err == nil {
		t.Error("expected error for missing frontmatter")
	}
}

// TestParseFrontmatterUnterminated tests parseFrontmatter with unterminated frontmatter
func TestParseFrontmatterUnterminated(t *testing.T) {
	content := "---\nname: test\ndescription: something\n"
	f := tmpFile(t, content)
	defer f.Close()
	_, err := parseFrontmatter(f)
	if err == nil {
		t.Error("expected error for unterminated frontmatter")
	}
	if !strings.Contains(err.Error(), "unterminated") {
		t.Errorf("expected unterminated error, got %v", err)
	}
}

// TestParseFrontmatterMalformedLines tests parseFrontmatter with malformed lines
func TestParseFrontmatterMalformedLines(t *testing.T) {
	content := `---
name: test
no colon on this line
malformed = format
description: A test skill
---
`
	f := tmpFile(t, content)
	defer f.Close()
	fm, err := parseFrontmatter(f)
	if err != nil {
		t.Fatalf("parseFrontmatter: %v", err)
	}
	// Should skip malformed lines and keep valid ones
	if fm["name"] != "test" {
		t.Errorf("name = %q, want test", fm["name"])
	}
	if fm["description"] != "A test skill" {
		t.Errorf("description = %q, want A test skill", fm["description"])
	}
	if fm["no colon on this line"] != "" {
		t.Errorf("malformed line should not be in map")
	}
}

// TestParseFrontmatterQuotedValues tests parseFrontmatter with various quote styles
func TestParseFrontmatterQuotedValues(t *testing.T) {
	content := `---
single: 'quoted value'
double: "another value"
unquoted: plain
empty_quotes: ""
---
`
	f := tmpFile(t, content)
	defer f.Close()
	fm, err := parseFrontmatter(f)
	if err != nil {
		t.Fatalf("parseFrontmatter: %v", err)
	}

	tests := []struct {
		key  string
		want string
	}{
		{"single", "quoted value"},
		{"double", "another value"},
		{"unquoted", "plain"},
		{"empty_quotes", ""},
	}

	for _, tt := range tests {
		got := fm[tt.key]
		if got != tt.want {
			t.Errorf("%s = %q, want %q", tt.key, got, tt.want)
		}
	}
}

// TestParseSkillFileEmptyName tests parseSkillFile with missing name field
func TestParseSkillFileEmptyName(t *testing.T) {
	dir := t.TempDir()
	content := "---\ndescription: No name here\n---\nContent"
	skillDir := filepath.Join(dir, "skill")
	os.MkdirAll(skillDir, 0755)
	skillPath := filepath.Join(skillDir, "SKILL.md")
	os.WriteFile(skillPath, []byte(content), 0644)

	_, err := parseSkillFile(skillPath, skillDir)
	if err == nil {
		t.Error("expected error for missing name field")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("expected name error, got %v", err)
	}
}

func TestCheckSizes(t *testing.T) {
	dir := t.TempDir()
	small := "---\nname: small\ndescription: Small skill\n---\nShort body.\n"
	writeSkillMD(t, dir, "small", small)

	big := "---\nname: big\ndescription: Big skill\n---\n" + strings.Repeat("x", 2000)
	writeSkillMD(t, dir, "big", big)

	reg := Load([]string{dir})
	if reg.Len() != 2 {
		t.Fatalf("expected 2 skills, got %d", reg.Len())
	}

	// Limit below small — both should warn
	warnings := reg.CheckSizes(10)
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %d: %v", len(warnings), warnings)
	}

	// Limit above both
	warnings = reg.CheckSizes(50000)
	if len(warnings) != 0 {
		t.Errorf("expected 0 warnings with high limit, got %d", len(warnings))
	}

	// Limit between small and big — only big should warn
	warnings = reg.CheckSizes(500)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "big") {
		t.Errorf("warning should mention 'big', got: %s", warnings[0])
	}

	// Zero limit (disabled) — no warnings
	warnings = reg.CheckSizes(0)
	if warnings != nil {
		t.Errorf("expected nil with zero limit, got %v", warnings)
	}

	// Negative limit (disabled) — no warnings
	warnings = reg.CheckSizes(-1)
	if warnings != nil {
		t.Errorf("expected nil with negative limit, got %v", warnings)
	}
}

// TestLoadAutogeneratedFlag proves the autogenerated frontmatter field is
// parsed into Skill.Autogenerated, that unmarked skills default to false,
// and that alternate truthy spellings ("yes"/"1") also register as true.
func TestLoadAutogeneratedFlag(t *testing.T) {
	dir := t.TempDir()
	writeSkillMD(t, dir, "auto-true", "---\nname: auto-true\ndescription: Auto skill\nautogenerated: true\n---\n")
	writeSkillMD(t, dir, "auto-yes", "---\nname: auto-yes\ndescription: Auto skill\nautogenerated: yes\n---\n")
	writeSkillMD(t, dir, "auto-one", "---\nname: auto-one\ndescription: Auto skill\nautogenerated: 1\n---\n")
	writeSkillMD(t, dir, "auto-false", "---\nname: auto-false\ndescription: Reviewed skill\nautogenerated: false\n---\n")
	writeSkillMD(t, dir, "auto-absent", "---\nname: auto-absent\ndescription: Human skill\n---\n")

	reg := Load([]string{dir})
	if reg.Len() != 5 {
		t.Fatalf("expected 5 skills, got %d", reg.Len())
	}

	byName := map[string]bool{}
	for _, s := range reg.All() {
		byName[s.Name] = s.Autogenerated
	}
	for _, name := range []string{"auto-true", "auto-yes", "auto-one"} {
		if !byName[name] {
			t.Errorf("%s: Autogenerated = false, want true", name)
		}
	}
	for _, name := range []string{"auto-false", "auto-absent"} {
		if byName[name] {
			t.Errorf("%s: Autogenerated = true, want false", name)
		}
	}
}

// TestSystemBlockAutogeneratedTag proves the SystemBlock output appends the
// review-me marker for autogenerated skills and leaves reviewed skills clean,
// so future agent sessions can see which skills need refinement.
func TestSystemBlockAutogeneratedTag(t *testing.T) {
	dir := t.TempDir()
	writeSkillMD(t, dir, "draft", "---\nname: draft\ndescription: Auto from reflection\nautogenerated: true\n---\n")
	writeSkillMD(t, dir, "stable", "---\nname: stable\ndescription: Hand-crafted\n---\n")

	reg := Load([]string{dir})
	block := reg.SystemBlock("")

	if !strings.Contains(block, "Auto from reflection [autogenerated — review and refine if used]") {
		t.Errorf("autogenerated skill missing review marker in block:\n%s", block)
	}
	for _, line := range strings.Split(block, "\n") {
		if strings.Contains(line, "stable") && strings.Contains(line, "autogenerated") {
			t.Errorf("stable (human) skill should not carry the autogenerated marker: %q", line)
		}
	}
}

// --- Loader: shared dir scanned once across agents ---

const sharedSkill = "---\nname: shared\ndescription: Shared skill\n---\n"

// TestLoaderScansSharedOnce is the core regression test for the duplicate
// "autogenerated, needs human review" warnings: the shared directory must be
// read from disk exactly once no matter how many agents reference it.
func TestLoaderScansSharedOnce(t *testing.T) {
	shared := t.TempDir()
	writeSkillMD(t, shared, "shared", sharedSkill)

	agentA := t.TempDir()
	agentB := t.TempDir()

	l := NewLoader()
	l.LoadForAgent(shared, agentA)
	l.LoadForAgent(shared, agentB)
	l.LoadForAgent(shared, agentA) // a reload-style repeat must not re-scan

	if got := l.scanCount[shared]; got != 1 {
		t.Fatalf("shared dir scanned %d times, want exactly 1", got)
	}
}

// TestLoaderMergesSharedAndAgent verifies each agent sees the shared skills
// plus its own dedicated skills.
func TestLoaderMergesSharedAndAgent(t *testing.T) {
	shared := t.TempDir()
	writeSkillMD(t, shared, "shared", sharedSkill)

	agentA := t.TempDir()
	writeSkillMD(t, agentA, "alpha", "---\nname: alpha\ndescription: A only\n---\n")
	agentB := t.TempDir()
	writeSkillMD(t, agentB, "beta", "---\nname: beta\ndescription: B only\n---\n")

	l := NewLoader()
	regA := l.LoadForAgent(shared, agentA)
	regB := l.LoadForAgent(shared, agentB)

	if !hasSkill(regA, "shared") || !hasSkill(regA, "alpha") {
		t.Errorf("agent A registry = %v, want shared+alpha", names(regA))
	}
	if hasSkill(regA, "beta") {
		t.Errorf("agent A registry leaked beta: %v", names(regA))
	}
	if !hasSkill(regB, "shared") || !hasSkill(regB, "beta") {
		t.Errorf("agent B registry = %v, want shared+beta", names(regB))
	}
	if hasSkill(regB, "alpha") {
		t.Errorf("agent B registry leaked alpha: %v", names(regB))
	}
}

// TestLoaderAgentOverrideDoesNotCorruptSharedCache guards the slice-copy in
// LoadForAgent: one agent overriding a shared skill must not change what the
// next agent sees for that name.
func TestLoaderAgentOverrideDoesNotCorruptSharedCache(t *testing.T) {
	shared := t.TempDir()
	writeSkillMD(t, shared, "shared", sharedSkill)

	// agentA defines its own "shared" skill, overriding the shared one.
	agentA := t.TempDir()
	writeSkillMD(t, agentA, "shared", "---\nname: shared\ndescription: A override\n---\n")
	agentB := t.TempDir() // no override

	l := NewLoader()
	regA := l.LoadForAgent(shared, agentA)
	regB := l.LoadForAgent(shared, agentB)

	if got := skillDesc(regA, "shared"); got != "A override" {
		t.Errorf("agent A shared description = %q, want %q", got, "A override")
	}
	if got := skillDesc(regB, "shared"); got != "Shared skill" {
		t.Errorf("agent B shared description = %q, want %q (cache corrupted)", got, "Shared skill")
	}
	// Each agent registry holds exactly one "shared" entry.
	if n := countSkill(regA, "shared"); n != 1 {
		t.Errorf("agent A has %d shared entries, want 1", n)
	}
}

// TestLoaderMissingSharedDir tolerates a non-existent shared directory.
func TestLoaderMissingSharedDir(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "does-not-exist")
	agentA := t.TempDir()
	writeSkillMD(t, agentA, "alpha", "---\nname: alpha\ndescription: A only\n---\n")

	l := NewLoader()
	reg := l.LoadForAgent(shared, agentA)
	if !hasSkill(reg, "alpha") || reg.Len() != 1 {
		t.Errorf("registry = %v, want only alpha", names(reg))
	}
}

func hasSkill(r *Registry, name string) bool {
	for _, s := range r.All() {
		if s.Name == name {
			return true
		}
	}
	return false
}

func countSkill(r *Registry, name string) int {
	n := 0
	for _, s := range r.All() {
		if s.Name == name {
			n++
		}
	}
	return n
}

func skillDesc(r *Registry, name string) string {
	for _, s := range r.All() {
		if s.Name == name {
			return s.Description
		}
	}
	return ""
}

func names(r *Registry) []string {
	var out []string
	for _, s := range r.All() {
		out = append(out, s.Name)
	}
	return out
}

// tmpFile creates a temporary file with content and seeks to start
func tmpFile(t *testing.T, content string) *os.File {
	f, err := os.CreateTemp(t.TempDir(), "*.md")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("seek temp file: %v", err)
	}
	return f
}
