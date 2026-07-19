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
