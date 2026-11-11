package main

import (
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/log"
	"foci/internal/warnings"
)

// TestPushLogWarning_ReloginOnlyToCCLoginAgents (#3014): the log warning hook
// pushes every WARN/ERROR to every agent's queues. An agent that does not use
// the Claude Code login (codex, opencode, API) is not held by it and does not
// drop its warnings on success, so it must never receive the login's own
// warnings ("relogin", "relogin/<agent>"): it cannot act on them. An agent on
// the CC login still receives them (held, then dropped on success), and every
// agent still receives other components' warnings.
func TestPushLogWarning_ReloginOnlyToCCLoginAgents(t *testing.T) {
	newAgent := func(cc bool) *agent.Agent {
		a := &agent.Agent{
			DelegatedManager: &agent.DelegatedManager{},
			WarningQueue:     warnings.NewQueue(100, time.Hour),
			ChatWarningQueue: warnings.NewQueue(100, time.Hour),
		}
		if cc {
			a.ReloginTrigger = func(string, string) bool { return true }
		}
		return a
	}
	cc, other := newAgent(true), newAgent(false)

	for _, comp := range []string{"relogin", "relogin/clutch"} {
		for _, a := range []*agent.Agent{cc, other} {
			pushLogWarning(a, log.ERROR, comp, "CC re-login aborted: "+comp)
		}
	}
	if n := other.Warnings().Len(); n != 0 {
		t.Errorf("non-CC agent: %d relogin warnings queued, want 0: %v", n, other.Warnings().Drain())
	}
	if n := other.ChatWarnings().Len(); n != 0 {
		t.Errorf("non-CC agent: %d relogin chat warnings queued, want 0", n)
	}
	if n := cc.Warnings().Len(); n != 2 {
		t.Errorf("CC agent: %d relogin warnings queued, want 2", n)
	}
	if n := cc.ChatWarnings().Len(); n != 2 {
		t.Errorf("CC agent: %d relogin chat warnings queued, want 2", n)
	}

	// Other components reach every agent; a name that only starts with
	// "relogin" is not the login's.
	for _, comp := range []string{"agent", "reloginx"} {
		pushLogWarning(other, log.WARN, comp, "something else: "+comp)
	}
	if n := other.Warnings().Len(); n != 2 {
		t.Errorf("non-CC agent: %d other warnings queued, want 2", n)
	}
	if n := other.ChatWarnings().Len(); n != 2 {
		t.Errorf("non-CC agent: %d other chat warnings queued, want 2", n)
	}
}
