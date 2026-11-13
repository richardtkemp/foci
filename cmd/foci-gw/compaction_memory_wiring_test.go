package main

import (
	"testing"

	"foci/internal/agent"
	"foci/internal/config"
)

// TestSetup_DelegatedAgentsGetCompactionMemoryHook is the #3088 regression.
// The pre-compaction reflection hook was registered in configureAPI only, so
// from March to November 2026 no delegated agent (claude-code, codex,
// opencode) ever ran it: the compaction paths looped over an empty hook list
// and logged nothing. This drives the setup sequence setupAgent runs for a
// delegated agent (configureUniversal, then configureDelegated) and requires
// exactly one hook, for each delegated backend.
func TestSetup_DelegatedAgentsGetCompactionMemoryHook(t *testing.T) {
	for _, backend := range []string{"claude-code", "codex", "opencode"} {
		t.Run(backend, func(t *testing.T) {
			p, shared := wiringFixture(t)
			ag := &agent.Agent{}
			configureUniversal(ag, p, nil)
			if _, ok := configureDelegated(ag, p, shared, backend, config.BackendConfig{}); !ok {
				t.Fatalf("configureDelegated(%s) returned ok=false", backend)
			}
			if n := len(ag.CompactionMemoryFunc); n != 1 {
				t.Fatalf("%s agent has %d pre-compaction reflection hooks, want 1", backend, n)
			}
		})
	}
}
