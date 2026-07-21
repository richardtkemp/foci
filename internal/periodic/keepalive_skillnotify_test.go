package periodic

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/session"
)

// runGit runs a git command in dir with a fixed identity and (if non-zero)
// a fixed commit date, so the skill-notify routing test's git-attribution
// gate (skills.AttributeToGit — #1404) is deterministic.
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

// TestReflection_SkillNotifyGoesToReflectedSession proves a skill created/updated
// during a reflection branch is notified to the session that was reflected (the
// branch's parent key), not to keys[0]. The second-processed branch edits the
// skill; the notification must target that branch's key.
func TestReflection_SkillNotifyGoesToReflectedSession(t *testing.T) {
	now := time.Now()

	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	for _, key := range []string{"test/c1", "test/c2"} {
		idx.Upsert(session.SessionIndexEntry{
			SessionKey:  key,
			FilePath:    "/tmp/test.jsonl",
			CreatedAt:   now.Add(-24 * time.Hour),
			SessionType: session.SessionTypeChat,
			Status:      session.SessionStatusActive,
		})
		idx.UpdateActivity(key, now.Add(-30*time.Minute))
		idx.StampReflection(key, now.Add(-2*time.Hour))
	}

	skillRoot := t.TempDir()
	runGit(t, skillRoot, time.Time{}, "init", "-q")
	skillFile := filepath.Join(skillRoot, "myskill", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillFile, []byte("---\nname: myskill\ndescription: d\n---\nv1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Back-date so the later rewrite reliably advances the mtime Diff keys on.
	old := now.Add(-time.Hour)
	os.Chtimes(skillFile, old, old)
	runGit(t, skillRoot, old, "add", "myskill/SKILL.md")
	runGit(t, skillRoot, old, "commit", "-q", "-m", "v1")

	var order []string
	var editor, notifiedKey, notifiedName, notifiedMarkdown string
	notifyCount := 0

	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		reflectCfg: config.ResolvedReflection{
			IntervalEnabled:       true,
			Interval:              "1h",
			IntervalPrompt:        "reflection.md",
			NotifyOnSkillCreation: true,
		},
		sessionIndex:    idx,
		skillDirs:       []string{skillRoot},
		lastInteraction: now.Add(-30 * time.Minute),
		lastReflection:  now.Add(-2 * time.Hour),
		agent: &fakeBackgroundAgent{
			branchFn: func(_, parentKey, _ string, _ bool) bool {
				order = append(order, parentKey)
				if len(order) == 2 {
					editTime := time.Now()
					os.WriteFile(skillFile, []byte("---\nname: myskill\ndescription: d\n---\nv2\n"), 0o644)
					os.Chtimes(skillFile, editTime, editTime)
					// Commit within this branch's git-attribution window
					// (winStart/winEnd bracket this call — see maybeReflection)
					// so the skills.AttributeToGit gate (#1404) fires.
					runGit(t, skillRoot, editTime, "commit", "-q", "-am", "v2 update")
					editor = parentKey
				}
				return true
			},
		},
		notifySkillChange: func(sessionKey, skillName, markdown string) {
			notifyCount++
			notifiedKey = sessionKey
			notifiedName = skillName
			notifiedMarkdown = markdown
		},
		done: make(chan struct{}),
	}

	r.maybeReflection()
	waitIdle(t, r)

	if len(order) != 2 {
		t.Fatalf("expected 2 reflection branches, got %d (%v)", len(order), order)
	}
	if editor == order[0] {
		t.Fatalf("editor was keys[0] (%s) — test can't prove non-keys[0] routing", editor)
	}
	if notifyCount != 1 {
		t.Fatalf("notifySkillChange called %d times, want 1", notifyCount)
	}
	if notifiedKey != editor {
		t.Errorf("skill notify went to %q, want the reflected-from session %q (keys[0] was %q)", notifiedKey, editor, order[0])
	}
	if notifiedName != "myskill" {
		t.Errorf("expected notified skill name %q, got %q", "myskill", notifiedName)
	}
	if !strings.Contains(notifiedMarkdown, "v2 update") {
		t.Errorf("expected commit message %q in notified markdown, got:\n%s", "v2 update", notifiedMarkdown)
	}
}

// TestReflection_SkillNotify_NonGitRepo_UsesTextPath is the regression guard
// for #1404's follow-up (Dick): a skill dir that is NOT a git repo must keep
// its exact pre-#1404 behaviour — the plain mtime-diff text notification via
// FormatChanges/NotifySkillChangeText — even though NotifySkillChange (the
// new git-commit-gated attachment path) is also wired. Only the text path
// may fire here; the git path must not.
func TestReflection_SkillNotify_NonGitRepo_UsesTextPath(t *testing.T) {
	now := time.Now()

	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  "test/c1",
		FilePath:    "/tmp/test.jsonl",
		CreatedAt:   now.Add(-24 * time.Hour),
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})
	idx.UpdateActivity("test/c1", now.Add(-30*time.Minute))
	idx.StampReflection("test/c1", now.Add(-2*time.Hour))

	// Deliberately NOT a git repo (no `git init`) — the regression this guards.
	skillRoot := t.TempDir()
	skillFile := filepath.Join(skillRoot, "myskill", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillFile, []byte("---\nname: myskill\ndescription: d\n---\nv1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-time.Hour)
	os.Chtimes(skillFile, old, old)

	var textCount, attachCount int
	var notifiedText string

	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		reflectCfg: config.ResolvedReflection{
			IntervalEnabled:       true,
			Interval:              "1h",
			IntervalPrompt:        "reflection.md",
			NotifyOnSkillCreation: true,
		},
		sessionIndex:    idx,
		skillDirs:       []string{skillRoot},
		lastInteraction: now.Add(-30 * time.Minute),
		lastReflection:  now.Add(-2 * time.Hour),
		agent: &fakeBackgroundAgent{
			branchFn: func(_, _, _ string, _ bool) bool {
				editTime := time.Now()
				os.WriteFile(skillFile, []byte("---\nname: myskill\ndescription: d\n---\nv2\n"), 0o644)
				os.Chtimes(skillFile, editTime, editTime)
				return true
			},
		},
		notifySkillChange: func(sessionKey, skillName, markdown string) {
			attachCount++
		},
		notifySkillChangeText: func(sessionKey, text string) {
			textCount++
			notifiedText = text
		},
		done: make(chan struct{}),
	}

	r.maybeReflection()
	waitIdle(t, r)

	if attachCount != 0 {
		t.Fatalf("non-git-repo change fired the git-attachment path %d times, want 0", attachCount)
	}
	if textCount != 1 {
		t.Fatalf("non-git-repo change fired the text path %d times, want 1 (regression: #1404's git-gating must not swallow non-repo notifications)", textCount)
	}
	if !strings.Contains(notifiedText, "Skill updated: myskill") {
		t.Errorf("expected legacy FormatChanges text, got:\n%s", notifiedText)
	}
}

// TestReflection_SkillNotify_GitRepoNoCommit_NoNotification proves the
// intended #1404 suppression still holds for a git-repo skill dir: an mtime
// change with NO commit landing in the reflection window must produce
// nothing at all — not the attachment path (no commit to attribute to) and
// not the legacy text path either (the change is a git-repo change, so it
// does not fall into the non-repo bucket that keeps the old behaviour).
func TestReflection_SkillNotify_GitRepoNoCommit_NoNotification(t *testing.T) {
	now := time.Now()

	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  "test/c1",
		FilePath:    "/tmp/test.jsonl",
		CreatedAt:   now.Add(-24 * time.Hour),
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})
	idx.UpdateActivity("test/c1", now.Add(-30*time.Minute))
	idx.StampReflection("test/c1", now.Add(-2*time.Hour))

	skillRoot := t.TempDir()
	runGit(t, skillRoot, time.Time{}, "init", "-q")
	skillFile := filepath.Join(skillRoot, "myskill", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillFile, []byte("---\nname: myskill\ndescription: d\n---\nv1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-time.Hour)
	os.Chtimes(skillFile, old, old)
	runGit(t, skillRoot, old, "add", "myskill/SKILL.md")
	runGit(t, skillRoot, old, "commit", "-q", "-m", "v1")

	var textCount, attachCount int

	r := &Runner{
		log:     log.NewComponentLogger("keepalive:test"),
		agentID: "test",
		reflectCfg: config.ResolvedReflection{
			IntervalEnabled:       true,
			Interval:              "1h",
			IntervalPrompt:        "reflection.md",
			NotifyOnSkillCreation: true,
		},
		sessionIndex:    idx,
		skillDirs:       []string{skillRoot},
		lastInteraction: now.Add(-30 * time.Minute),
		lastReflection:  now.Add(-2 * time.Hour),
		agent: &fakeBackgroundAgent{
			branchFn: func(_, _, _ string, _ bool) bool {
				editTime := time.Now()
				// Edit the file (mtime advances, Diff sees a change) but
				// deliberately do NOT commit — no commit lands in the window.
				os.WriteFile(skillFile, []byte("---\nname: myskill\ndescription: d\n---\nv2 uncommitted\n"), 0o644)
				os.Chtimes(skillFile, editTime, editTime)
				return true
			},
		},
		notifySkillChange: func(sessionKey, skillName, markdown string) {
			attachCount++
		},
		notifySkillChangeText: func(sessionKey, text string) {
			textCount++
		},
		done: make(chan struct{}),
	}

	r.maybeReflection()
	waitIdle(t, r)

	if attachCount != 0 {
		t.Errorf("git-repo change with no commit in window fired the attachment path %d times, want 0", attachCount)
	}
	if textCount != 0 {
		t.Errorf("git-repo change with no commit in window fired the text path %d times, want 0 (a git-repo dir's changes must not fall back to the legacy text path)", textCount)
	}
}
