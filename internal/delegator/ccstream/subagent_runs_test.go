package ccstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeSubagentMeta writes a CC subagent meta.json (the task_id -> original Agent
// tool_use id + description bridge CC persists on disk) at the exact path
// b.loadSubagentMeta reads from, so a test can exercise post-restart rehydration
// (#1433) hermetically. HOME must already be pointed at a temp dir by the caller.
func writeSubagentMeta(t *testing.T, home, workDir, sessionID, taskID, toolUseID, description string) {
	t.Helper()
	dir := filepath.Join(home, ccProjectsDir, projectSlug(workDir), sessionID, "subagents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir subagents dir: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"agentType": "general-purpose", "description": description,
		"toolUseId": toolUseID, "spawnDepth": 1,
	})
	if err := os.WriteFile(filepath.Join(dir, "agent-"+taskID+".meta.json"), body, 0o644); err != nil {
		t.Fatalf("write meta.json: %v", err)
	}
}

// TestSubagentPostRestartFollowUpRehydratesIdentity is the RED->GREEN regression
// for #1433. The live bug: foci's subagent run maps are IN-MEMORY only and do not
// survive a foci restart; CC's `--resume` keeps the SAME session uuid but does NOT
// re-stream the historical Agent tool_use block (both verified live via the
// verify-cc-stream-hooks restart probe, 2026-07-20), so agentLabels is empty after
// a restart. A SendMessage FOLLOW-UP to a pre-restart subagent then arrives as a
// task_started whose tool_use id is the SendMessage id (task_id stable). Before the
// fix, onTaskStarted misclassifies this as a fresh first-sighting: groupKey = the
// SendMessage id, label = agentLabels[SendMessage id] = "" -> the #1425 fallback
// emits a BLANK SubagentStart under the wrong group key (the blank chit users saw).
//
// The fix recovers the subagent's real identity from CC's persisted
// agent-<task_id>.meta.json ({toolUseId == original Agent id == stable groupKey,
// description == label}), so the follow-up is treated as a REACTIVATION under the
// ORIGINAL group key — which is also what the resumed subagent's text keeps as its
// parent_tool_use_id post-restart (verified live), so it collapses into the
// original chit instead of opening a blank new one.
func TestSubagentPostRestartFollowUpRehydratesIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	const (
		workDir      = "/work/dir"
		sessionID    = "11111111-2222-3333-4444-555555555555"
		taskID       = "a_restart_task"           // stable subagent identity
		origGroupKey = "toolu_orig_agent"          // the pre-restart Agent tool_use id (in meta.json)
		sendMsgID    = "toolu_send_after_restart"  // the follow-up SendMessage's tool_use id
		label        = "Explore the codebase"      // meta.json description
		followUp     = "now also check the config" // the SendMessage body
	)
	// CC's on-disk bridge, written during the PRE-restart session.
	writeSubagentMeta(t, home, workDir, sessionID, taskID, origGroupKey, label)

	// A FRESH backend, exactly as after a foci restart: empty maps, no memory of
	// the Agent spawn. sessionID/workDir are what --resume reconnects with.
	b := &Backend{workDir: workDir, sessionID: sessionID}
	type start struct {
		group, label, prompt string
		run                  int
	}
	type end struct {
		group string
		run   int
	}
	var starts []start
	var ends []end
	applyHandler(b, &testHandler{
		OnSubagentStart: func(g, l, p string, r int) { starts = append(starts, start{g, l, p, r}) },
		OnSubagentEnd:   func(g string, r int) { ends = append(ends, end{g, r}) },
	})

	// Post-restart follow-up: SendMessage to the pre-restart subagent (by task_id),
	// then the resumed task_started carrying the SendMessage's tool_use id.
	mkTool := func(id, name, input string) *AssistantMessage {
		return &AssistantMessage{Message: BetaMessage{
			Content: []ContentBlock{{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)}},
		}}
	}
	sys := func(subtype string, ev TaskEvent) {
		ev.Subtype = subtype
		raw, _ := json.Marshal(ev)
		b.OnSystem(subtype, raw)
	}

	b.OnAssistant(mkTool(sendMsgID, "SendMessage",
		`{"to":"`+taskID+`","message":"`+followUp+`"}`))
	sys("task_started", TaskEvent{TaskID: taskID, ToolUseID: sendMsgID})

	// Exactly one start, under the ORIGINAL group key, with the real label + the
	// follow-up prompt — NEVER a blank chit under the SendMessage id.
	if len(starts) != 1 {
		t.Fatalf("post-restart follow-up starts = %+v, want exactly 1", starts)
	}
	got := starts[0]
	if got.group == sendMsgID {
		t.Fatalf("start emitted under the SendMessage id %q (blank-chit bug) — want the original group key %q", sendMsgID, origGroupKey)
	}
	if got.group != origGroupKey {
		t.Fatalf("start group = %q, want original group key %q", got.group, origGroupKey)
	}
	if got.label != label {
		t.Fatalf("start label = %q, want %q (blank label is the #1433 bug)", got.label, label)
	}
	if got.prompt != followUp {
		t.Fatalf("start prompt = %q, want the follow-up message %q", got.prompt, followUp)
	}

	// The resumed subagent's text keeps the ORIGINAL Agent parent_tool_use_id
	// post-restart (verified live) — it must map to the rehydrated run, not read as
	// the untracked default.
	if idx := b.runIndexForGroup(origGroupKey); idx != got.run {
		t.Errorf("runIndexForGroup(%q) = %d, want %d (the reactivated run index)", origGroupKey, idx, got.run)
	}

	// The run's end (task_notification carries the SendMessage tool_use id) must map
	// back to the original group key via the stable task_id — not close a phantom
	// group under the SendMessage id.
	sys("task_notification", TaskEvent{TaskID: taskID, ToolUseID: sendMsgID, Status: "completed"})
	if len(ends) != 1 || ends[0].group != origGroupKey {
		t.Fatalf("post-restart run end = %+v, want group %q", ends, origGroupKey)
	}
}

// TestSubagentReactivation drives the full #1355 lifecycle through the real stream
// entry points and pins the fix: a SendMessage-resumed subagent re-opens the
// activity tracker AND emits a fresh per-run SubagentStart under the STABLE group
// key (the original Agent tool_use id), and each run's end maps back to that group
// key + run index — NOT the resume's fresh tool_use id.
//
// Wire facts it encodes (verified live 2026-07-19): task_id is stable across a
// resume; the resumed run's task_started/task_notification carry a NEW tool_use id;
// the subagent's text keeps the original tool_use id as its group key.
//
// #1425 UPDATE: this test drives task_started WITHOUT ever firing the PreToolUse
// hook, so run 1's start now comes from the task_started FALLBACK (the hook is
// absent here exactly like the #1423 bug — the fallback is the only source that
// fires). See TestSubagentStartFallbackWhenHookNeverFires for a focused version of
// just that case, and TestSubagentStartHookAndTaskStartedRaceEmitExactlyOnce (both
// orderings) for the dedup guarantee this test does NOT exercise.
func TestSubagentReactivation(t *testing.T) {
	mkTool := func(id, name, input string) *AssistantMessage {
		return &AssistantMessage{Message: BetaMessage{
			Content: []ContentBlock{{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)}},
		}}
	}
	sys := func(b *Backend, subtype string, ev TaskEvent) {
		ev.Subtype = subtype
		raw, _ := json.Marshal(ev)
		b.OnSystem(subtype, raw)
	}

	b := &Backend{}
	type start struct {
		group, label, prompt string
		run                  int
	}
	type end struct {
		group string
		run   int
	}
	var starts []start
	var ends []end
	applyHandler(b, &testHandler{
		OnSubagentStart: func(g, l, p string, r int) { starts = append(starts, start{g, l, p, r}) },
		OnSubagentEnd:   func(g string, r int) { ends = append(ends, end{g, r}) },
	})

	const (
		groupKey = "toolu_agent" // original Agent tool_use id == stable group key
		taskID   = "task_abc"    // stable across the resume
		resumeID = "toolu_send"  // the SendMessage / resumed run's fresh tool_use id
	)

	// Run 1: the Agent spawn (label stashed, tracker Add), task_started binds the
	// run, then task_notification:completed ends it (the subagent stops + notifies
	// BEFORE the parent resumes it — the real sequence).
	b.OnAssistant(mkTool(groupKey, "Agent", `{"description":"Explore","prompt":"do part one","run_in_background":true}`))
	if b.agents.Pending() != 1 {
		t.Fatalf("setup: Pending after Agent spawn = %d, want 1", b.agents.Pending())
	}
	sys(b, "task_started", TaskEvent{TaskID: taskID, ToolUseID: groupKey})
	// #1425: with no PreToolUse hook ever firing in this test, the first
	// task_started's FALLBACK is the only source and must emit run 1's start,
	// carrying the label + prompt stashed off the Agent tool_use block.
	if len(starts) != 1 || starts[0] != (start{groupKey, "Explore", "do part one", 1}) {
		t.Fatalf("first task_started fallback start = %+v, want exactly [{toolu_agent Explore \"do part one\" 1}]", starts)
	}
	// Subagent text emitted now (run 1) maps to run index 1 via the stable group key.
	if got := b.runIndexForGroup(groupKey); got != 1 {
		t.Errorf("runIndexForGroup after run 1 start = %d, want 1", got)
	}
	sys(b, "task_notification", TaskEvent{TaskID: taskID, ToolUseID: groupKey, Status: "completed"})
	if b.agents.Pending() != 0 {
		t.Fatalf("run 1 end did not clear tracker: Pending()=%d, want 0", b.agents.Pending())
	}
	if len(ends) != 1 || ends[0] != (end{groupKey, 1}) {
		t.Fatalf("run 1 end = %+v, want [{toolu_agent 1}]", ends)
	}

	// Resume: a SendMessage targeting the subagent (by task_id), then the resumed
	// task_started with a FRESH tool_use id.
	b.OnAssistant(mkTool(resumeID, "SendMessage", `{"to":"task_abc","message":"do part two"}`))
	sys(b, "task_started", TaskEvent{TaskID: taskID, ToolUseID: resumeID})

	// A fresh SubagentStart for run 2, under the STABLE group key, carrying the
	// SendMessage prompt — and the tracker re-Added (chip re-opens). starts[0] is
	// run 1's #1425 fallback from above; this is the SECOND entry.
	if len(starts) != 2 {
		t.Fatalf("reactivation starts = %+v, want exactly 2 (run-1 fallback + run-2 reactivation)", starts)
	}
	if got := starts[1]; got != (start{groupKey, "Explore", "do part two", 2}) {
		t.Fatalf("reactivation start = %+v, want {toolu_agent Explore \"do part two\" 2}", got)
	}
	if b.agents.Pending() != 1 {
		t.Errorf("tracker not re-Added on reactivation: Pending()=%d, want 1", b.agents.Pending())
	}
	// Run 2's text now maps to run index 2 — the whole point of populating
	// SubagentText.RunIndex so the client's detail view groups it under run 2.
	if got := b.runIndexForGroup(groupKey); got != 2 {
		t.Errorf("runIndexForGroup after reactivation = %d, want 2", got)
	}
	// An untracked group (start missed) reads as run 1, matching the client's
	// runIndex.coerceAtLeast(1) default.
	if got := b.runIndexForGroup("no_such_group"); got != 1 {
		t.Errorf("runIndexForGroup(untracked) = %d, want 1", got)
	}

	// Run 2 ends: task_notification carries the RESUME tool_use id, but the end must
	// map back to the stable group key + run 2 (not resumeID), and clear the tracker.
	sys(b, "task_notification", TaskEvent{TaskID: taskID, ToolUseID: resumeID, Status: "completed"})
	if len(ends) != 2 || ends[1] != (end{groupKey, 2}) {
		t.Fatalf("run 2 end = %+v, want last {toolu_agent 2}", ends)
	}
	if b.agents.Pending() != 0 {
		t.Errorf("run 2 end did not clear tracker: Pending()=%d, want 0", b.agents.Pending())
	}
}

// TestSubagentPromptWhileRunning pins the #1419 fix: a SendMessage sent to a
// subagent that is STILL RUNNING (task_started seen, no task_notification yet)
// must surface immediately via OnSubagentPrompt at the run's CURRENT index — CC
// never refires task_started for this case (verified live), so #1355's
// stash-for-the-next-task_started mechanism never fires and the follow-up would
// otherwise be silently dropped. It must also NOT open a new run (no
// OnSubagentStart, no runIndex bump) — CC delivers exactly ONE
// task_notification:completed for the whole continuous execution, so a second
// "run" would never get its own end and its chit would spin forever.
//
// It also pins the boundary with #1355: once the run truly ends, a LATER
// SendMessage reverts to the stash-for-reactivation path (OnSubagentPrompt does
// NOT fire again; OnSubagentStart fires instead, on the resumed task_started).
func TestSubagentPromptWhileRunning(t *testing.T) {
	mkTool := func(id, name, input string) *AssistantMessage {
		return &AssistantMessage{Message: BetaMessage{
			Content: []ContentBlock{{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)}},
		}}
	}
	sys := func(b *Backend, subtype string, ev TaskEvent) {
		ev.Subtype = subtype
		raw, _ := json.Marshal(ev)
		b.OnSystem(subtype, raw)
	}

	b := &Backend{}
	type prompt struct {
		group, text string
		run         int
	}
	type start struct {
		group, label, prompt string
		run                  int
	}
	type end struct {
		group string
		run   int
	}
	var prompts []prompt
	var starts []start
	var ends []end
	applyHandler(b, &testHandler{
		OnSubagentStart:  func(g, l, p string, r int) { starts = append(starts, start{g, l, p, r}) },
		OnSubagentEnd:    func(g string, r int) { ends = append(ends, end{g, r}) },
		OnSubagentPrompt: func(g, p string, r int) { prompts = append(prompts, prompt{g, p, r}) },
	})

	const (
		groupKey = "toolu_agent" // original Agent tool_use id == stable group key
		taskID   = "task_abc"    // stable across resumes
		msg1ID   = "toolu_send1" // the still-running SendMessage's tool_use id
		msg2ID   = "toolu_send2" // the after-end SendMessage's tool_use id (real #1355 resume)
	)

	// Launch + bind run 1 — still running (no task_notification yet). #1425: no
	// PreToolUse hook fires in this test, so this task_started's FALLBACK emits
	// run 1's start.
	b.OnAssistant(mkTool(groupKey, "Agent", `{"description":"Explore","prompt":"do part one","run_in_background":true}`))
	sys(b, "task_started", TaskEvent{TaskID: taskID, ToolUseID: groupKey})
	if len(starts) != 1 || starts[0] != (start{groupKey, "Explore", "do part one", 1}) {
		t.Fatalf("run-1 fallback start = %+v, want exactly [{toolu_agent Explore \"do part one\" 1}]", starts)
	}

	// A SendMessage arrives WHILE the subagent is still running.
	b.OnAssistant(mkTool(msg1ID, "SendMessage", `{"to":"task_abc","message":"do part two, still running"}`))

	// Must surface immediately, on run 1 — no new run opened (starts stays at 1,
	// the run-1 fallback from above; still-running SendMessage adds no start).
	if len(prompts) != 1 || prompts[0] != (prompt{groupKey, "do part two, still running", 1}) {
		t.Fatalf("prompts after still-running SendMessage = %+v, want exactly [{toolu_agent \"do part two, still running\" 1}]", prompts)
	}
	if len(starts) != 1 {
		t.Fatalf("still-running SendMessage must not open a new run: starts = %+v", starts)
	}
	if got := b.runIndexForGroup(groupKey); got != 1 {
		t.Errorf("runIndexForGroup after still-running SendMessage = %d, want 1 (unchanged)", got)
	}

	// The run's TRUE end (one notification covers both part one and the
	// still-running follow-up) — must close run 1, not some phantom run 2.
	sys(b, "task_notification", TaskEvent{TaskID: taskID, ToolUseID: groupKey, Status: "completed"})
	if len(ends) != 1 || ends[0] != (end{groupKey, 1}) {
		t.Fatalf("end after still-running prompt = %+v, want [{toolu_agent 1}]", ends)
	}
	if b.agents.Pending() != 0 {
		t.Errorf("run end did not clear tracker: Pending()=%d, want 0", b.agents.Pending())
	}

	// A SECOND SendMessage, now that the run has ENDED, is the #1355 case: it
	// must NOT surface via OnSubagentPrompt — it stashes for the reactivation.
	b.OnAssistant(mkTool(msg2ID, "SendMessage", `{"to":"task_abc","message":"do part three, after end"}`))
	if len(prompts) != 1 {
		t.Fatalf("after-end SendMessage must not add an OnSubagentPrompt: prompts = %+v", prompts)
	}
	sys(b, "task_started", TaskEvent{TaskID: taskID, ToolUseID: msg2ID})
	if len(starts) != 2 || starts[1] != (start{groupKey, "Explore", "do part three, after end", 2}) {
		t.Fatalf("after-end SendMessage reactivation start = %+v, want last {toolu_agent Explore \"do part three, after end\" 2}", starts)
	}
}

// TestSubagentEndUntrackedFallsBackToToolUseID keeps the pre-#1355 behaviour for a
// task_notification whose task_started was never seen (untracked): end on the raw
// tool_use id at run index 0, so a missed start still finalizes a group.
func TestSubagentEndUntrackedFallsBackToToolUseID(t *testing.T) {
	b := &Backend{}
	var ends []string
	applyHandler(b, &testHandler{
		OnSubagentEnd: func(g string, r int) { ends = append(ends, g) },
	})
	raw, _ := json.Marshal(TaskEvent{Subtype: "task_notification", Status: "completed", ToolUseID: "toolu_orphan", TaskID: "task_unseen"})
	b.OnSystem("task_notification", raw)
	if len(ends) != 1 || ends[0] != "toolu_orphan" {
		t.Fatalf("untracked end = %v, want [toolu_orphan]", ends)
	}
}

// fireAgentPreToolUse simulates a PreToolUse hook_response for the Agent tool
// arriving on b's stream, exactly as handleHookResponse would parse it off CC's
// stdout — including tool_input, so the emitted start's label/prompt match what
// the SAME Agent tool_use's native OnAssistant detection would stash (the two
// paths must agree when testing which one "wins" a race). Mirrors the
// construction in TestHandleHookResponse_AgentPreToolUseFiresSubagentStart
// (hooks_test.go), plus ToolInput.
func fireAgentPreToolUse(b *Backend, toolUseID, installID, toolInput string) {
	stdout, _ := json.Marshal(hookScriptOutput{
		HookEvent: "PreToolUse", InstallID: installID,
		ToolUseID: toolUseID, ToolName: "Agent", ToolInput: toolInput,
	})
	env, _ := json.Marshal(hookResponseEnvelope{HookEvent: "PreToolUse", Stdout: string(stdout)})
	b.handleHookResponse(env)
}

// TestSubagentStartFallbackWhenHookNeverFires is the RED test for #1423/#1425: a
// BACKGROUND subagent (run_in_background:true, CC's Agent-tool default per the
// verify-cc-stream-hooks live probe) whose PreToolUse hook never fires — the
// ~7% case — must still get exactly one SubagentStart, sourced from the
// task_started fallback, carrying the same label + prompt a working hook would
// have produced. Before #1425 this asserted zero starts (the bug); it now
// asserts exactly one.
func TestSubagentStartFallbackWhenHookNeverFires(t *testing.T) {
	mkTool := func(id, name, input string) *AssistantMessage {
		return &AssistantMessage{Message: BetaMessage{
			Content: []ContentBlock{{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)}},
		}}
	}
	b := &Backend{}
	type start struct {
		group, label, prompt string
		run                  int
	}
	var starts []start
	applyHandler(b, &testHandler{
		OnSubagentStart: func(g, l, p string, r int) { starts = append(starts, start{g, l, p, r}) },
	})

	const groupKey = "toolu_bg_agent"

	// The Agent tool_use streams natively (always happens, hook or no hook) —
	// this is what stashes the label + prompt the fallback will read.
	b.OnAssistant(mkTool(groupKey, "Agent",
		`{"description":"Triage failing tests","prompt":"find the root cause","run_in_background":true}`))

	// The PreToolUse hook NEVER fires (dropped/raced — #1423). task_started
	// still arrives natively (it's not hook-sourced) and must fall back.
	raw, _ := json.Marshal(TaskEvent{Subtype: "task_started", TaskID: "task_bg1", ToolUseID: groupKey})
	b.OnSystem("task_started", raw)

	want := start{groupKey, "Triage failing tests", "find the root cause", 1}
	if len(starts) != 1 || starts[0] != want {
		t.Fatalf("fallback start = %+v, want exactly [%+v]", starts, want)
	}
}

// TestSubagentStartHookAndTaskStartedRaceEmitExactlyOnce proves the dedup
// guarantee (#1425): whichever of {PreToolUse hook, task_started fallback}
// arrives first wins and the other is a silent no-op — regardless of arrival
// order. Table-driven over both orderings, including the "hook fires LATE"
// edge (after task_started already fell back).
func TestSubagentStartHookAndTaskStartedRaceEmitExactlyOnce(t *testing.T) {
	mkTool := func(id, name, input string) *AssistantMessage {
		return &AssistantMessage{Message: BetaMessage{
			Content: []ContentBlock{{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)}},
		}}
	}
	const groupKey = "toolu_race_agent"

	run := func(t *testing.T, hookFirst bool) {
		b := &Backend{hookInstallID: "install-a"}
		type start struct {
			group, label, prompt string
			run                  int
		}
		var starts []start
		applyHandler(b, &testHandler{
			OnSubagentStart: func(g, l, p string, r int) { starts = append(starts, start{g, l, p, r}) },
		})

		b.OnAssistant(mkTool(groupKey, "Agent",
			`{"description":"Explore","prompt":"do the thing","run_in_background":true}`))
		taskStarted := func() {
			raw, _ := json.Marshal(TaskEvent{Subtype: "task_started", TaskID: "task_race", ToolUseID: groupKey})
			b.OnSystem("task_started", raw)
		}
		hook := func() {
			fireAgentPreToolUse(b, groupKey, "install-a", `{"description":"Explore","prompt":"do the thing"}`)
		}

		if hookFirst {
			hook()
			taskStarted() // the fallback's markSubagentStarted check must no-op
		} else {
			taskStarted() // the fallback claims the groupKey first
			hook()        // the "hook fires LATE" edge — must no-op, not double-emit
		}

		want := start{groupKey, "Explore", "do the thing", 1}
		if len(starts) != 1 || starts[0] != want {
			t.Fatalf("hookFirst=%v: starts = %+v, want exactly [%+v]", hookFirst, starts, want)
		}
	}

	t.Run("hook_then_task_started", func(t *testing.T) { run(t, true) })
	t.Run("task_started_then_late_hook", func(t *testing.T) { run(t, false) })
}
