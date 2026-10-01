package ccstream

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	flog "foci/internal/log"
	"foci/internal/modelinfo"
)

// syncBuffer is a bytes.Buffer safe to read while tail goroutines log into it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// captureDebugLog routes the global event log into a buffer at DEBUG for the
// test's lifetime. Global state, so callers must not be t.Parallel.
func captureDebugLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	flog.SetOutput(buf)
	flog.SetLevel(flog.DEBUG)
	t.Cleanup(func() {
		flog.SetOutput(os.Stderr)
		flog.SetLevel(flog.INFO)
	})
	return buf
}

// TestSubagentHarness_TailsAndRowsFromACCEventSequence is the #1936 harness: the
// REAL handlers, fed the event sequence CC emits for one turn that launches an
// Agent subagent and a run_in_background Bash command, with the subagent's
// transcript on disk. It asserts which tails start, which subagent calls reach
// the ledger, and the decision lines the log narrates for each.
//
// Before this, every question about the tail's behaviour cost a live probe of
// 100+ seconds, and a missing subagent call (#1934) could not be told apart from
// a tail that never started, one on a bad path, or one stopped early. Each of
// those now leaves a named line, and this test pins that they do.
//
// Event shapes: task_started carries tool_use_id + task_id + task_type
// (local_agent / local_bash captured live on CC 2.1.280, #1935);
// task_notification carries the same ids plus a terminal status.
func TestSubagentHarness_TailsAndRowsFromACCEventSequence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		foreground bool
	}{
		{"background agent", false},
		{"foreground agent", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFastTail(t)
			logs := captureDebugLog(t)
			live := withLiveLedger(t)
			home := t.TempDir()
			t.Setenv("HOME", home)

			const (
				agentToolUse = "toolu_harness_agent"
				agentTaskID  = "a1936000000000001"
				bashToolUse  = "toolu_harness_bash"
				bashTaskID   = "b1936xyz"
				subModel     = "claude-sonnet-5"
			)

			b := &Backend{workDir: "/home/foci/clutch"}
			b.sessionID = "19360000-0000-0000-0000-000000000000"
			lg := newCCLedger(b, "harness/s", "harness", nil)
			b.ledger.Store(lg)

			var (
				mu       sync.Mutex
				texts    []string
				result   *delegator.TurnResult
				ends     []string
				subStart []string
			)
			b.AttachSessionEvents(&delegator.SessionEvents{
				OnSubagentText: func(g, text string, _ int) {
					mu.Lock()
					texts = append(texts, g+":"+text)
					mu.Unlock()
				},
				OnSubagentStart: func(g, _, _ string, _ int) { subStart = append(subStart, g) },
				OnSubagentEnd:   func(g string, _ int) { ends = append(ends, g) },
			})
			b.beginTurn(&delegator.TurnEvents{
				TurnID:         "turn-1936",
				OnTurnComplete: func(r *delegator.TurnResult) { result = r },
			})

			// The subagent's transcript: an in-flight line, then the same
			// message COMPLETED (stop_reason set). It is booked once, at its
			// completed figures (#1923).
			path := b.subagentFilePath(agentTaskID, ".jsonl")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			// Billed after the adapter launched: an earlier line is a previous
			// process's, and not booked.
			msg := `{"type":"assistant","isSidechain":true,"timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) + `",` +
				`"message":{"id":"msg_h1","model":"` + subModel + `",` +
				`"usage":{"input_tokens":4,"output_tokens":%OUT%,"cache_read_input_tokens":7000,` +
				`"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":0}},` +
				`"content":[{"type":"text","text":"HARNESS-TEXT"}]%STOP%}}` + "\n"
			inFlight := strings.NewReplacer("%OUT%", "2", "%STOP%", "").Replace(msg)
			done := strings.NewReplacer("%OUT%", "500", "%STOP%", `,"stop_reason":"end_turn"`).Replace(msg)
			if err := os.WriteFile(path, []byte(inFlight+done), 0o644); err != nil {
				t.Fatal(err)
			}

			sys := func(ev TaskEvent) {
				raw, _ := json.Marshal(ev)
				b.OnSystem(ev.Subtype, raw)
			}
			running := func(group string) bool {
				m := b.subagentTails()
				m.mu.Lock()
				defer m.mu.Unlock()
				_, ok := m.tails[group]
				return ok
			}

			// The parent's tool_use blocks, as the stream carries them. The Agent
			// one's run_in_background decides whether the tail forwards text.
			agentInput := `{"description":"harness","prompt":"do it","run_in_background":` +
				map[bool]string{true: "false", false: "true"}[tc.foreground] + `}`
			b.OnAssistant(&AssistantMessage{Message: BetaMessage{Content: []ContentBlock{
				{Type: "tool_use", ID: agentToolUse, Name: "Agent", Input: json.RawMessage(agentInput)},
				{Type: "tool_use", ID: bashToolUse, Name: "Bash", Input: json.RawMessage(`{"command":"sleep 100","run_in_background":true}`)},
			}}})
			sys(TaskEvent{Type: "system", Subtype: "task_started", TaskID: agentTaskID, ToolUseID: agentToolUse, TaskType: "local_agent"})
			sys(TaskEvent{Type: "system", Subtype: "task_started", TaskID: bashTaskID, ToolUseID: bashToolUse, TaskType: taskTypeBash})

			if !running(agentToolUse) {
				t.Fatal("no tail running for the Agent subagent after its task_started")
			}
			if running(bashToolUse) {
				t.Fatal("a tail is running for the background Bash task — it has no transcript (#1935)")
			}

			sys(TaskEvent{Type: "system", Subtype: "task_notification", TaskID: bashTaskID, ToolUseID: bashToolUse, Status: "completed"})
			sys(TaskEvent{Type: "system", Subtype: "task_notification", TaskID: agentTaskID, ToolUseID: agentToolUse, Status: "completed"})
			if running(agentToolUse) {
				t.Fatal("the Agent tail is still running after task_notification:completed")
			}

			b.mu.Lock()
			b.lastModel = "claude-opus-5"
			b.mu.Unlock()
			b.OnResult(&ResultMessage{Subtype: "success", Result: "ok", ModelUsage: map[string]ModelUsage{
				"claude-opus-5": {InputTokens: 10, OutputTokens: 1000, CostUSD: 0.03},
				subModel:        {InputTokens: 4, OutputTokens: 500, CacheReadInputTokens: 7000, CacheCreationInputTokens: 100, CostUSD: 0.01},
			}})

			// --- the ledger: exactly one subagent call, the Agent's, at its COMPLETED figures ---
			if result == nil || result.Usage == nil {
				t.Fatal("no turn result")
			}
			lg.close()
			rows, err := live.Calls(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			var subs []accounting.CallRow
			for _, r := range rows {
				if r.Actor != "" {
					subs = append(subs, r)
				}
			}
			if len(subs) != 1 {
				t.Fatalf("subagent calls = %+v, want exactly one (the Agent's; the Bash task has none)", subs)
			}
			sc := subs[0]
			if sc.Actor != agentToolUse || sc.Model != subModel || sc.TurnID != "turn-1936" {
				t.Errorf("subagent call = {actor=%s model=%s turn=%s}, want {%s %s turn-1936}",
					sc.Actor, sc.Model, sc.TurnID, agentToolUse, subModel)
			}
			if sc.Count(modelinfo.ClassOutput) != 500 || sc.Count(modelinfo.ClassCacheRead) != 7000 || sc.Count(modelinfo.ClassCacheWrite5m) != 100 {
				t.Errorf("subagent call = %+v, want the completed line's (out=500 cr=7000 5m=100)", sc)
			}

			// --- text: forwarded only for a foreground subagent ---
			mu.Lock()
			gotTexts := append([]string(nil), texts...)
			mu.Unlock()
			if tc.foreground && len(gotTexts) == 0 {
				t.Error("foreground subagent's text was not forwarded")
			}
			if !tc.foreground && len(gotTexts) != 0 {
				t.Errorf("background subagent's text was forwarded %v — it already reaches the parent stream", gotTexts)
			}

			// --- chits: only the Agent opens and closes one ---
			if len(subStart) != 1 || subStart[0] != agentToolUse {
				t.Errorf("SubagentStart groups = %v, want [%s]", subStart, agentToolUse)
			}
			if len(ends) != 1 || ends[0] != agentToolUse {
				t.Errorf("SubagentEnd groups = %v, want [%s]", ends, agentToolUse)
			}

			// --- the narration: every decision named, grep-able by group ---
			out := logs.String()
			for _, want := range []string{
				"subagent tail: starting for group=" + agentToolUse,
				"subagent tail: NOT started, task_type=" + taskTypeBash + " is a background Bash task",
				"subagent tail: opened " + path,
				"subagent tail: closed group=" + agentToolUse + " lines=2 terminal=true",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("log lacks %q\n--- log ---\n%s", want, out)
				}
			}
		})
	}
}

// TestSubagentTail_FinalizeWithoutATailIsNamed: a task_notification for a group
// whose tail never started (or already ended) must not read like one that
// stopped a tail (#1936).
func TestSubagentTail_FinalizeWithoutATailIsNamed(t *testing.T) {
	logs := captureDebugLog(t)
	m := newSubagentTailManager(nil, nil)
	m.finalize("toolu_never")
	if want := "subagent tail: finalize found no running tail for group=toolu_never"; !strings.Contains(logs.String(), want) {
		t.Errorf("log lacks %q\n--- log ---\n%s", want, logs.String())
	}
}
