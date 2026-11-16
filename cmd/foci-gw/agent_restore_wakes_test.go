package main

import (
	"testing"

	"foci/internal/agent"
)

// TestAgentRestorePhases_RearmsWakes is #3131: the stored foci_remind wakes
// are re-armed by a restore phase (after plat.StartAll restores the app chat
// bindings), not during the agent's build. An overdue wake fired during the
// build ran before the bindings existed, so its turn got a sink that showed
// the app no activity and no streamed text (Stuart, 16 Nov 00:15-00:25).
func TestAgentRestorePhases_RearmsWakes(t *testing.T) {
	var order []string
	ag := &agent.Agent{
		DepthRestore: func() { order = append(order, "depth") },
		WakeRestore:  func() { order = append(order, "wakes") },
	}
	inst := &agentInstance{id: "test", ag: ag}
	for _, phase := range agentRestorePhases {
		phase(inst)
	}
	if len(order) != 2 || order[0] != "depth" || order[1] != "wakes" {
		t.Fatalf("restore phases ran %q, want the wakes re-armed once, after the depth restore", order)
	}
}
