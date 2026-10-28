package tools

// Depth side of spawn ownership (#2624, spawn_reparent.go): which node owns a
// spawn a leaf started, and when a reported leaf's spawns may hand over.
//
// Handover point (Dick 2026-10-28): a reported node's spawns pass to its
// parent only once the parent has RECEIVED the report, i.e. its [DEPTH
// RESULTS] fan-in turn has run. Until then the spawn's messages are held. So
// nothing from a reparented spawn ever reaches a fanned parent mid-tree: it
// arrives after the fan-in, as the parent carries on.

import (
	"encoding/json"
	"fmt"
	"slices"
)

// depthMaxWalk bounds the walk up a tree (levels are far fewer; a corrupt
// record that loops must not hang a delivery).
const depthMaxWalk = 64

// SpawnRoute resolves the owner of the spawns session parent started: parent
// itself while it is open (or not a depth node at all); once it has reported
// (or is otherwise done) and its parent has received that through its fan-in,
// the parent, and so on up, node by node, to the root chat. A done node whose
// report has not reached its parent's fan-in yet holds them (Hold).
//
// While depth mode is off (#2628, Dick 2026-10-28) nothing is held: no fan-in
// can happen then, so a done node hands over at once and the result goes to
// the first open node up the tree, usually the root chat.
//
// Before the trees are restored (spawns restore first at startup, and depth
// mode may be off) the nodes are read from their persisted records.
func (m *DepthManager) SpawnRoute(parent string) SpawnRoute {
	off := !m.Enabled()
	m.mu.Lock()
	defer m.mu.Unlock()
	lookup := m.lookupLocked()

	r := SpawnRoute{Owner: parent}
	if start := lookup(parent); start != nil && start.LeafID != "" {
		r.Via = fmt.Sprintf("depth leaf %q (%s)", start.Title, parent)
	}
	for range depthMaxWalk {
		n := lookup(r.Owner)
		if n == nil || !n.done() || n.LeafID == "" {
			return r
		}
		up := n.Parent
		if up == "" {
			up = n.Tree
		}
		q := lookup(up)
		if q == nil {
			// The parent's record is gone (expired): the root chat takes it.
			if n.Tree == "" || n.Tree == n.Key {
				return r
			}
			r.Owner = n.Tree
			continue
		}
		if !off && !m.handedOverLocked(n, q) {
			r.Hold = true
			return r
		}
		r.Owner = q.Key
	}
	depthLog.Warnf("spawn owner walk from %s did not end after %d nodes; using %s", parent, depthMaxWalk, r.Owner)
	return r
}

// lookupLocked returns a node lookup by session key: the live nodes, or,
// before the trees are restored, the persisted records (read once per
// lookup). Caller holds m.mu while using it.
func (m *DepthManager) lookupLocked() func(key string) *depthNode {
	var stored map[string]*depthNode
	return func(key string) *depthNode {
		if n := m.nodes[key]; n != nil || m.restored {
			return n
		}
		if stored == nil {
			stored = m.storedNodes()
		}
		return stored[key]
	}
}

// SessionLive reports whether a prompt shown in sessionKey's chat can reach
// the user (#2627): false only for a depth leaf that has reported, been
// dismissed, lost or dropped, whose conversation the user has left. Any
// other session is live, including an open or fanned node, the root, and a
// side thread (a done leaf the user reopened and is talking to).
func (m *DepthManager) SessionLive(sessionKey string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.lookupLocked()(sessionKey)
	if n == nil || n.LeafID == "" {
		return true
	}
	return !n.done() || n.State == depthSide
}

// handedOverLocked reports whether done node n's report has reached parent
// q: q's current fan-out no longer includes n (q fanned in and fanned out
// again, or a reset cleared it), or q's fan-in turn for it has run. Caller
// holds m.mu.
func (m *DepthManager) handedOverLocked(n, q *depthNode) bool {
	if !slices.Contains(q.Children, n.Key) {
		return true
	}
	return q.State != depthFanned && q.Results == "" && !m.fanning[q.Key]
}

// storedNodes reads every persisted node record of the agent. Caller holds
// m.mu (the store takes none of it).
func (m *DepthManager) storedNodes() map[string]*depthNode {
	out := map[string]*depthNode{}
	if m.deps.Store == nil {
		return out
	}
	rows, err := m.deps.Store.AgentMetadataByPrefix(m.deps.AgentID, depthMetaPrefix)
	if err != nil {
		depthLog.Warnf("read depth nodes for a spawn's owner: %v", err)
		return out
	}
	for _, v := range rows {
		var n depthNode
		if err := json.Unmarshal([]byte(v), &n); err != nil || n.Key == "" {
			continue
		}
		out[n.Key] = &n
	}
	return out
}

// handover tells the spawner a handover point passed, so held spawn results
// whose owner may now take them are delivered.
func (m *DepthManager) handover() {
	if m.deps.OnHandover != nil {
		m.deps.OnHandover()
	}
}
