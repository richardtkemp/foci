package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type memMeta map[string]map[string]string // agent → key → value

func (m memMeta) AgentMetadataByPrefix(agentID, prefix string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range m[agentID] {
		if strings.HasPrefix(k, prefix) {
			out[k] = v
		}
	}
	return out, nil
}

func (m memMeta) put(t *testing.T, agentID string, n depthNode) {
	t.Helper()
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	if m[agentID] == nil {
		m[agentID] = map[string]string{}
	}
	m[agentID][depthMetaPrefix+n.Key] = string(b)
}

// TestDepthHolds is #3134's ruling (Dick, 2026-11-16): "Don't gzip or delete
// leaves while their depth mode is still running, up to 14 days (then gzip)".
func TestDepthHolds(t *testing.T) {
	m := memMeta{}
	root := "alpha/c1"
	// The root's current tree is T2; it has not taken T2's [DEPTH RESULTS]
	// yet, so T2 is still running though its leaves are all done.
	m.put(t, "alpha", depthNode{Key: root, Tree: root, TreeID: "T2", State: depthActive, Results: "[DEPTH RESULTS] ..."})
	m.put(t, "alpha", depthNode{Key: "alpha/c1/b5", LeafID: "L5", Tree: root, TreeID: "T2", State: depthReported})
	// Tree T1 is running: one leaf still active, one reported.
	m.put(t, "alpha", depthNode{Key: "alpha/c1/b1", LeafID: "L1", Tree: root, TreeID: "T1", State: depthActive})
	m.put(t, "alpha", depthNode{Key: "alpha/c1/b2", LeafID: "L2", Tree: root, TreeID: "T1", State: depthReported})
	// Tree T0 finished: every leaf done, no results pending.
	m.put(t, "alpha", depthNode{Key: "alpha/c1/b3", LeafID: "L3", Tree: root, TreeID: "T0", State: depthReported})
	m.put(t, "alpha", depthNode{Key: "alpha/c1/b4", LeafID: "L4", Tree: root, TreeID: "T0", State: depthDismissed})

	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	h := NewDepthHolds(m)
	h.now = func() time.Time { return now }

	day := 24 * time.Hour
	cases := []struct {
		key          string
		idle         time.Duration
		delete, gzip bool
	}{
		{"alpha/c1/b1", 2 * day, true, true},   // running, young
		{"alpha/c1/b2", 2 * day, true, true},   // done leaf of a running tree
		{"alpha/c1/b1", 15 * day, true, false}, // running, past 14 days: gzip, never delete
		{"alpha/c1/b3", 2 * day, false, false}, // finished tree
		{"alpha/c1/b5", 2 * day, true, true},   // results pending on the root
		{root, time.Hour, false, false},        // the root chat is no leaf
		{"alpha/c9/b1", time.Hour, false, false},
		{"beta/c1/b1", time.Hour, false, false},
	}
	for _, c := range cases {
		if got := h.HoldsDelete(c.key); got != c.delete {
			t.Errorf("HoldsDelete(%s) = %v, want %v", c.key, got, c.delete)
		}
		if got := h.HoldsGzip(c.key, now.Add(-c.idle)); got != c.gzip {
			t.Errorf("HoldsGzip(%s, idle %v) = %v, want %v", c.key, c.idle, got, c.gzip)
		}
	}
	var nilHolds *DepthHolds
	if nilHolds.HoldsDelete("alpha/c1/b1") || NewDepthHolds(nil).HoldsDelete("alpha/c1/b1") {
		t.Error("a nil DepthHolds or store must hold nothing")
	}
}
