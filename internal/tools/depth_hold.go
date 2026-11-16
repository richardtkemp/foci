package tools

import (
	"encoding/json"
	"time"

	"foci/internal/session"
)

// DepthTranscriptHoldMax is how long, idle, a leaf of a running depth tree
// keeps its backend transcript uncompressed (#3134). Ruling (Dick,
// 2026-11-16): "Don't gzip or delete leaves while their depth mode is still
// running, up to 14 days (then gzip)".
const DepthTranscriptHoldMax = 14 * 24 * time.Hour

// DepthMetadataReader is the part of the session index DepthHolds reads:
// the persisted depth:<key> node records.
type DepthMetadataReader interface {
	AgentMetadataByPrefix(agentID, prefix string) (map[string]string, error)
}

// DepthHolds answers, for the session sweeps, whether depth mode holds a
// session's backend transcript (#3134). It reads the persisted node records,
// not a DepthManager's memory, so it gives the same answer before a
// restart's DepthRestore has run and while [depth] enabled is off. Build one
// per sweep: each agent's records are read once, on first use.
type DepthHolds struct {
	store DepthMetadataReader
	now   func() time.Time
	// running is, per agent, the leaf keys of that agent's running trees.
	running map[string]map[string]bool
}

// NewDepthHolds returns a DepthHolds over store. A nil store holds nothing.
func NewDepthHolds(store DepthMetadataReader) *DepthHolds {
	return &DepthHolds{store: store, now: time.Now, running: map[string]map[string]bool{}}
}

// HoldsDelete reports whether sessionKey is a leaf of a depth tree that is
// still running: its transcript must not be deleted, however long idle.
func (h *DepthHolds) HoldsDelete(sessionKey string) bool {
	if h == nil || h.store == nil {
		return false
	}
	agentID := session.AgentIDFromAnyKey(sessionKey)
	leaves, ok := h.running[agentID]
	if !ok {
		leaves = runningDepthLeaves(h.store, agentID)
		h.running[agentID] = leaves
	}
	return leaves[sessionKey]
}

// HoldsGzip reports whether sessionKey's transcript must stay uncompressed:
// it is a leaf of a running depth tree, idle (since lastActivity) for less
// than DepthTranscriptHoldMax.
func (h *DepthHolds) HoldsGzip(sessionKey string, lastActivity time.Time) bool {
	if !h.HoldsDelete(sessionKey) {
		return false
	}
	return h.now().Sub(lastActivity) < DepthTranscriptHoldMax
}

// runningDepthLeaves returns the session keys of every leaf (any node with a
// leaf id, nested ones included) of agentID's depth trees that are still
// running. A tree (one root conversation's TreeID, #2614) is running while
// any of its leaves is not done (active or fanned out) or any of its nodes
// holds a [DEPTH RESULTS] message not yet taken. Done leaves of a running
// tree are held too: the user can still open them as side threads.
// A read error holds nothing, logged: the sweeps then act as before #3134.
func runningDepthLeaves(store DepthMetadataReader, agentID string) map[string]bool {
	rows, err := store.AgentMetadataByPrefix(agentID, depthMetaPrefix)
	if err != nil {
		depthLog.Warnf("depth holds: load %s's depth nodes: %v", agentID, err)
		return nil
	}
	type treeRef struct{ tree, id string }
	running := map[treeRef]bool{}
	var leaves []depthNode
	for _, v := range rows {
		var n depthNode
		if err := json.Unmarshal([]byte(v), &n); err != nil || n.Key == "" {
			continue
		}
		ref := treeRef{n.Tree, n.TreeID}
		if n.LeafID == "" {
			// A root's record carries its CURRENT tree's id; its pending
			// results belong to that tree.
			if n.Results != "" {
				running[ref] = true
			}
			continue
		}
		leaves = append(leaves, n)
		if !n.done() || n.Results != "" {
			running[ref] = true
		}
	}
	out := map[string]bool{}
	for _, n := range leaves {
		if running[treeRef{n.Tree, n.TreeID}] {
			out[n.Key] = true
		}
	}
	return out
}
