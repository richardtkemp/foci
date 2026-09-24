package ccstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"foci/internal/delegator"
)

// Nested subagents (#1554). The events replayed here are the shapes captured
// live on CC 2.1.280 (verify-cc-stream-hooks nested_probe.sh and
// nested_bg_text_probe.sh, 2026-09-24):
//
//   - the nested Agent tool_use block reaches the parent stream tagged
//     parent_tool_use_id = the spawner's groupKey, BEFORE the nested task_started;
//   - the nested PreToolUse hook carries agent_id = the spawner's task_id;
//   - task_started / task_notification carry no parentage at all;
//   - a background grandchild's text is tagged with its OWN Agent id.

type nestedRecorder struct {
	starts []string
	ends   []string
	texts  [][2]string // {groupKey, text}
}

func (r *nestedRecorder) attach(b *Backend) {
	b.AttachSessionEvents(&delegator.SessionEvents{
		OnSubagentStart: func(groupKey, _, _ string, _ int) { r.starts = append(r.starts, groupKey) },
		OnSubagentEnd:   func(groupKey string, _ int) { r.ends = append(r.ends, groupKey) },
		OnSubagentText: func(groupKey, text string, _ int) {
			r.texts = append(r.texts, [2]string{groupKey, text})
		},
	})
	b.beginTurn(nil)
}

func nestedTaskEvent(t *testing.T, subtype, taskID, toolUseID, status string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(TaskEvent{Subtype: subtype, TaskID: taskID, ToolUseID: toolUseID, Status: status, TaskType: "local_agent"})
	if err != nil {
		t.Fatalf("marshal TaskEvent: %v", err)
	}
	return raw
}

// spawnTopLevel replays the main thread's Agent tool_use and its task_started.
func spawnTopLevel(t *testing.T, b *Backend, groupKey, taskID string, background bool) {
	t.Helper()
	input := `{"description":"top work","prompt":"do it","run_in_background":false}`
	if background {
		input = `{"description":"top work","prompt":"do it","run_in_background":true}`
	}
	b.OnAssistant(&AssistantMessage{Message: BetaMessage{Content: []ContentBlock{
		{Type: "tool_use", ID: groupKey, Name: "Agent", Input: json.RawMessage(input)},
	}}})
	b.OnSystem("task_started", nestedTaskEvent(t, "task_started", taskID, groupKey, ""))
}

// subagentMsg replays a parent-stream assistant message from inside group ptu.
func subagentMsg(b *Backend, ptu string, blocks ...ContentBlock) {
	b.OnAssistant(&AssistantMessage{ParentToolUseID: &ptu, Message: BetaMessage{Content: blocks}})
}

func nestedAgentBlock(id string) ContentBlock {
	return ContentBlock{Type: "tool_use", ID: id, Name: "Agent", Input: json.RawMessage(`{"description":"nested work","prompt":"sub"}`)}
}

// nestedPreToolUse builds the hook_response for a nested Agent PreToolUse.
func nestedPreToolUse(t *testing.T, install, toolUseID, spawnerTaskID string) json.RawMessage {
	t.Helper()
	stdout, _ := json.Marshal(hookScriptOutput{
		HookEvent: "PreToolUse", InstallID: install, ToolUseID: toolUseID,
		ToolName: "Agent", ToolInput: `{"description":"nested work"}`, AgentID: spawnerTaskID,
	})
	env, _ := json.Marshal(hookResponseEnvelope{HookEvent: "PreToolUse", Stdout: string(stdout), Outcome: "success"})
	return env
}

// The core regression: a grandchild opens no chit, sends no end, and leaves its
// spawner's tracker entry alone.
func TestNestedAgent_NoChitNoEnd(t *testing.T) {
	var r nestedRecorder
	b := &Backend{}
	r.attach(b)

	spawnTopLevel(t, b, "toolu_parent", "task_parent", true)
	subagentMsg(b, "toolu_parent", nestedAgentBlock("toolu_nested"))
	b.OnSystem("task_started", nestedTaskEvent(t, "task_started", "task_nested", "toolu_nested", ""))
	b.OnSystem("task_notification", nestedTaskEvent(t, "task_notification", "task_nested", "toolu_nested", "completed"))

	if len(r.starts) != 1 || r.starts[0] != "toolu_parent" {
		t.Errorf("starts = %v, want only [toolu_parent]", r.starts)
	}
	if len(r.ends) != 0 {
		t.Errorf("ends = %v, want none (the grandchild ended, its spawner did not)", r.ends)
	}
	if got := b.agents.Pending(); got != 1 {
		t.Errorf("Pending() = %d, want 1 (the spawner is still running)", got)
	}

	b.OnSystem("task_notification", nestedTaskEvent(t, "task_notification", "task_parent", "toolu_parent", "completed"))
	if len(r.ends) != 1 || r.ends[0] != "toolu_parent" {
		t.Errorf("ends = %v, want [toolu_parent] once the spawner completes", r.ends)
	}
	if got := b.agents.Pending(); got != 0 {
		t.Errorf("Pending() = %d after spawner completed, want 0", got)
	}
}

// The hook alone (native block missed) is enough to recognise a nested spawn:
// its agent_id is the spawner's task_id.
func TestNestedAgent_HookOnlyRegisters(t *testing.T) {
	var r nestedRecorder
	b := &Backend{hookInstallID: "install-us"}
	r.attach(b)

	spawnTopLevel(t, b, "toolu_parent", "task_parent", true)
	b.handleHookResponse(nestedPreToolUse(t, "install-us", "toolu_nested", "task_parent"))
	b.OnSystem("task_started", nestedTaskEvent(t, "task_started", "task_nested", "toolu_nested", ""))
	b.OnSystem("task_notification", nestedTaskEvent(t, "task_notification", "task_nested", "toolu_nested", "completed"))

	if len(r.starts) != 1 || len(r.ends) != 0 {
		t.Errorf("starts=%v ends=%v, want [toolu_parent] and none", r.starts, r.ends)
	}
	if anc, nested := b.topLevelAncestor("toolu_nested"); !nested || anc != "toolu_parent" {
		t.Errorf("topLevelAncestor = (%q, %v), want (toolu_parent, true)", anc, nested)
	}
}

// A background grandchild's text carries its own Agent id; it must land in the
// spawner's chit, including through a depth-3 chain.
func TestNestedAgent_TextReattributedToTopLevel(t *testing.T) {
	var r nestedRecorder
	b := &Backend{}
	r.attach(b)

	spawnTopLevel(t, b, "toolu_parent", "task_parent", true)
	subagentMsg(b, "toolu_parent", ContentBlock{Type: "text", Text: "child says"}, nestedAgentBlock("toolu_nested"))
	subagentMsg(b, "toolu_nested", ContentBlock{Type: "text", Text: "grandchild says"}, nestedAgentBlock("toolu_deep"))
	subagentMsg(b, "toolu_deep", ContentBlock{Type: "text", Text: "great-grandchild says"})

	want := [][2]string{
		{"toolu_parent", "child says"},
		{"toolu_parent", "grandchild says"},
		{"toolu_parent", "great-grandchild says"},
	}
	if len(r.texts) != len(want) {
		t.Fatalf("texts = %v, want %v", r.texts, want)
	}
	for i := range want {
		if r.texts[i] != want[i] {
			t.Errorf("texts[%d] = %v, want %v", i, r.texts[i], want[i])
		}
	}
}

// A grandchild completing must stop only ITS OWN transcript tail, never the
// spawner's, which is still being written.
func TestNestedAgent_CompletionLeavesSpawnerTail(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var r nestedRecorder
	b := &Backend{workDir: "/work/dir", sessionID: "11111111-2222-3333-4444-555555555555"}
	r.attach(b)
	m := b.subagentTails()
	t.Cleanup(func() { m.finalize("toolu_parent") })

	spawnTopLevel(t, b, "toolu_parent", "task_parent", false)
	subagentMsg(b, "toolu_parent", nestedAgentBlock("toolu_nested"))
	b.OnSystem("task_started", nestedTaskEvent(t, "task_started", "task_nested", "toolu_nested", ""))

	m.mu.Lock()
	_, nestedTailing := m.tails["toolu_nested"]
	m.mu.Unlock()
	if !nestedTailing {
		t.Fatal("no tail for the grandchild: its usage would be lost")
	}

	b.OnSystem("task_notification", nestedTaskEvent(t, "task_notification", "task_nested", "toolu_nested", "completed"))

	m.mu.Lock()
	_, parentTailing := m.tails["toolu_parent"]
	_, nestedTailing = m.tails["toolu_nested"]
	m.mu.Unlock()
	if !parentTailing {
		t.Error("spawner's tail was stopped by its grandchild's completion")
	}
	if nestedTailing {
		t.Error("grandchild's tail still running after its completion")
	}
}

// A SendMessage resume of a grandchild: its task_* events carry the
// SendMessage's tool_use id, so only the remembered task_id identifies it.
func TestNestedAgent_ResumeStaysSuppressed(t *testing.T) {
	var r nestedRecorder
	b := &Backend{}
	r.attach(b)

	spawnTopLevel(t, b, "toolu_parent", "task_parent", true)
	subagentMsg(b, "toolu_parent", nestedAgentBlock("toolu_nested"))
	b.OnSystem("task_started", nestedTaskEvent(t, "task_started", "task_nested", "toolu_nested", ""))
	b.OnSystem("task_notification", nestedTaskEvent(t, "task_notification", "task_nested", "toolu_nested", "completed"))
	b.OnSystem("task_started", nestedTaskEvent(t, "task_started", "task_nested", "toolu_sendmsg", ""))
	b.OnSystem("task_notification", nestedTaskEvent(t, "task_notification", "task_nested", "toolu_sendmsg", "completed"))

	if len(r.starts) != 1 || len(r.ends) != 0 {
		t.Errorf("starts=%v ends=%v, want [toolu_parent] and none", r.starts, r.ends)
	}
}

// After a foci restart the maps are empty and the Agent block is never
// re-streamed. The meta sidecar's spawnDepth is then the only depth signal:
// without it a resumed grandchild is rehydrated as a depth-1 "reactivation".
func TestNestedAgent_PostRestartMetaSpawnDepth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const workDir, sessionID = "/work/dir", "11111111-2222-3333-4444-555555555555"
	dir := filepath.Join(home, ccProjectsDir, projectSlug(workDir), sessionID, "subagents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(taskID string, meta map[string]any) {
		body, _ := json.Marshal(meta)
		if err := os.WriteFile(filepath.Join(dir, "agent-"+taskID+".meta.json"), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Shapes captured live on CC 2.1.280.
	write("task_parent", map[string]any{"description": "top work", "toolUseId": "toolu_parent", "spawnDepth": 1})
	write("task_nested", map[string]any{"description": "nested work", "toolUseId": "toolu_nested",
		"parentAgentId": "task_parent", "spawnDepth": 2})

	var r nestedRecorder
	b := &Backend{workDir: workDir, sessionID: sessionID}
	r.attach(b)
	t.Cleanup(func() { b.subagentTails().finalize("toolu_sendmsg") })

	b.OnSystem("task_started", nestedTaskEvent(t, "task_started", "task_nested", "toolu_sendmsg", ""))
	subagentMsg(b, "toolu_nested", ContentBlock{Type: "text", Text: "resumed grandchild"})
	b.OnSystem("task_notification", nestedTaskEvent(t, "task_notification", "task_nested", "toolu_sendmsg", "completed"))

	if len(r.starts) != 0 || len(r.ends) != 0 {
		t.Errorf("starts=%v ends=%v, want none for a resumed grandchild", r.starts, r.ends)
	}
	if len(r.texts) != 1 || r.texts[0][0] != "toolu_parent" {
		t.Errorf("texts = %v, want the resumed grandchild's text under toolu_parent", r.texts)
	}
}
