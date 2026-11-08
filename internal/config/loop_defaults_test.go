package config

import "testing"

// TestMaxToolLoops_DefaultWhenUnset is #2977: with [agent_loop]
// max_tool_loops absent from foci.toml, the resolved budget is the documented
// 100, not 0. A 0 made every API one-shot spawn return "Max tool call depth
// reached." before its first model call. Covers the production path (Load)
// and a config built without Load, and the explore budget beside it.
func TestMaxToolLoops_DefaultWhenUnset(t *testing.T) {
	cfg, err := loadTOML(t, "[[agents]]\nid = \"a\"\n")
	if err != nil {
		t.Fatal(err)
	}
	rc := Resolve(cfg, cfg.Agents[0])
	if got := rc.Loop.MaxToolLoops; got != DefaultMaxToolLoops {
		t.Errorf("Load, key unset: Loop.MaxToolLoops = %d, want %d", got, DefaultMaxToolLoops)
	}
	if got := rc.Tools.ExploreMaxDepth; got != DefaultExploreMaxDepth {
		t.Errorf("Load, key unset: Tools.ExploreMaxDepth = %d, want %d", got, DefaultExploreMaxDepth)
	}

	bare := Resolve(&Config{}, AgentConfig{})
	if got := bare.Loop.MaxToolLoops; got != DefaultMaxToolLoops {
		t.Errorf("no Load: Loop.MaxToolLoops = %d, want %d", got, DefaultMaxToolLoops)
	}
	if got := bare.Tools.ExploreMaxDepth; got != DefaultExploreMaxDepth {
		t.Errorf("no Load: Tools.ExploreMaxDepth = %d, want %d", got, DefaultExploreMaxDepth)
	}

	// An explicit 0 means the default too, as the API turn loop has always
	// read it; a positive value is kept.
	zero := Resolve(&Config{AgentLoop: AgentLoopConfig{MaxToolLoops: Ptr(0)}}, AgentConfig{})
	if got := zero.Loop.MaxToolLoops; got != DefaultMaxToolLoops {
		t.Errorf("explicit 0: Loop.MaxToolLoops = %d, want %d", got, DefaultMaxToolLoops)
	}
	set := Resolve(&Config{AgentLoop: AgentLoopConfig{MaxToolLoops: Ptr(7)}}, AgentConfig{})
	if got := set.Loop.MaxToolLoops; got != 7 {
		t.Errorf("explicit 7: Loop.MaxToolLoops = %d, want 7", got)
	}
}
