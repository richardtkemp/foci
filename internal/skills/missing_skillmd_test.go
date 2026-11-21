package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// warnLinesFor returns the WARN lines in out that name path.
func warnLinesFor(out, path string) []string {
	var hits []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "WARN") && strings.Contains(l, path) {
			hits = append(hits, l)
		}
	}
	return hits
}

// TestLoadYoungFolderWithoutSkillMDIsSilent pins #3229: a skill folder created
// moments ago whose SKILL.md is not written yet is skipped with no WARN (Dick
// 2026-11-21: "Skip only if the folder is <1hr old, otherwise still warn").
func TestLoadYoungFolderWithoutSkillMDIsSilent(t *testing.T) {
	buf := captureSkillsLog(t)
	dir := t.TempDir()
	young := filepath.Join(dir, "in-progress")
	if err := os.Mkdir(young, 0o755); err != nil {
		t.Fatal(err)
	}

	if reg := Load([]string{dir}); reg.Len() != 0 {
		t.Fatalf("got %d skills, want 0", reg.Len())
	}
	if w := warnLinesFor(buf.String(), young); len(w) != 0 {
		t.Errorf("a folder under 1 hour old with no SKILL.md must not WARN; got:\n%s", strings.Join(w, "\n"))
	}
}

// TestLoadOldFolderWithoutSkillMDWarns is the other arm of the ruling: a folder
// 1 hour old or older that still has no SKILL.md keeps today's WARN.
func TestLoadOldFolderWithoutSkillMDWarns(t *testing.T) {
	buf := captureSkillsLog(t)
	dir := t.TempDir()
	old := filepath.Join(dir, "abandoned")
	if err := os.Mkdir(old, 0o755); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	if reg := Load([]string{dir}); reg.Len() != 0 {
		t.Fatalf("got %d skills, want 0", reg.Len())
	}
	if w := warnLinesFor(buf.String(), filepath.Join(old, "SKILL.md")); len(w) == 0 {
		t.Errorf("a folder 1 hour or older with no SKILL.md must still WARN; log:\n%s", buf.String())
	}
}

// TestLoadYoungFolderWithBadSkillMDWarns: the young-folder grace covers only a
// MISSING SKILL.md. One that exists but fails to parse still WARNs as before,
// however new the folder is.
func TestLoadYoungFolderWithBadSkillMDWarns(t *testing.T) {
	buf := captureSkillsLog(t)
	dir := t.TempDir()
	writeSkillMD(t, dir, "broken", "---\ndescription: no name field\n---\nbody\n")

	if reg := Load([]string{dir}); reg.Len() != 0 {
		t.Fatalf("got %d skills, want 0", reg.Len())
	}
	file := filepath.Join(dir, "broken", "SKILL.md")
	if w := warnLinesFor(buf.String(), file); len(w) == 0 {
		t.Errorf("a SKILL.md that exists but fails to parse must still WARN; log:\n%s", buf.String())
	}
}
