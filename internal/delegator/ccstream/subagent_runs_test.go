package ccstream

import (
	"encoding/json"
	"testing"
)

// TestSubagentReactivation drives the full #1355 lifecycle through the real stream
// entry points and pins the fix: a SendMessage-resumed subagent re-opens the
// activity tracker AND emits a fresh per-run SubagentStart under the STABLE group
// key (the original Agent tool_use id), and each run's end maps back to that group
// key + run index — NOT the resume's fresh tool_use id.
//
// Wire facts it encodes (verified live 2026-07-19): task_id is stable across a
// resume; the resumed run's task_started/task_notification carry a NEW tool_use id;
// the subagent's text keeps the original tool_use id as its group key.
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
	// The first task_started must NOT emit a start (run 1's start is hook-driven).
	if len(starts) != 0 {
		t.Fatalf("first task_started emitted a start: %+v", starts)
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
	// SendMessage prompt — and the tracker re-Added (chip re-opens).
	if len(starts) != 1 {
		t.Fatalf("reactivation starts = %+v, want exactly 1", starts)
	}
	if got := starts[0]; got != (start{groupKey, "Explore", "do part two", 2}) {
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

	// Launch + bind run 1 — still running (no task_notification yet).
	b.OnAssistant(mkTool(groupKey, "Agent", `{"description":"Explore","prompt":"do part one","run_in_background":true}`))
	sys(b, "task_started", TaskEvent{TaskID: taskID, ToolUseID: groupKey})

	// A SendMessage arrives WHILE the subagent is still running.
	b.OnAssistant(mkTool(msg1ID, "SendMessage", `{"to":"task_abc","message":"do part two, still running"}`))

	// Must surface immediately, on run 1 — no new run opened.
	if len(prompts) != 1 || prompts[0] != (prompt{groupKey, "do part two, still running", 1}) {
		t.Fatalf("prompts after still-running SendMessage = %+v, want exactly [{toolu_agent \"do part two, still running\" 1}]", prompts)
	}
	if len(starts) != 0 {
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
	if len(starts) != 1 || starts[0] != (start{groupKey, "Explore", "do part three, after end", 2}) {
		t.Fatalf("after-end SendMessage reactivation start = %+v, want [{toolu_agent Explore \"do part three, after end\" 2}]", starts)
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
