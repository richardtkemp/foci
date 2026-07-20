package ccstream

// Subagent reactivation tracking (#1355).
//
// A subagent can run more than once: the initial `Agent` tool spawn, then any
// number of `SendMessage` resumes of the same subagent. The problem this solves:
// foci keyed subagent lifecycle on the `tool_use_id`, but that CHANGES on every
// resume (run 2's task_started/task_notification carry the SendMessage block's id,
// not the Agent's), while the running-subagent tracker only ever `Add`s on an
// `Agent` tool_use block. So a resumed subagent never re-registered — the activity
// chip stayed dead and the app's run group showed "completed" though work went on.
//
// The fix keys on the STABLE identity: the `task_id`, which is identical across all
// runs of one subagent (verified live 2026-07-19). The group key the app collapses
// every run's text under stays the ORIGINAL Agent `tool_use_id` — which is also what
// the subagent's text keeps as its `parent_tool_use_id` across resumes, so all runs
// share one continuous view.
//
// #1419 adds a second SendMessage case #1355 didn't cover: messaging a subagent
// that is STILL RUNNING (task_started seen, no task_notification:completed yet).
// Live probing (verify-cc-stream-hooks skill, 2026-07-20) showed CC never refires
// task_started for this case — the message is folded into the live run with no
// stream signal at all — so the #1355 stash-for-the-next-task_started mechanism
// never fires and the follow-up is silently dropped. The `active` flag distinguishes
// the two cases: SendMessage to an ACTIVE run surfaces immediately via
// OnSubagentPrompt at the run's CURRENT index (no new chit, no new run); SendMessage
// to an inactive (ended) run keeps stashing for the eventual reactivation's
// task_started, as before.

// subagentRun is the per-subagent (per task_id) reactivation state.
type subagentRun struct {
	groupKey      string // original Agent tool_use_id; stable across reactivations
	label         string // agent description, reused for every run's chit
	runIndex      int    // 1 for the initial spawn, bumped on each reactivation
	pendingPrompt string // next reactivation's prompt (from a SendMessage block), consumed at the resumed task_started
	active        bool   // true from task_started until this run's task_notification:completed (#1419)
}

// setAgentLabel stashes an Agent block's description by its tool_use_id (= groupKey)
// so the first task_started for the resulting subagent can bind the label.
func (b *Backend) setAgentLabel(groupKey, label string) {
	if groupKey == "" {
		return
	}
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	if b.agentLabels == nil {
		b.agentLabels = map[string]string{}
	}
	b.agentLabels[groupKey] = label
}

// setAgentPrompt stashes an Agent block's prompt by its tool_use_id (= groupKey),
// mirroring setAgentLabel. The PreToolUse hook path reads the prompt straight off
// its own hook payload (hooks.go), but the task_started fallback (#1425) has no
// such payload — TaskEvent carries no prompt field — so it reads this stash
// instead, giving a hook-missed run 1 the same SubagentStart content a working
// hook would have produced.
func (b *Backend) setAgentPrompt(groupKey, prompt string) {
	if groupKey == "" {
		return
	}
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	if b.agentPrompts == nil {
		b.agentPrompts = map[string]string{}
	}
	b.agentPrompts[groupKey] = prompt
}

// markSubagentStarted check-and-sets groupKey as having had its run-1
// SubagentStart emitted. Two independent sources race to emit that start —
// the PreToolUse hook (hooks.go) and the task_started fallback (#1425, below)
// — and this is the single dedup point both call through: whichever arrives
// first claims the groupKey and returns false (caller should emit); the
// other finds it already claimed and returns true (caller must skip —
// emitting would double the chit). Safe for concurrent use; nil/empty
// groupKey is a no-op that always reports "already started" so a caller
// with no group key never emits.
func (b *Backend) markSubagentStarted(groupKey string) (alreadyStarted bool) {
	if groupKey == "" {
		return true
	}
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	if b.subagentStarted == nil {
		b.subagentStarted = map[string]bool{}
	}
	if b.subagentStarted[groupKey] {
		return true
	}
	b.subagentStarted[groupKey] = true
	return false
}

// stashResumePrompt records the message a SendMessage block sent to a subagent
// (keyed by its task_id == the SendMessage `to`), to be surfaced as the prompt on
// the reactivation's SubagentStart. When the task_id isn't tracked in memory it
// tries to rehydrate the run from CC's on-disk meta.json first — the #1433
// post-restart case, where a SendMessage follow-up targets a subagent this backend
// never saw spawn. Still a no-op if rehydration also fails (the SendMessage
// targeted something that isn't a subagent we can identify).
func (b *Backend) stashResumePrompt(taskID, prompt string) {
	if taskID == "" {
		return
	}
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	run := b.subagentRuns[taskID]
	if run == nil {
		run = b.rehydrateRunLocked(taskID)
	}
	if run != nil {
		run.pendingPrompt = prompt
	}
}

// rehydrateRunLocked reconstructs a subagentRun for taskID from CC's persisted
// agent-<taskID>.meta.json when this backend has no in-memory record of it — the
// #1433 post-restart recovery. foci's run maps are in-memory only and CC's
// `--resume` does not re-stream the historical Agent tool_use block, so after a
// restart the ONLY source of a pre-restart subagent's original group key (the Agent
// tool_use id) + label is the on-disk meta sidecar (verified live 2026-07-20). The
// rebuilt run is inserted inactive at runIndex 1; the caller (a task_started or a
// SendMessage stash) advances it. Also back-fills agentLabels so any later lookup
// under the recovered group key is consistent. Returns nil when the sidecar is
// absent/unreadable (not a subagent we can identify — the caller must NOT open a
// blank chit). Caller must hold subagentRunsMu.
func (b *Backend) rehydrateRunLocked(taskID string) *subagentRun {
	groupKey, label, ok := b.loadSubagentMeta(taskID)
	if !ok {
		return nil
	}
	if b.subagentRuns == nil {
		b.subagentRuns = map[string]*subagentRun{}
	}
	run := &subagentRun{groupKey: groupKey, label: label, runIndex: 1, active: false}
	b.subagentRuns[taskID] = run
	if b.agentLabels == nil {
		b.agentLabels = map[string]string{}
	}
	b.agentLabels[groupKey] = label
	b.logger().Infof("subagent_rehydrate task_id=%s group=%s label=%q (from on-disk meta.json, post-restart)", taskID, groupKey, label)
	return run
}

// onTaskStarted binds or advances a subagent's run state at a task_started event.
// The FIRST task_started for a task_id binds the run (groupKey = the Agent
// tool_use_id it carries) at runIndex 1 and returns reactivated=false, with the
// run's stashed launch prompt — run 1's SubagentStart is NORMALLY emitted by the
// PreToolUse hook (fires earlier, around the tool_use itself), but the caller
// uses the returned run + prompt to emit a FALLBACK start (#1425, guarded by
// markSubagentStarted) when the hook drops (#1423, ~7% of background subagents).
// A SUBSEQUENT task_started (same task_id, new tool_use_id) is a SendMessage resume:
// it bumps runIndex and returns reactivated=true with the pending resume prompt, so
// the caller re-Adds to the tracker and emits a fresh SubagentStart for the new run
// (no hook exists for SendMessage, so this path is unconditional, unguarded by
// markSubagentStarted).
//
// #1433: a task_started whose task_id is untracked in memory is only a genuine fresh
// Agent spawn when its tool_use_id was stashed live off an Agent tool_use block this
// session (agentLabels has it). Otherwise it is a SendMessage/resume id whose Agent
// spawn this backend never saw — the post-restart follow-up case — and binding
// groupKey = that resume id yields a BLANK chit (label empty). Such a task_started is
// instead rehydrated from CC's on-disk meta.json into a REACTIVATION under the
// original group key; if it can't be identified at all, NO start is emitted (a blank
// start is worse than none).
func (b *Backend) onTaskStarted(taskID, toolUseID string) (run *subagentRun, reactivated bool, prompt string) {
	if taskID == "" {
		return nil, false, ""
	}
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	if b.subagentRuns == nil {
		b.subagentRuns = map[string]*subagentRun{}
	}
	existing := b.subagentRuns[taskID]
	if existing == nil {
		if _, known := b.agentLabels[toolUseID]; known {
			// Genuine fresh Agent spawn: the Agent tool_use block streamed live this
			// session (OnAssistant stashed its label + prompt under toolUseID, which
			// IS the Agent tool_use id == the stable groupKey). Bind run 1 and return
			// it as a first-sighting so the caller emits the #1425 fallback start
			// (guarded by markSubagentStarted) with the stashed content.
			nr := &subagentRun{groupKey: toolUseID, label: b.agentLabels[toolUseID], runIndex: 1, active: true}
			b.subagentRuns[taskID] = nr
			return nr, false, b.agentPrompts[toolUseID]
		}
		// Not a fresh spawn (toolUseID is a SendMessage/resume id with no live stash):
		// the #1433 post-restart follow-up. Recover the subagent's real identity from
		// disk so it reactivates under the ORIGINAL group key instead of opening a
		// blank chit. Nil => unidentifiable => caller emits nothing.
		existing = b.rehydrateRunLocked(taskID)
		if existing == nil {
			return nil, false, ""
		}
	}
	// Reactivation: an in-session resume of a tracked run, or a just-rehydrated
	// post-restart run. Bump the run index, mark active, hand back the pending
	// resume prompt.
	existing.runIndex++
	existing.active = true
	prompt = existing.pendingPrompt
	existing.pendingPrompt = ""
	return existing, true, prompt
}

// endRunForTask looks up the run state for a task_id at its task_notification:
// completed and marks it inactive, so a LATER SendMessage to the same task_id
// (before any reactivation) correctly stashes for the eventual resume (#1355)
// instead of trying to surface immediately as if the run were still live (#1419).
// Returns nil if untracked (task_started was missed).
func (b *Backend) endRunForTask(taskID string) *subagentRun {
	if taskID == "" {
		return nil
	}
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	run := b.subagentRuns[taskID]
	if run != nil {
		run.active = false
	}
	return run
}

// activeRunForTask returns the run state for a task_id and whether it is
// currently ACTIVE (task_started seen, matching task_notification:completed not
// yet seen) — the signal a SendMessage block uses to decide whether its message
// can be surfaced immediately (still running, #1419) or must be stashed for the
// eventual reactivation (already ended, #1355). Returns (nil, false) if
// untracked (the SendMessage target isn't a subagent we've seen task_started for).
func (b *Backend) activeRunForTask(taskID string) (run *subagentRun, active bool) {
	if taskID == "" {
		return nil, false
	}
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	run = b.subagentRuns[taskID]
	if run == nil {
		return nil, false
	}
	return run, run.active
}

// runIndexForGroup returns the current run index for a subagent identified by its
// stable groupKey (the original Agent tool_use id) — the run a text block emitted
// now belongs to. Subagent text keeps the ORIGINAL parent tool_use id across
// reactivations (verified via live probe), so it maps to the run whose groupKey
// matches. Returns 1 when untracked (start missed / pre-reactivation), matching
// the client's runIndex.coerceAtLeast(1) default. There is one run entry per
// groupKey (a task_id is reused across reactivations, its runIndex bumped).
func (b *Backend) runIndexForGroup(groupKey string) int {
	if groupKey == "" {
		return 1
	}
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	for _, run := range b.subagentRuns {
		if run.groupKey == groupKey {
			return run.runIndex
		}
	}
	return 1
}
