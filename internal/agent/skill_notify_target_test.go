package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/skills"
)

// TestDetectAndNotifySkillChanges_BranchNotifiesRootChat (#1652): the
// session-end (reset) and compaction reflection passes run on a branch key
// (agent/c123/b<ts>). No chat is bound to a branch, so a notice addressed to it
// is dropped (app: no binding for the key) or misrouted. The notice must go to
// the branch's root — the chat whose session was reflected.
func TestDetectAndNotifySkillChanges_BranchNotifiesRootChat(t *testing.T) {
	cases := []struct{ reflecting, want string }{
		{"clutch/c123/b1709596800", "clutch/c123"},
		{"clutch/c123", "clutch/c123"},
	}
	for _, tc := range cases {
		t.Run(tc.reflecting, func(t *testing.T) {
			skillRoot := t.TempDir()
			a := &Agent{SkillDirs: []string{skillRoot}}
			var got []string
			a.SkillChangeNotifyText = func(sessionKey, _ string) { got = append(got, sessionKey) }

			before := skills.Snapshot(a.SkillDirs)
			winStart := time.Now()
			dir := filepath.Join(skillRoot, "new-skill")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: new-skill\n---\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			a.detectAndNotifySkillChanges(context.Background(), tc.reflecting, before, winStart, time.Now())

			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("notified %v, want [%s]", got, tc.want)
			}
		})
	}
}
