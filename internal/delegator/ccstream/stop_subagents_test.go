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
	transcript := func(id, model string) []byte {
		return []byte(`{"type":"assistant","timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) +
			`","message":{"id":"` + id + `","model":"` + model + `","stop_reason":"end_turn","usage":{}}}`)
	}
	m := b.subagentTails()
	m.ledgerLine("toolu_agent", transcript("m0", "<synthetic>"))
	if last[0].Model != "haiku" {
		t.Errorf("a <synthetic> record replaced the model: %q", last[0].Model)
	}
	m.ledgerLine("toolu_agent", transcript("m1", "claude-haiku-4-5"))
	if last[0].Model != "claude-haiku-4-5" {
		t.Errorf("model = %q after the transcript named it, want claude-haiku-4-5", last[0].Model)
	}
}

// TestStopTask_StopsOnlyThatTask pins #2901: StopTask stops one running-list
// entry and StopForegroundCommands only the main thread's foreground calls,
// where the kind-wide stops above take everything of a kind.
func TestStopTask_StopsOnlyThatTask(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	b := newTestBackend(&buf)
	tool := func(id, name, input string) *AssistantMessage {
		return &AssistantMessage{Message: BetaMessage{
			Content: []ContentBlock{{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)}},
		}}
	}
	started := func(taskID, toolUseID, taskType string) {
		raw, _ := json.Marshal(map[string]any{
			"type": "system", "subtype": "task_started", "task_id": taskID, "tool_use_id": toolUseID, "task_type": taskType,
		})
		b.OnSystem("task_started", raw)
	}
	b.OnAssistant(tool("toolu_a1", "Agent", `{"description":"one","prompt":"go"}`))
	b.OnAssistant(tool("toolu_a2", "Agent", `{"description":"two","prompt":"go"}`))
	b.OnAssistant(tool("toolu_bg1", "Bash", `{"command":"sleep 60","run_in_background":true}`))
	b.OnAssistant(tool("toolu_bg2", "Bash", `{"command":"sleep 61","run_in_background":true}`))
	b.OnAssistant(tool("toolu_fg", "Bash", `{"command":"make test"}`))
	started("task_a1", "toolu_a1", "local_agent")
	started("task_a2", "toolu_a2", "local_agent")
	started("task_bg1", "toolu_bg1", taskTypeBash)
	started("task_bg2", "toolu_bg2", taskTypeBash)
	started("task_fg", "toolu_fg", taskTypeBash)

	sent := func() []string {
		var ids []string
		for _, line := range strings.Split(buf.String(), "\n") {
			if _, rest, ok := strings.Cut(line, `"subtype":"stop_task","task_id":"`); ok {
				id, _, _ := strings.Cut(rest, `"`)
				ids = append(ids, id)
			}
		}
		buf.Reset()
		return ids
	}
	for _, c := range []struct{ id, want string }{{"toolu_a2", "task_a2"}, {"toolu_bg1", "task_bg1"}} {
		ok, err := b.StopTask(t.Context(), c.id)
		if err != nil || !ok {
			t.Fatalf("StopTask(%s) = %v, %v", c.id, ok, err)
		}
		if got := sent(); len(got) != 1 || got[0] != c.want {
			t.Errorf("StopTask(%s) sent %v, want only %s", c.id, got, c.want)
		}
	}
	if ok, err := b.StopTask(t.Context(), "toolu_gone"); err != nil || ok {
		t.Errorf("StopTask of an unknown id = %v, %v; want false, nil", ok, err)
	}
	if got := sent(); len(got) != 0 {
		t.Errorf("StopTask of an unknown id sent %v", got)
	}
	n, err := b.StopForegroundCommands(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("StopForegroundCommands = %d, %v; want 1", n, err)
	}
	if got := sent(); len(got) != 1 || got[0] != "task_fg" {
		t.Errorf("StopForegroundCommands sent %v, want only task_fg", got)
	}
}
