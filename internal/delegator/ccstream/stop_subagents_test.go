package ccstream

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

// TestStopSubagents_SendsStopTaskPerTrackedTask pins #2138: stopping subagents
// sends one stop_task control request per tracked task, naming its task_id
// (not the tracker's tool_use key), and skips an entry whose task_started has
// not arrived yet. No interrupt is sent: the main turn is left alone.
func TestStopSubagents_SendsStopTaskPerTrackedTask(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	b := newTestBackend(&buf)
	tool := func(id, name, input string) *AssistantMessage {
		return &AssistantMessage{Message: BetaMessage{
			Content: []ContentBlock{{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)}},
		}}
	}
	started := func(taskID, toolUseID, taskType string) []byte {
		raw, _ := json.Marshal(map[string]any{
			"type": "system", "subtype": "task_started", "task_id": taskID, "tool_use_id": toolUseID, "task_type": taskType,
		})
		return raw
	}
	b.OnAssistant(tool("toolu_agent", "Agent", `{"description":"researcher","prompt":"go","model":"haiku"}`))
	b.OnAssistant(tool("toolu_bash", "Bash", `{"command":"sleep 60","run_in_background":true}`))
	b.OnAssistant(tool("toolu_late", "Agent", `{"description":"not started yet","prompt":"go"}`))
	b.OnSystem("task_started", started("task_a", "toolu_agent", "local_agent"))
	b.OnSystem("task_started", started("task_b", "toolu_bash", taskTypeBash))
	buf.Reset()

	n, err := b.StopSubagents(t.Context())
	if err != nil {
		t.Fatalf("StopSubagents: %v", err)
	}
	if n != 2 {
		t.Fatalf("sent %d stop requests, want 2", n)
	}
	out := buf.String()
	for _, want := range []string{`"subtype":"stop_task","task_id":"task_a"`, `"subtype":"stop_task","task_id":"task_b"`} {
		if !strings.Contains(out, want) {
			t.Errorf("stop_task %s not sent; wire:\n%s", want, out)
		}
	}
	if strings.Contains(out, "interrupt") {
		t.Errorf("an interrupt was sent; stopping subagents must leave the turn alone:\n%s", out)
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
