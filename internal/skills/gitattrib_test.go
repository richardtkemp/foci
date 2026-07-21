package skills

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runGit runs a git command in dir with a fixed committer identity (no
// reliance on the test host's global git config) and, when commitDate is
// non-zero, a fixed author/committer date so commit-window tests are
// deterministic rather than racing the real clock.
func runGit(t *testing.T, dir string, commitDate time.Time, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:forbidigo // test file: forbidigo is excluded for _test.go (.golangci.yml)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if !commitDate.IsZero() {
		ts := commitDate.Format(time.RFC3339)
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_DATE="+ts, "GIT_COMMITTER_DATE="+ts)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initRepo creates a git repo at dir (t.TempDir()-backed).
func initRepo(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, time.Time{}, "init", "-q")
}

// commitFile writes relPath (relative to dir) and commits it at commitDate.
func commitFile(t *testing.T, dir, relPath, content string, commitDate time.Time) {
	t.Helper()
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, time.Time{}, "add", relPath)
	runGit(t, dir, commitDate, "commit", "-q", "-m", "update "+relPath)
}

func TestAttributeToGit_NotGitRepo(t *testing.T) {
	dir := t.TempDir() // not a git repo
	changes := []SkillChange{
		{Dir: dir, Name: "myskill", ChangedFiles: []string{"SKILL.md"}},
	}
	now := time.Now()
	reports := AttributeToGit(context.Background(), changes, now.Add(-time.Hour), now.Add(time.Hour))
	if len(reports) != 0 {
		t.Fatalf("expected no reports for a non-git dir, got %d: %+v", len(reports), reports)
	}
}

func TestAttributeToGit_NoCommitInWindow(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)
	old := time.Now().Add(-48 * time.Hour)
	commitFile(t, dir, "SKILL.md", "v1", old)

	changes := []SkillChange{
		{Dir: dir, Name: "myskill", ChangedFiles: []string{"SKILL.md"}},
	}
	// Window is nowhere near the commit's timestamp.
	winStart := time.Now().Add(-time.Hour)
	winEnd := time.Now()
	reports := AttributeToGit(context.Background(), changes, winStart, winEnd)
	if len(reports) != 0 {
		t.Fatalf("expected no reports when no commit falls in the window, got %d: %+v", len(reports), reports)
	}
}

func TestAttributeToGit_CommitInWindow(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)
	// Base commit, well before the window.
	commitFile(t, dir, "SKILL.md", "v1\n", time.Now().Add(-time.Hour))

	winStart := time.Now()
	commitTime := winStart.Add(2 * time.Second)
	commitFile(t, dir, "SKILL.md", "v2 — updated body\n", commitTime)
	winEnd := commitTime.Add(2 * time.Second)

	changes := []SkillChange{
		{Dir: dir, Name: "myskill", ChangedFiles: []string{"SKILL.md"}},
	}
	reports := AttributeToGit(context.Background(), changes, winStart, winEnd)
	if len(reports) != 1 {
		t.Fatalf("expected 1 report for a commit inside the window, got %d: %+v", len(reports), reports)
	}
	rep := reports[0]
	if rep.Name != "myskill" {
		t.Errorf("expected Name=myskill, got %q", rep.Name)
	}
	if rep.SkillDir != dir {
		t.Errorf("expected SkillDir=%q, got %q", dir, rep.SkillDir)
	}
	if !strings.Contains(rep.Markdown, "update SKILL.md") {
		t.Errorf("expected commit message in markdown, got:\n%s", rep.Markdown)
	}
	if !strings.Contains(rep.Markdown, "v2") || !strings.Contains(rep.Markdown, "updated body") {
		t.Errorf("expected diff content in markdown, got:\n%s", rep.Markdown)
	}
}

func TestAttributeToGit_CommitOutsideWindowNotReported(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)
	commitFile(t, dir, "SKILL.md", "v1\n", time.Now().Add(-time.Hour))

	// Commit happens well AFTER the window closes — simulates a concurrent
	// writer (another session's own reflection pass, another agent process,
	// a human edit) touching the same shared skills dir outside this run.
	winStart := time.Now()
	winEnd := winStart.Add(2 * time.Second)
	commitFile(t, dir, "SKILL.md", "v2 — unrelated later edit\n", winEnd.Add(time.Hour))

	changes := []SkillChange{
		{Dir: dir, Name: "myskill", ChangedFiles: []string{"SKILL.md"}},
	}
	reports := AttributeToGit(context.Background(), changes, winStart, winEnd)
	if len(reports) != 0 {
		t.Fatalf("expected no reports for a commit outside the window, got %d: %+v", len(reports), reports)
	}
}

func TestAttributeToGit_CommitTouchesDifferentFileNotReported(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)
	commitFile(t, dir, "README.md", "base\n", time.Now().Add(-time.Hour))

	winStart := time.Now()
	commitTime := winStart.Add(time.Second)
	// The commit inside the window touches a DIFFERENT file than the one
	// Diff flagged as changed — must not be reported.
	commitFile(t, dir, "other.md", "unrelated change\n", commitTime)
	winEnd := commitTime.Add(time.Second)

	changes := []SkillChange{
		{Dir: dir, Name: "myskill", ChangedFiles: []string{"SKILL.md"}},
	}
	reports := AttributeToGit(context.Background(), changes, winStart, winEnd)
	if len(reports) != 0 {
		t.Fatalf("expected no reports when the in-window commit doesn't touch the changed file, got %d: %+v", len(reports), reports)
	}
}

func TestAttributeToGit_MultipleCommitsChronological(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)
	commitFile(t, dir, "SKILL.md", "v1\n", time.Now().Add(-time.Hour))

	winStart := time.Now()
	first := winStart.Add(time.Second)
	second := winStart.Add(3 * time.Second)
	commitFile(t, dir, "SKILL.md", "v2 first-in-window\n", first)
	commitFile(t, dir, "SKILL.md", "v3 second-in-window\n", second)
	winEnd := second.Add(2 * time.Second)

	changes := []SkillChange{
		{Dir: dir, Name: "myskill", ChangedFiles: []string{"SKILL.md"}},
	}
	reports := AttributeToGit(context.Background(), changes, winStart, winEnd)
	if len(reports) != 1 {
		t.Fatalf("expected 1 report (both commits merged into one skill's report), got %d", len(reports))
	}
	md := reports[0].Markdown
	firstIdx := strings.Index(md, "first-in-window")
	secondIdx := strings.Index(md, "second-in-window")
	if firstIdx < 0 || secondIdx < 0 {
		t.Fatalf("expected both commit messages in markdown, got:\n%s", md)
	}
	if firstIdx > secondIdx {
		t.Errorf("expected chronological (oldest first) order, got first-in-window at %d, second-in-window at %d", firstIdx, secondIdx)
	}
}

func TestAttributeToGit_NoFilesSkipsSkill(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)
	changes := []SkillChange{
		{Dir: dir, Name: "empty-change", IsNew: false},
	}
	now := time.Now()
	reports := AttributeToGit(context.Background(), changes, now.Add(-time.Hour), now.Add(time.Hour))
	if len(reports) != 0 {
		t.Fatalf("expected no reports for a SkillChange with no created/changed files, got %d", len(reports))
	}
}

// TestSplitByGitRepo is the #1404 regression guard at the skills-package
// level: a change whose skill dir is NOT a git repo must land in
// nonGitDirChanges (so callers keep firing the legacy FormatChanges text
// notification for it unconditionally — Dick did not want that case to
// change), while a change whose dir IS a git repo lands in gitDirChanges
// (so callers gate it through AttributeToGit instead).
func TestSplitByGitRepo(t *testing.T) {
	repoDir := t.TempDir()
	initRepo(t, repoDir)
	nonRepoDir := t.TempDir() // deliberately not a git repo

	changes := []SkillChange{
		{Dir: repoDir, Name: "tracked", ChangedFiles: []string{"SKILL.md"}},
		{Dir: nonRepoDir, Name: "untracked", ChangedFiles: []string{"SKILL.md"}},
	}

	gitDirChanges, nonGitDirChanges := SplitByGitRepo(context.Background(), changes)

	if len(gitDirChanges) != 1 || gitDirChanges[0].Name != "tracked" {
		t.Fatalf("expected gitDirChanges = [tracked], got %+v", gitDirChanges)
	}
	if len(nonGitDirChanges) != 1 || nonGitDirChanges[0].Name != "untracked" {
		t.Fatalf("expected nonGitDirChanges = [untracked], got %+v", nonGitDirChanges)
	}
}
