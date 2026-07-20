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

// stashResumePrompt records the message a SendMessage block sent to a subagent
// (keyed by its task_id == the SendMessage `to`), to be surfaced as the prompt on
// the reactivation's SubagentStart. No-op if we've never seen that task_id (the
// SendMessage targeted something we aren't tracking).
func (b *Backend) stashResumePrompt(taskID, prompt string) {
	if taskID == "" {
		return
	}
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	if run := b.subagentRuns[taskID]; run != nil {
		run.pendingPrompt = prompt
	}
}

// onTaskStarted binds or advances a subagent's run state at a task_started event.
// The FIRST task_started for a task_id binds the run (groupKey = the Agent
// tool_use_id it carries) at runIndex 1 and returns reactivated=false — run 1's
// SubagentStart is emitted by the PreToolUse hook, so the caller emits nothing.
// A SUBSEQUENT task_started (same task_id, new tool_use_id) is a SendMessage resume:
// it bumps runIndex and returns reactivated=true with the pending resume prompt, so
// the caller re-Adds to the tracker and emits a fresh SubagentStart for the new run.
func (b *Backend) onTaskStarted(taskID, toolUseID string) (run *subagentRun, reactivated bool, prompt string) {
	if taskID == "" {
		return nil, false, ""
	}
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	if b.subagentRuns == nil {
		b.subagentRuns = map[string]*subagentRun{}
	}
	if existing := b.subagentRuns[taskID]; existing != nil {
		existing.runIndex++
		existing.active = true
		prompt = existing.pendingPrompt
		existing.pendingPrompt = ""
		return existing, true, prompt
	}
	// First sighting: the tool_use_id on this first task_started IS the Agent
	// tool_use_id (the stable groupKey). Bind and reuse the stashed label.
	nr := &subagentRun{groupKey: toolUseID, label: b.agentLabels[toolUseID], runIndex: 1, active: true}
	b.subagentRuns[taskID] = nr
	return nr, false, ""
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
