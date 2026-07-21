package skills

import (
	"context"
	"strings"
	"time"

	"foci/internal/procx"
)

// gitOpTimeout bounds each individual git subprocess call (rev-parse/log/show)
// used by AttributeToGit. These are local, read-only, and fast; a generous
// timeout just protects against a wedged git process on a huge/locked repo.
const gitOpTimeout = 10 * time.Second

// GitReport is a skill change attributed to a real git commit: a commit that
// landed strictly within the monitored window and touches at least one of the
// skill's changed/created files. Markdown is the commit's message plus the
// diff of the touched files (via `git show`), ready to write to a .md file
// and deliver as an attachment.
type GitReport struct {
	SkillDir string // absolute path to the skill directory (SkillChange.Dir)
	Name     string // skill name (SkillChange.Name)
	Markdown string // commit message(s) + diff of the touched files
}

// AttributeToGit gates skill changes (as detected by Diff, which only knows
// "some file's mtime advanced") on a real git commit that landed inside
// [start, end] and touches at least one of the created/changed files. This
// is the causal check that replaces the old "any mtime moved in the window"
// attribution: the mtime window is real, but a shared skills directory
// (internal/skills/resolve.go: home/shared/skills is common to every agent
// on the box) can have concurrent writers — another session's own
// reflection/session-end/compaction pass, a different agent process, or a
// human/subagent editing skills directly — any of which lands inside an
// unrelated branch's mtime window and gets misattributed. Gating on a
// specific commit, in the window, touching the specific files, ties the
// report to something that actually happened as part of THIS run: the
// convention (see shared/skills/*) is that a reflection pass which edits a
// skill also commits it.
//
// A skill whose directory is not inside a git working tree, or for which no
// commit in the window touches its changed files, produces nothing — no
// false report. When multiple commits in the window touch the files, each
// contributes its message+diff to the same report, oldest first.
func AttributeToGit(ctx context.Context, changes []SkillChange, start, end time.Time) []GitReport {
	var reports []GitReport
	for _, c := range changes {
		files := touchedFiles(c)
		if len(files) == 0 {
			continue
		}
		if !isGitRepo(ctx, c.Dir) {
			continue
		}
		hashes, err := commitsInWindow(ctx, c.Dir, files, start, end)
		if err != nil || len(hashes) == 0 {
			continue
		}
		md, ok := buildMarkdown(ctx, c.Dir, hashes, files)
		if !ok {
			continue
		}
		reports = append(reports, GitReport{SkillDir: c.Dir, Name: c.Name, Markdown: md})
	}
	return reports
}

// SplitByGitRepo partitions changes by whether their skill directory is
// inside a git working tree. Callers use this as the branch point (per
// Dick, #1404): a change in a NON-git-repo skill dir must keep its exact
// pre-#1404 behaviour — the plain mtime-diff text notification via
// FormatChanges — since there is no commit to gate on and no repo to have
// concurrent writers colliding via git history; only a change whose skill
// dir IS a git repo goes through the new commit-window-gated attachment
// path (AttributeToGit), where the over-report fix actually applies.
func SplitByGitRepo(ctx context.Context, changes []SkillChange) (gitDirChanges, nonGitDirChanges []SkillChange) {
	for _, c := range changes {
		if isGitRepo(ctx, c.Dir) {
			gitDirChanges = append(gitDirChanges, c)
		} else {
			nonGitDirChanges = append(nonGitDirChanges, c)
		}
	}
	return gitDirChanges, nonGitDirChanges
}

// touchedFiles returns the created+changed files for a SkillChange, the
// pathspec passed to git (paths are relative to c.Dir, matching how `git -C
// c.Dir ...` resolves them).
func touchedFiles(c SkillChange) []string {
	if len(c.CreatedFiles) == 0 && len(c.ChangedFiles) == 0 {
		return nil
	}
	files := make([]string, 0, len(c.CreatedFiles)+len(c.ChangedFiles))
	files = append(files, c.CreatedFiles...)
	files = append(files, c.ChangedFiles...)
	return files
}

// isGitRepo reports whether dir is inside a git working tree. Skill dirs
// don't have to each be a repo root — git searches upward for `.git`, so
// this is true for a skill dir nested under a repo (e.g. the whole
// shared/skills tree tracked from an ancestor directory).
func isGitRepo(ctx context.Context, dir string) bool {
	octx, cancel := context.WithTimeout(ctx, gitOpTimeout)
	defer cancel()
	cmd := procx.Spawn(octx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// gitWindowPad extends [start, end] on both sides before handing it to `git
// log --since/--until`. Git commit timestamps have 1-second resolution and
// --since/--until are formatted without a fractional part (time.RFC3339), so
// a window edge that lands mid-second can truncate away a commit that is
// genuinely inside the wall-clock window by a few hundred milliseconds. A
// small pad trades a negligible widening (a report can't be more precise
// than git's own timestamp resolution anyway) for not dropping real,
// in-window commits to rounding.
const gitWindowPad = 1 * time.Second

// commitsInWindow returns the hashes (oldest first) of commits under dir
// that touch at least one of files and whose commit date falls within
// [start, end] (padded by gitWindowPad). Empty (no error) when nothing
// matches.
func commitsInWindow(ctx context.Context, dir string, files []string, start, end time.Time) ([]string, error) {
	octx, cancel := context.WithTimeout(ctx, gitOpTimeout)
	defer cancel()
	args := []string{"-C", dir, "log",
		"--since=" + start.Add(-gitWindowPad).Format(time.RFC3339),
		"--until=" + end.Add(gitWindowPad).Format(time.RFC3339),
		"--format=%H", "--"}
	args = append(args, files...)
	cmd := procx.Spawn(octx, "git", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	// git log lists newest-first; report oldest-first (chronological).
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return lines, nil
}

// buildMarkdown renders each commit's message + diff (restricted to files)
// via `git show`, concatenated in order. Returns ok=false if no commit
// produced any output (e.g. all `git show` calls failed).
func buildMarkdown(ctx context.Context, dir string, hashes []string, files []string) (string, bool) {
	var b strings.Builder
	any := false
	for _, h := range hashes {
		out, err := gitShow(ctx, dir, h, files)
		if err != nil || strings.TrimSpace(out) == "" {
			continue
		}
		b.WriteString(out)
		if !strings.HasSuffix(out, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
		any = true
	}
	if !any {
		return "", false
	}
	return strings.TrimRight(b.String(), "\n") + "\n", true
}

// gitShow returns `git show`'s output for hash, with the diff restricted to
// files — this yields the commit metadata + message header followed by the
// diff hunks for just the skill's touched files (not the commit's full
// diff, if it touched other files too).
func gitShow(ctx context.Context, dir, hash string, files []string) (string, error) {
	octx, cancel := context.WithTimeout(ctx, gitOpTimeout)
	defer cancel()
	args := append([]string{"-C", dir, "show", "--no-color", hash, "--"}, files...)
	cmd := procx.Spawn(octx, "git", args...)
	out, err := cmd.Output()
	return string(out), err
}
