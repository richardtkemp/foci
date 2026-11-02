package tools

import "testing"

// #2787: a run cut short after BeginAgentStop(agent) is treated like one cut by
// a shutdown — interrupted, its record kept for the agent's next start — and a
// run of another agent is not; ForgetAgent drops only that agent's entries.
func TestSpawnTracker_AgentStop(t *testing.T) {
	tr := NewSpawnTracker()
	tr.mu.Lock()
	tr.spawns = map[string]map[string]*trackedSpawn{
		"bob/c1":   {"s1": {running: true}},
		"carol/c1": {"s2": {running: true}},
	}
	tr.mu.Unlock()
	tr.BeginAgentStop("bob")
	if _, done, _, interrupted, _ := tr.finishOrNext("bob/c1", "s1", true); !done || !interrupted {
		t.Errorf("bob's cut-short run: done=%v interrupted=%v, want both", done, interrupted)
	}
	if _, _, _, interrupted, _ := tr.finishOrNext("carol/c1", "s2", true); interrupted {
		t.Error("carol's run was treated as cut by bob's stop")
	}
	tr.ForgetAgent("bob")
	tr.mu.Lock()
	_, bob := tr.spawns["bob/c1"]
	_, carol := tr.spawns["carol/c1"]
	stopping := tr.stopping["bob"]
	tr.mu.Unlock()
	if bob || !carol || stopping {
		t.Errorf("after ForgetAgent: bob entries %v, carol entries %v, bob stopping %v", bob, carol, stopping)
	}
}
