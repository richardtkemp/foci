package main

import (
	"fmt"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/tools"
)

// TestBackendPrompt_SpawnerChatClosedIsUnattended proves #2684 requirement 5:
// a spawn's prompt whose owner's chat is closed is not shown anywhere (not
// even by the root/default fallback); it is decided as an unattended ask —
// a permission is denied and a question declined with the unattended
// messages. With the chat open it is shown in the owner's chat as before.
func TestBackendPrompt_SpawnerChatClosedIsUnattended(t *testing.T) {
	t.Parallel()
	const (
		spawn = "main/c2684/b300"
		owner = "main/c2684"
	)
	ownerOf := func(branchKey string) (string, bool) {
		if branchKey == spawn {
			return owner, true
		}
		return "", false
	}
	for _, closed := range []bool{true, false} {
		t.Run(fmt.Sprintf("closed=%v", closed), func(t *testing.T) {
			t.Parallel()
			hub := &fakeAppHub{bound: map[string]bool{owner: true, "main/c9": true}}
			rec := &failRecorder{}
			approval, question := backendPromptFuncs(backendPromptDeps{
				ag:             &agent.Agent{DelegatedManager: &agent.DelegatedManager{}},
				connMgr:        hub,
				agentID:        "main",
				defaultSession: func() string { return "main/c9" },
				failPrompt:     rec.fail,
				owner:          ownerOf,
				chatClosed:     func(sk string) bool { return closed && sk == owner },
			})
			for j, tc := range []struct {
				post func(sessionKey, requestID, text, summary, attachmentPath string, choices []delegator.PromptChoice)
				want string
			}{
				{approval, delegator.UnattendedDenyMessage},
				{question, delegator.UnattendedQuestionMessage},
			} {
				id := fmt.Sprintf("req-2684-closed-%v-%d", closed, j)
				before := len(hub.shownIn)
				calls := rec.calls
				tc.post(spawn, id, "Allow Bash?", "Bash: go build", "", promptChoices())
				t.Cleanup(func() { platform.CancelInteractiveMessage(id, "done") })
				if !closed {
					if rec.calls != calls || len(hub.shownIn) != before+1 || hub.shownIn[before] != owner {
						t.Fatalf("open chat: shown in %v (refusals %d), want %s", hub.shownIn[before:], rec.calls-calls, owner)
					}
					continue
				}
				if len(hub.shownIn) != before {
					t.Fatalf("closed chat: prompt shown in %v, want nowhere", hub.shownIn[before:])
				}
				if rec.calls != calls+1 || rec.sk != spawn || rec.req != id || rec.reason != tc.want {
					t.Errorf("closed chat: refusal sk=%q req=%q reason=%q (calls %d), want %s %s %q", rec.sk, rec.req, rec.reason, rec.calls-calls, spawn, id, tc.want)
				}
			}
		})
	}
}

// TestPrependSpawnOrigin_KnownSpawns proves a spawn's prompt is named by its
// spawn and any other session's is unchanged. (The direct spawn's batch
// session goes through tools.SpawnOfSession, proved in the tools package.)
func TestPrependSpawnOrigin_KnownSpawns(t *testing.T) {
	t.Parallel()
	const parent, bk = "main/c26842", "main/c26842/b1"
	tools.Spawns.Add(parent, tools.RunningSpawn{ID: "spawn-bg2684", BranchKey: bk, Description: "watch CI"}, func() {})
	t.Cleanup(func() { tools.Spawns.Stop(parent) })
	if got := prependSpawnOrigin(tools.Spawns, bk, "Allow?"); !strings.HasPrefix(got, "🐣 **From spawn spawn-bg2684:** watch CI") {
		t.Errorf("background spawn prompt = %q, want it named", got)
	}
	if got := prependSpawnOrigin(tools.Spawns, "main/c26842/b2", "Allow?"); got != "Allow?" {
		t.Errorf("a non-spawn session's prompt = %q, want it unchanged", got)
	}
}
