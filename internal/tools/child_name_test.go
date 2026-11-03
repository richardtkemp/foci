package tools

import "testing"

// #2836: a message from one of a session's own children names the child.

func TestChildNameSpawn(t *testing.T) {
	t.Parallel()
	tracker := NewSpawnTracker()
	s := NewSpawner(SpawnDeps{AgentID: "cnspawn", Tracker: tracker}, nil)
	RegisterSpawner("cnspawn", s)
	t.Cleanup(func() { UnregisterSpawner("cnspawn") })
	tracker.Add("cnspawn/c1", RunningSpawn{ID: "spawn-a", BranchKey: "cnspawn/c1/b1", Name: " todo-9 "}, func() {})
	tracker.Add("cnspawn/c1", RunningSpawn{ID: "spawn-b", BranchKey: "cnspawn/c1/b2"}, func() {})

	for _, c := range []struct{ parent, child, want string }{
		{"cnspawn/c1", "cnspawn/c1/b1", "todo-9"},  // named spawn, trimmed
		{"cnspawn/c1", "cnspawn/c1/b2", "spawn-b"}, // pre-#2721 spawn: its id
		{"cnspawn/c9", "cnspawn/c1/b1", ""},        // not this session's child
		{"cnspawn/c1", "cnspawn/c1/b3", ""},        // a plain branch: no name
		{"cnspawn/c1", "cnspawn/c1", ""},           // itself
	} {
		if got := ChildName(c.parent, c.child); got != c.want {
			t.Errorf("ChildName(%q, %q) = %q, want %q", c.parent, c.child, got, c.want)
		}
	}
}

func TestChildNameDepthLeaf(t *testing.T) {
	t.Parallel()
	m := NewDepthManager(DepthDeps{AgentID: "cndepth"})
	t.Cleanup(func() { UnregisterDepthManager("cndepth") })
	m.mu.Lock()
	m.nodes["cndepth/c2"] = &depthNode{Key: "cndepth/c2", LeafID: "l2", Parent: "cndepth/c1", Title: "Review the cache"}
	m.nodes["cndepth/c3"] = &depthNode{Key: "cndepth/c3", LeafID: "l3", Parent: "cndepth/c1", Title: "  "}
	m.nodes["cndepth/c4"] = &depthNode{Key: "cndepth/c4", LeafID: "l4", Parent: "cndepth/c2", Title: "Nested"}
	m.mu.Unlock()

	for _, c := range []struct{ parent, child, want string }{
		{"cndepth/c1", "cndepth/c2", "Review the cache"},
		{"cndepth/c1", "cndepth/c3", "l3"}, // blank title: the leaf id
		{"cndepth/c2", "cndepth/c4", "Nested"},
		{"cndepth/c1", "cndepth/c4", ""}, // a grandchild is not a child
		{"cndepth/c1", "cndepth/c9", ""},
	} {
		if got := ChildName(c.parent, c.child); got != c.want {
			t.Errorf("ChildName(%q, %q) = %q, want %q", c.parent, c.child, got, c.want)
		}
	}
}
