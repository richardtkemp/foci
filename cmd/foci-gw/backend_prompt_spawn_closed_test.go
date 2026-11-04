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
	tools.Spawns.Add(parent, tools.RunningSpawn{ID: "spawn-bg2684", BranchKey: bk, Name: "watch CI"}, func() {})
	t.Cleanup(func() { tools.Spawns.Stop(parent) })
	if got := spawnOrigin(tools.Spawns, bk); !strings.HasPrefix(got, "🐣 **From spawn spawn-bg2684:** watch CI") {
		t.Errorf("background spawn prompt = %q, want it named", got)
	}
	if got := spawnOrigin(tools.Spawns, "main/c26842/b2"); got != "" {
		t.Errorf("a non-spawn session's prompt = %q, want it unchanged", got)
	}
}

// agentRecordingHub is a fakeAppHub that records the agent each connection
// lookup is made for.
type agentRecordingHub struct {
	*fakeAppHub
	agents []string
}

func (h *agentRecordingHub) ForSessionOrPrimary(sessionKey, agentID string) platform.Connection {
	h.mu.Lock()
	h.agents = append(h.agents, agentID)
	h.mu.Unlock()
	return h.fakeAppHub.ForSessionOrPrimary(sessionKey, agentID)
}

// TestBackendPrompt_CrossAgentSpawnShownInCallersChat proves #2730 (Dick
// 2026-11-01: "for a spawn as another agent - I want their permission
// requests to be routed to parent too!"): a permission request from a spawn
// running as helen for a clutch session is shown in the caller's (clutch's)
// chat, looked up for clutch, headed with the agent and the spawn. When the
// caller's chat cannot show it, it is refused: it never falls back to helen's
// default chat. Fail-arm: before #2730 promptTargets ended the caller's chain
// on this agent's default session, so the second case showed it in helen/c9.
func TestBackendPrompt_CrossAgentSpawnShownInCallersChat(t *testing.T) {
	t.Parallel()
	const (
		spawn  = "helen/ixspawn/b2730"
		caller = "clutch/c2730"
	)
	tools.Spawns.Add(caller, tools.RunningSpawn{ID: "spawn-x2730", BranchKey: spawn, Name: "build it", Agent: "helen"}, func() {})
	t.Cleanup(func() { tools.Spawns.Stop(caller) })
	ownerOf := func(branchKey string) (string, bool) {
		if branchKey == spawn {
			return caller, true
		}
		return "", false
	}
	for _, callerLive := range []bool{true, false} {
		t.Run(fmt.Sprintf("callerLive=%v", callerLive), func(t *testing.T) {
			t.Parallel()
			hub := &agentRecordingHub{fakeAppHub: &fakeAppHub{bound: map[string]bool{caller: callerLive, "helen/c9": true}}}
			rec := &failRecorder{}
			approval, _ := backendPromptFuncs(backendPromptDeps{
				ag:             &agent.Agent{DelegatedManager: &agent.DelegatedManager{}},
				connMgr:        hub,
				agentID:        "helen",
				defaultSession: func() string { return "helen/c9" },
				failPrompt:     rec.fail,
				owner:          ownerOf,
			})
			id := fmt.Sprintf("req-2730-%v", callerLive)
			approval(spawn, id, "Allow Bash?", "Bash: go build", "", promptChoices())
			t.Cleanup(func() { platform.CancelInteractiveMessage(id, "done") })
			for _, a := range hub.agents {
				if a != "clutch" {
					t.Errorf("a connection was looked up for agent %q, want only the caller's (clutch): %v", a, hub.agents)
					break
				}
			}
			if !callerLive {
				if len(hub.shownIn) != 0 {
					t.Fatalf("caller's chat down: shown in %v, want nowhere (never helen's chats)", hub.shownIn)
				}
				if rec.calls != 1 || rec.sk != spawn || rec.req != id {
					t.Errorf("caller's chat down: refusal sk=%q req=%q (calls %d), want one for %s", rec.sk, rec.req, rec.calls, spawn)
				}
				return
			}
			if rec.calls != 0 || len(hub.shownIn) != 1 || hub.shownIn[0] != caller {
				t.Fatalf("shown in %v (refusals %d), want only the caller's chat %s", hub.shownIn, rec.calls, caller)
			}
			if want := "🐣 **From helen's spawn spawn-x2730:** build it"; !strings.HasPrefix(hub.texts[0], want) {
				t.Errorf("prompt text = %q, want it to start %q", hub.texts[0], want)
			}
		})
	}
}
