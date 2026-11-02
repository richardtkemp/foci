package tools

import (
	"strings"
	"testing"
	"time"
)

// cacheSpawnAgent is a scriptedSpawnAgent that reports a prompt-cache expiry
// for every session, as *agent.Agent does (SpawnCacheReporter).
type cacheSpawnAgent struct {
	*scriptedSpawnAgent
	expiry func(now time.Time) time.Time
}

func (a *cacheSpawnAgent) CacheExpiry(_ string, at time.Time) time.Time { return a.expiry(at) }

// TestSpawnSend_ColdFinishedSpawnIsNotWoken is Dick's 2026-11-02 rule (#2491):
// spawn_send to a finished spawn whose cache is cold starts no turn, leaves
// the spawn as it was, and tells the caller the message was not delivered.
func TestSpawnSend_ColdFinishedSpawnIsNotWoken(t *testing.T) {
	t.Parallel()
	for name, expiry := range map[string]func(now time.Time) time.Time{
		"expired an hour ago": func(now time.Time) time.Time { return now.Add(-time.Hour) },
		"no cache recorded":   func(time.Time) time.Time { return time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			agent := &cacheSpawnAgent{scriptedSpawnAgent: newScriptedSpawnAgent(0), expiry: expiry}
			h := newFollowupHarness(agent)
			id := h.start(t, "find the bug")
			waitStarted(t, agent.scriptedSpawnAgent, 0)
			h.result(t)

			got := h.send(t, id, "now fix it")
			if !strings.Contains(got, "NOT delivered") || !strings.Contains(got, "cold") || !strings.Contains(got, "new spawn") {
				t.Errorf("send to a cold finished spawn = %q, want a not-delivered result naming the cold cache and a new spawn", got)
			}
			time.Sleep(100 * time.Millisecond)
			if calls, _ := agent.turns(); len(calls) != 1 {
				t.Errorf("turns = %d, want 1 (the cold spawn must not be woken)", len(calls))
			}
			if st := h.status(t, id); !strings.Contains(st, ": completed") || strings.Contains(st, "now fix it") {
				t.Errorf("spawn status after a refused send = %q, want it still completed with no logged message", st)
			}
		})
	}
}

// TestSpawnSend_WarmFinishedSpawnStillResumes: the rule refuses only a cold
// cache; a warm finished spawn resumes as before.
func TestSpawnSend_WarmFinishedSpawnStillResumes(t *testing.T) {
	t.Parallel()
	agent := &cacheSpawnAgent{scriptedSpawnAgent: newScriptedSpawnAgent(0),
		expiry: func(now time.Time) time.Time { return now.Add(50 * time.Minute) }}
	h := newFollowupHarness(agent)
	id := h.start(t, "find the bug")
	waitStarted(t, agent.scriptedSpawnAgent, 0)
	h.result(t)
	if got := h.send(t, id, "now fix it"); !strings.Contains(got, "resumed") {
		t.Fatalf("send to a warm finished spawn = %q, want it resumed", got)
	}
	waitStarted(t, agent.scriptedSpawnAgent, 1)
}
