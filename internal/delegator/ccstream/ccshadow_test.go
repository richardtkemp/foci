package ccstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/modelinfo"
)

// TestShadowWiring drives the Backend's own hooks: the stream names a
// main-thread call, CC appends its line to the main transcript, a result
// arrives, and the process exits. The call lands in the SHADOW ledger — and
// nowhere else: no observer (api.jsonl, trace generation) sees a shadow
// booking, and the live ledger is untouched. Not parallel: it sets the
// process's shadow ledger.
func TestShadowWiring(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-shadow.db")
	shadow, _, err := accounting.Open(path, accounting.Options{Shadow: true, NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	accounting.SetShadow(shadow)
	t.Cleanup(func() { accounting.SetShadow(nil); _ = shadow.Close() })
	accounting.BookedHook = func(b accounting.Booking) { t.Errorf("a shadow booking reached the observers: %+v", b.Call) }
	t.Cleanup(func() { accounting.BookedHook = nil })

	b := newTestBackend(&bytes.Buffer{})
	sh := newCCShadow(b, "cap/c1", "cap", nil)
	b.shadow.Store(sh)
	transcript := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"assistant","message":{"id":"old","model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":9}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(transcript)
	sh.startMainTail(transcript, fi.Size()) // a resumed process: the old line is an earlier process's

	b.turnMu.Lock()
	b.turnRowID = "cap/c1@1"
	b.turnMu.Unlock()
	stop := "end_turn"
	b.OnAssistant(&AssistantMessage{Type: "assistant", Message: BetaMessage{ID: "msg_1", Model: "claude-opus-5",
		StopReason: &stop, Usage: TokenUsage{InputTokens: 10, OutputTokens: 20}}})
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"assistant","timestamp":"2026-09-29T10:00:00Z","message":{"id":"msg_1","model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":20}}}` + "\n")
	_ = f.Close()
	b.OnResult(&ResultMessage{Type: "result", ModelUsage: map[string]ModelUsage{"claude-opus-5": {InputTokens: 10, OutputTokens: 20, CostUSD: 0.01}}})
	sh.close()

	rows, err := shadow.Calls(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TurnID != "cap/c1@1" || rows[0].Count(modelinfo.ClassOutput) != 20 {
		t.Fatalf("shadow calls = %+v, want msg_1 on cap/c1@1", rows)
	}
	if accounting.Live() != nil {
		t.Error("the test must not have a live ledger")
	}
}

// TestShadowWorkflowAgentsBookOnInvokingTurn is #2130 defect 1. A Workflow
// run's agents write their transcripts under subagents/workflows/<run id>/,
// not subagents/, and the stream announces the whole run with one
// task_started. Their calls must be booked from those transcripts, each on
// the turn that invoked the Workflow (the #2111 ruling: subagent spend is
// tied to its invoking turn), with their observed 5m cache writes, even when
// an agent's transcript appears after that turn has ended. A result that
// arrives mid-run must not book the run's spend as a turn-less remainder, and
// the run's journal.jsonl is not an agent transcript.
func TestShadowWorkflowAgentsBookOnInvokingTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-shadow.db")
	shadow, _, err := accounting.Open(path, accounting.Options{Shadow: true, NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	accounting.SetShadow(shadow)
	t.Cleanup(func() { accounting.SetShadow(nil); _ = shadow.Close() })

	b := newTestBackend(&bytes.Buffer{})
	b.hookInstallID = "install-a"
	sh := newCCShadow(b, "cap/c1", "cap", nil)
	b.shadow.Store(sh)
	b.turnMu.Lock()
	b.turnRowID = "cap/c1@1"
	b.turnMu.Unlock()

	dir := filepath.Join(t.TempDir(), "subagents", "workflows", "wf_0bd537f1-cdd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	agentLine := func(id, model string, out, w5m int) string {
		return fmt.Sprintf(`{"type":"assistant","timestamp":"%s","message":{"id":%q,"model":%q,"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":0}}}}`+"\n",
			time.Now().UTC().Format(time.RFC3339Nano), id, model, out, w5m, w5m)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("agent-a1.jsonl", agentLine("msg_a1", "claude-sonnet-5", 40, 1000))
	write("agent-a1.meta.json", `{"agentType":"workflow-subagent"}`)
	write("journal.jsonl", agentLine("msg_journal", "claude-sonnet-5", 1, 1)) // not an agent: never booked

	// The Workflow tool resolves at launch; its PostToolUse carries the run.
	resp, _ := json.Marshal(map[string]string{"status": "async_launched", "taskId": "wd6wda9q7",
		"taskType": "local_workflow", "runId": "wf_0bd537f1-cdd", "transcriptDir": dir})
	stdout, _ := json.Marshal(hookScriptOutput{HookEvent: "PostToolUse", InstallID: "install-a",
		ToolUseID: "toolu_wf", ToolName: "Workflow", ToolResponse: string(resp)})
	env, _ := json.Marshal(hookResponseEnvelope{HookEvent: "PostToolUse", Stdout: string(stdout)})
	b.handleHookResponse(env)
	started, _ := json.Marshal(TaskEvent{Subtype: "task_started", TaskID: "wd6wda9q7", ToolUseID: "toolu_wf", TaskType: "local_workflow"})
	b.OnSystem("task_started", started)

	// The invoking turn ends while the run carries on; a result lands mid-run.
	b.turnMu.Lock()
	b.turnRowID = "cap/c1@2"
	b.turnMu.Unlock()
	sonnet := ModelUsage{InputTokens: 3, OutputTokens: 40, CacheCreationInputTokens: 1000}
	b.OnResult(&ResultMessage{Type: "result", ModelUsage: map[string]ModelUsage{"claude-sonnet-5": sonnet}})
	write("agent-a2.jsonl", agentLine("msg_a2", "claude-opus-5", 70, 2000))

	done, _ := json.Marshal(TaskEvent{Subtype: "task_notification", Status: "completed", TaskID: "wd6wda9q7", ToolUseID: "toolu_wf"})
	b.OnSystem("task_notification", done)
	b.OnResult(&ResultMessage{Type: "result", ModelUsage: map[string]ModelUsage{"claude-sonnet-5": sonnet,
		"claude-opus-5": {InputTokens: 3, OutputTokens: 70, CacheCreationInputTokens: 2000}}})
	b.subagentTails().stopAll()
	sh.close()

	rows, err := shadow.Calls(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("rows = %d, want the two agents' calls and nothing else (no remainder, no journal line)", len(rows))
	}
	want := map[string]int{"claude-sonnet-5": 1000, "claude-opus-5": 2000}
	for _, r := range rows {
		if r.Kind != accounting.KindCall {
			t.Errorf("%s row kind=%s turn=%q: the run's spend fell into a remainder", r.Model, r.Kind, r.TurnID)
			continue
		}
		if r.TurnID != "cap/c1@1" || r.Actor == "" || r.Count(modelinfo.ClassCacheWrite5m) != want[r.Model] {
			t.Errorf("%s = turn %q actor %q 5m %d, want the invoking turn cap/c1@1, a workflow actor, %d 5m writes",
				r.Model, r.TurnID, r.Actor, r.Count(modelinfo.ClassCacheWrite5m), want[r.Model])
		}
	}
}
