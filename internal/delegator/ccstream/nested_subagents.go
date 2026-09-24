package ccstream

// Nested subagents (#1554).
//
// A subagent can itself call the Agent tool, spawning a grandchild
// (spawnDepth >= 2). Only depth-1 subagents get a chit in the main chat: a
// grandchild is part of its spawner's work. Before this, a nested spawn's
// PreToolUse was dropped by the hook sidechain filter (so no start), while its
// task_notification still fired OnSubagentEnd (so an end with no start, and a
// spurious peer chit).
//
// The constraint, re-verified live on CC 2.1.280 (verify-cc-stream-hooks,
// nested_probe.sh / nested_bg_text_probe.sh): task_started / task_progress /
// task_notification carry NO parentage, so a grandchild's lifecycle events are
// shaped exactly like a child's. Depth is knowable only at SPAWN, from:
//
//  1. the native stream (primary): the nested Agent tool_use block reaches the
//     parent stdout tagged parent_tool_use_id = the spawner's groupKey, and
//     always precedes the nested task_started;
//  2. the PreToolUse hook (second source, like #1425): a nested Agent call's
//     agent_id is the SPAWNER's task_id (a depth-1 call has none);
//  3. after a foci restart, when neither was seen: CC's agent-<task_id>.meta.json
//     sidecar, which records spawnDepth and parentAgentId.
//
// Scope: this fixes foci's rendering and bookkeeping only. Where CC delivers a
// grandchild's completion notification (observed 2026-09-22: to the top-level
// session rather than to the spawning subagent) is CC's own routing, outside
// anything foci sees or controls, and is NOT changed here.

// registerNestedAgent records that nestedID is an Agent spawn made BY a subagent
// whose group is parentGroupKey. Idempotent. The two spawn signals race, and the
// hook's parent can be unresolved (""), so a resolved parent upgrades an
// unresolved one but never the reverse.
func (b *Backend) registerNestedAgent(nestedID, parentGroupKey string) {
	if nestedID == "" || nestedID == parentGroupKey {
		return
	}
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	b.registerNestedAgentLocked(nestedID, parentGroupKey)
}

func (b *Backend) registerNestedAgentLocked(nestedID, parentGroupKey string) {
	if b.nestedAgents == nil {
		b.nestedAgents = map[string]string{}
	}
	if existing, dup := b.nestedAgents[nestedID]; dup && (existing != "" || parentGroupKey == "") {
		return
	}
	b.nestedAgents[nestedID] = parentGroupKey
	b.logger().Infof("subagent_nested_registered group=%s parent=%s", nestedID, parentGroupKey)
}

// topLevelAncestor resolves groupKey to the depth-1 group it belongs to, walking
// the chain so depth 3+ collapses too. nested=false means groupKey is already
// top-level. nested=true with ancestor=="" means nested but unattributable. The
// iteration cap guards against a cycle built from malformed stream data.
func (b *Backend) topLevelAncestor(groupKey string) (ancestor string, nested bool) {
	if groupKey == "" {
		return "", false
	}
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	cur := groupKey
	for range 16 {
		parent, ok := b.nestedAgents[cur]
		if !ok {
			return cur, cur != groupKey
		}
		if parent == "" {
			return "", true
		}
		cur = parent
	}
	b.logger().Warnf("topLevelAncestor: nested chain from %s exceeded depth cap; treating as unattributable", groupKey)
	return "", true
}

// groupKeyForTask maps a subagent's task_id (which is what a hook's agent_id
// carries) to its group key: a depth-1 run's groupKey, or a nested run's own
// Agent tool_use id. "" when the task is unknown.
func (b *Backend) groupKeyForTask(taskID string) string {
	if taskID == "" {
		return ""
	}
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	if run := b.subagentRuns[taskID]; run != nil {
		return run.groupKey
	}
	return b.nestedTasks[taskID]
}

// nestedTask reports whether a task_* event (identified by task_id and the
// event's tool_use_id) belongs to a nested subagent, returning that subagent's
// own group key. It remembers the task_id on first recognition, so a later
// SendMessage resume of the same grandchild (whose events carry the
// SendMessage's tool_use id, not the Agent's) is still recognised.
//
// A task already bound as a depth-1 run, or whose tool_use id has a depth-1
// Agent stash, is never nested and costs no disk read. Otherwise the on-disk
// meta sidecar is the last resort (post-restart: the maps are in-memory only).
// That read also covers an unknown background Bash task, whose sidecar simply
// does not exist.
func (b *Backend) nestedTask(taskID, toolUseID string) (groupKey string, nested bool) {
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	if key, ok := b.nestedTasks[taskID]; ok && taskID != "" {
		return key, true
	}
	if _, ok := b.nestedAgents[toolUseID]; ok && toolUseID != "" {
		b.rememberNestedTaskLocked(taskID, toolUseID)
		return toolUseID, true
	}
	if taskID == "" || b.subagentRuns[taskID] != nil {
		return "", false
	}
	if _, depth1 := b.agentLabels[toolUseID]; depth1 {
		return "", false
	}
	meta, ok := b.readSubagentMeta(taskID)
	if !ok || meta.SpawnDepth < 2 {
		return "", false
	}
	// Rebuild the ancestry so a resumed grandchild's text (which keeps its
	// original Agent id as parent_tool_use_id) re-attributes to its spawner's
	// chit instead of opening an orphan one.
	parentKey := ""
	if run := b.subagentRuns[meta.ParentAgentID]; run != nil {
		parentKey = run.groupKey
	} else if key, ok := b.nestedTasks[meta.ParentAgentID]; ok {
		parentKey = key
	} else if pm, ok := b.readSubagentMeta(meta.ParentAgentID); ok {
		parentKey = pm.ToolUseID
	}
	b.registerNestedAgentLocked(meta.ToolUseID, parentKey)
	b.rememberNestedTaskLocked(taskID, meta.ToolUseID)
	return meta.ToolUseID, true
}

func (b *Backend) rememberNestedTaskLocked(taskID, groupKey string) {
	if taskID == "" {
		return
	}
	if b.nestedTasks == nil {
		b.nestedTasks = map[string]string{}
	}
	b.nestedTasks[taskID] = groupKey
}
