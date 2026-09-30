package ccstream

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

// TestStopSubagentsAndCommands_StopEachKindSeparately pins #2138 and #2140:
// each stop sends one stop_task per task of ITS kind, naming the task_id (not
// the tracker's tool_use key), and never an interrupt, so the turn and the
// other kind carry on. Subagents are the Agent-tool spawns. Commands are the
// main agent's own shell calls, background and foreground, but not a command a
// subagent runs (stopping the subagent stops that). A task whose task_started
// has not arrived has no task_id and is skipped.
func TestStopSubagentsAndCommands_StopEachKindSeparately(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	b := newTestBackend(&buf)
	tool := func(parent *string, id, name, input string) *AssistantMessage {
		return &AssistantMessage{ParentToolUseID: parent, Message: BetaMessage{
			Content: []ContentBlock{{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)}},
		}}
	}
	task := func(subtype, taskID, toolUseID, taskType, status string) []byte {
		raw, _ := json.Marshal(map[string]any{
			"type": "system", "subtype": subtype, "task_id": taskID, "tool_use_id": toolUseID, "task_type": taskType, "status": status,
		})
		return raw
	}
	parent := "toolu_agent"
	b.OnAssistant(tool(nil, "toolu_agent", "Agent", `{"description":"researcher","prompt":"go","model":"haiku"}`))
	b.OnAssistant(tool(nil, "toolu_bash", "Bash", `{"command":"sleep 60","run_in_background":true}`))
	b.OnAssistant(tool(nil, "toolu_late", "Agent", `{"description":"not started yet","prompt":"go"}`))
	b.OnAssistant(tool(nil, "toolu_fg", "Bash", `{"command":"make test"}`))
	b.OnAssistant(tool(nil, "toolu_quick", "Bash", `{"command":"echo hi"}`))
	b.OnAssistant(tool(&parent, "toolu_sub_bash", "Bash", `{"command":"make build"}`))
	b.OnSystem("task_started", task("task_started", "task_a", "toolu_agent", "local_agent", ""))
	b.OnSystem("task_started", task("task_started", "task_b", "toolu_bash", taskTypeBash, ""))
	b.OnSystem("task_started", task("task_started", "task_c", "toolu_fg", taskTypeBash, ""))
	b.OnSystem("task_started", task("task_started", "task_d", "toolu_sub_bash", taskTypeBash, ""))

	stop := func(name string, fn func() (int, error), want []string, wantN int) {
		t.Helper()
		buf.Reset()
		n, err := fn()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out := buf.String()
		if n != wantN || strings.Count(out, `"subtype":"stop_task"`) != wantN {
			t.Fatalf("%s sent %d stop requests, want %d; wire:\n%s", name, n, wantN, out)
		}
		for _, id := range want {
			if !strings.Contains(out, `"subtype":"stop_task","task_id":"`+id+`"`) {
				t.Errorf("%s: stop_task %s not sent; wire:\n%s", name, id, out)
			}
		}
		if strings.Contains(out, "interrupt") {
			t.Errorf("%s sent an interrupt; it must leave the turn alone:\n%s", name, out)
		}
	}
	stop("StopSubagents", func() (int, error) { return b.StopSubagents(t.Context()) }, []string{"task_a"}, 1)
	stop("StopCommands", func() (int, error) { return b.StopCommands(t.Context()) }, []string{"task_b", "task_c"}, 2)

	// A finished foreground command leaves at its notification; one CC never
	// registered (too quick) leaves at turn end.
	b.OnSystem("task_notification", task("task_notification", "task_c", "toolu_fg", "", "completed"))
	stop("StopCommands after the foreground command ended", func() (int, error) { return b.StopCommands(t.Context()) }, []string{"task_b"}, 1)
	b.OnResult(&ResultMessage{Subtype: "success"})
	if ids := b.mainShellCallIDs(); len(ids) != 0 {
		t.Errorf("main shell calls after turn end = %v, want none", ids)
	}
}

// TestSubagentRunning_CarriesKindModelAndRealModel pins #2138's running list:
// an Agent spawn reports its requested model alias and subagent_type, a
// background Bash reports kind "command", and the transcript's real model
// replaces the alias.
func TestSubagentRunning_CarriesKindModelAndRealModel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	b := newTestBackend(&buf)
	var last []delegator.RunningSubagent
	b.SetOnSubagentRunning(func(r []delegator.RunningSubagent) { last = r })
	b.OnAssistant(&AssistantMessage{Message: BetaMessage{Content: []ContentBlock{
		{Type: "tool_use", ID: "toolu_agent", Name: "Agent", Input: json.RawMessage(`{"description":"researcher","prompt":"go","model":"haiku","subagent_type":"Explore"}`)},
		{Type: "tool_use", ID: "toolu_bash", Name: "Bash", Input: json.RawMessage(`{"command":"sleep 60","run_in_background":true}`)},
	}}})
	if len(last) != 2 {
		t.Fatalf("running = %+v, want 2 entries", last)
	}
	if a := last[0]; a.ID != "toolu_agent" || a.Kind != delegator.SubagentKindAgent || a.Model != "haiku" || a.SubagentType != "Explore" || a.Started.IsZero() {
		t.Errorf("agent entry = %+v", a)
	}
	if c := last[1]; c.Kind != delegator.SubagentKindCommand || c.Command != "sleep 60" {
		t.Errorf("bash entry = %+v, want kind command with its command line", c)
	}
	b.noteSubagentTranscriptUsage("toolu_agent", "<synthetic>", "m0", time.Now(), true, TokenUsage{})
	if last[0].Model != "haiku" {
		t.Errorf("a <synthetic> record replaced the model: %q", last[0].Model)
	}
	b.noteSubagentTranscriptUsage("toolu_agent", "claude-haiku-4-5", "m1", time.Now(), true, TokenUsage{})
	if last[0].Model != "claude-haiku-4-5" {
		t.Errorf("model = %q after the transcript named it, want claude-haiku-4-5", last[0].Model)
	}
}
