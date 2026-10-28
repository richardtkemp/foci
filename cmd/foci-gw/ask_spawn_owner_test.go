package main

import (
	"fmt"
	"path/filepath"
	"testing"

	"foci/internal/platform"
	"foci/internal/session"
)

// TestAskFromSpawnShownInOwner proves #2630: a foci_ask from a spawn's branch
// is shown in the spawn's CURRENT owner (here the parent L1 of the reported
// leaf L2 that started it, after L1's fan-in: Spawner.BranchOwner, covered by
// tools.TestSpawnBranchOwnerFollowsHandover), and falls back up the chain
// from the owner when the owner's chat cannot show it. Before, it went only
// to the branch's own chat, which on the app does not exist.
func TestAskFromSpawnShownInOwner(t *testing.T) {
	t.Parallel()
	const (
		spawn = "main/c1/b300" // the spawn's branch
		l2    = "main/c1/b200" // the reported leaf that started it
		l1    = "main/c1/b100" // L2's parent, which has fanned in: the owner
		root  = "main/c1"
	)
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	for child, parent := range map[string]string{spawn: l2, l2: l1, l1: root} {
		idx.Upsert(session.SessionIndexEntry{SessionKey: child, ParentSessionKey: parent, Status: session.SessionStatusActive})
	}
	owner := func(branchKey string) (string, bool) {
		if branchKey == spawn {
			return l1, true
		}
		return "", false
	}

	for i, tc := range []struct {
		name  string
		bound map[string]bool
		want  string
	}{
		{"owner shows it", map[string]bool{l2: true, l1: true, root: true}, l1},
		{"owner cannot, root can", map[string]bool{l2: true, root: true}, root},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := &fakeAppHub{bound: tc.bound}
			chats := newAskChats(owner, backendPromptDeps{sessionIdx: idx, agentID: "main", live: func(k string) bool { return k != l2 }})
			msgID := fmt.Sprintf("ask-2630-%d", i)
			id := newAskPresentFn("main", hub, chats)(spawn, msgID, "Which way?", "Q", askChoices(), func(string) {})
			t.Cleanup(func() { platform.CancelInteractiveMessage(msgID, "done") })

			if id == "" || len(hub.shownIn) != 1 || hub.shownIn[0] != tc.want {
				t.Fatalf("shown in %v (id %q), want [%s]", hub.shownIn, id, tc.want)
			}
		})
	}

	// Any other session's ask stays in its own chat.
	if got := newAskChats(owner, backendPromptDeps{sessionIdx: idx}).of(l1); len(got) != 1 || got[0] != l1 {
		t.Errorf("chats of a non-spawn session = %v, want [%s]", got, l1)
	}
}
