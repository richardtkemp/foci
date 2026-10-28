package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/session"
)

// TestBackendPrompt_SpawnFollowsOwner proves #2632 (Dick: "Follow the
// owner"): a backend prompt from a spawn's branch goes to the spawn's current
// owner, as its foci_ask does, in the two cases where #2627's walk did not:
// the starting leaf is a live side thread, and the branch's own chat can show
// it (on Telegram/Discord it resolves to the root chat). A session that is
// not a spawn's branch is unchanged.
func TestBackendPrompt_SpawnFollowsOwner(t *testing.T) {
	t.Parallel()
	const (
		spawn = "main/c1/b300" // the spawn's branch
		leaf  = "main/c1/b200" // started it, reported, reopened as a side thread (live)
		owner = "main/c1/b100" // the leaf's parent, which fanned in: the owner
		root  = "main/c1"
	)
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	for child, parent := range map[string]string{spawn: leaf, leaf: owner, owner: root} {
		idx.Upsert(session.SessionIndexEntry{SessionKey: child, ParentSessionKey: parent, Status: session.SessionStatusActive})
	}
	ownerOf := func(branchKey string) (string, bool) {
		if branchKey == spawn {
			return owner, true
		}
		return "", false
	}

	for i, tc := range []struct {
		name  string
		from  string
		bound map[string]bool
		want  string
	}{
		{"side-thread leaf", spawn, map[string]bool{leaf: true, owner: true, root: true}, owner},
		{"own chat can show it", spawn, map[string]bool{spawn: true, leaf: true, owner: true, root: true}, owner},
		{"owner cannot, falls back up", spawn, map[string]bool{leaf: true, root: true}, root},
		{"not a spawn: unchanged", leaf, map[string]bool{leaf: true, owner: true}, leaf},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := &fakeAppHub{bound: tc.bound}
			rec := &failRecorder{}
			reqID := fmt.Sprintf("req-2632-%d", i)
			approval, question := backendPromptFuncs(backendPromptDeps{
				ag:             &agent.Agent{DelegatedManager: &agent.DelegatedManager{}},
				connMgr:        hub,
				sessionIdx:     idx,
				agentID:        "main",
				defaultSession: func() string { return "main/c9" },
				failPrompt:     rec.fail,
				live:           func(string) bool { return true },
				owner:          ownerOf,
			})
			for j, post := range []func(sessionKey, requestID, text, summary, attachmentPath string, choices []delegator.PromptChoice){approval, question} {
				id := fmt.Sprintf("%s-%d", reqID, j)
				before := len(hub.shownIn)
				post(tc.from, id, "Allow Bash?", "Bash", "", promptChoices())
				t.Cleanup(func() { platform.CancelInteractiveMessage(id, "done") })
				if rec.calls != 0 || len(hub.shownIn) != before+1 || hub.shownIn[before] != tc.want {
					t.Fatalf("prompt %d shown in %v (refusals %d), want %s", j, hub.shownIn[before:], rec.calls, tc.want)
				}
				if tc.from == spawn && strings.Contains(hub.texts[before], "↪️") {
					t.Errorf("a spawn's prompt in its owner's chat carries the own-chat-failed line: %q", hub.texts[before])
				}
			}
		})
	}
}
