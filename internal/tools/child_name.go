package tools

import "strings"

// ChildName names child when it is a child session of parent, for the app's
// chit label: a message from one of a session's own children reads
// "from <child name>", not the child's agent and session key (#2836, Dick
// 2026-11-03). It returns "" when child is not a child of parent, or is one
// with no name:
//   - a running background spawn of parent (its branch is child): the
//     spawn's name, or its id for a spawn from before names existed (#2721);
//   - a depth-mode leaf whose parent node is parent: the leaf's title, or
//     its leaf id when the title is blank;
//   - any other child (a plain branch): "" — it has no name, so the chit
//     keeps the agent and session label.
func ChildName(parent, child string) string {
	if parent == "" || child == "" || parent == child {
		return ""
	}
	if name := spawnChildName(parent, child); name != "" {
		return name
	}
	return depthChildName(parent, child)
}

// spawnChildName is the name of the running background spawn of parent whose
// branch session is child, or "". A cross-agent spawn is tracked by the
// caller's Spawner, so every registered Spawner's tracker is asked.
func spawnChildName(parent, child string) string {
	for _, id := range spawnAgentIDs() {
		s := spawnerFor(id)
		if s == nil {
			continue
		}
		p, info, ok := s.deps.Tracker.ForBranch(child)
		if !ok || p != parent {
			continue
		}
		if n := strings.TrimSpace(info.Name); n != "" {
			return n
		}
		return info.ID
	}
	return ""
}

// depthChildName is the title of the depth-mode node child when its parent
// node is parent, or "".
func depthChildName(parent, child string) string {
	depthMgrMu.RLock()
	m := depthMgrs[agentIDOf(child)]
	depthMgrMu.RUnlock()
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.nodes[child]
	if n == nil || n.Parent != parent {
		return ""
	}
	if t := strings.TrimSpace(n.Title); t != "" {
		return t
	}
	return n.LeafID
}
