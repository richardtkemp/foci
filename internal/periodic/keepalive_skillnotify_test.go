package periodic

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/session"
)

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

	var order []string
	var editor, notifiedKey string
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
					os.WriteFile(skillFile, []byte("---\nname: myskill\ndescription: d\n---\nv2\n"), 0o644)
					os.Chtimes(skillFile, now, now)
					editor = parentKey
				}
				return true
			},
		},
		notifySkillChange: func(sessionKey, _ string) {
			notifyCount++
			notifiedKey = sessionKey
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
}
