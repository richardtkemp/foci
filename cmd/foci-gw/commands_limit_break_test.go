package main

import "testing"

// TestRegisterAgentCommands_LimitBreak (#3128): /limit_break and its /lb
// alias exist on the backends whose account a usage hold can hold (Claude
// Code, codex), not on opencode or an API agent.
func TestRegisterAgentCommands_LimitBreak(t *testing.T) {
	t.Parallel()
	for backend, want := range map[string]bool{"claude-code": true, "codex": true, "opencode": false, "": false} {
		reg := registerForBackend(backend, "a")
		cmd, lb := reg.Get("limit_break"), reg.Get("lb")
		if (cmd != nil) != want || (lb != nil) != want || cmd != lb {
			t.Errorf("backend %q: limit_break=%v lb=%v, want registered=%v (one command)", backend, cmd != nil, lb != nil, want)
		}
	}
}
