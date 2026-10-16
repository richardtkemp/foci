package main

import (
	"testing"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/config"
)

// TestRegisterAgentCommands_StopAliasIsImmediate pins that a stop alias
// (default "wait") is Immediate like /stop. A non-immediate alias is deferred
// behind the running turn, so it ran only once the turn had ended and stopped
// nothing (Dick 2026-10-16: "If /wait stops the turn then it needs to
// interrupt in order to do so!").
func TestRegisterAgentCommands_StopAliasIsImmediate(t *testing.T) {
	t.Parallel()
	resolved := &config.ResolvedAgentConfig{}
	resolved.Behavior.EnableStopAliases = true
	resolved.Behavior.StopAliases = []string{"stop", "wait"}
	p := cmdRegParams{
		ag:           &agent.Agent{},
		acfg:         config.AgentConfig{ID: "a"},
		cfg:          &config.Config{},
		resolved:     resolved,
		resolvedLive: config.NewLiveValue(resolved),
	}
	cmds, _ := registerAgentCommands(p, command.NewLastMessageStore())
	stop, wait := cmds.Get("stop"), cmds.Get("wait")
	if stop == nil || wait == nil {
		t.Fatalf("stop registered = %v, wait registered = %v; want both", stop != nil, wait != nil)
	}
	if !stop.Immediate {
		t.Fatalf("/stop is not Immediate; this test assumes it is")
	}
	if !wait.Immediate {
		t.Errorf("/wait (stop alias) Immediate = false, want true like /stop")
	}
}
