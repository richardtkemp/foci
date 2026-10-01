package ccstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"foci/internal/modelinfo"
)

// resumeCompletedLine is a transcript assistant record carrying billable usage: an
// id to dedup on and a terminal stop_reason, so the ledger books it completed.
func resumeCompletedLine(id string, out int) string {
	b, _ := json.Marshal(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"id":          id,
			"model":       "claude-sonnet-5",
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 1, "output_tokens": out},
			"content":     []any{},
		},
	})
	return string(b) + "\n"
}

func resumeTaskEvent(t *testing.T, b *Backend, subtype, taskID, toolUseID string) {
	t.Helper()
	ev := TaskEvent{Type: "system", Subtype: subtype, TaskID: taskID, ToolUseID: toolUseID, TaskType: "local_agent"}
	if subtype == "task_notification" {
		ev.Status = "completed"
	}
	raw, _ := json.Marshal(ev)
	b.OnSystem(subtype, raw)
}

func resumeTailRunning(m *subagentTailManager, key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.tails[key]
	return ok
}

// resumeFixture is a backend whose subagent transcript for taskID exists, with
// run 1's completed record already in it.
func resumeFixture(t *testing.T, taskID string) (*Backend, string) {
	t.Helper()
	withFastTail(t)
	t.Setenv("HOME", t.TempDir())
	b := &Backend{workDir: "/home/foci/clutch"}
	b.sessionID = "6bd15e3c-0000-0000-0000-000000000000"
	path := b.subagentTranscriptPath(taskID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(resumeCompletedLine("msg_run1", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.subagentTails().stopAll)
	return b, path
}

func resumeAppendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
}

// TestSendMessageResume_TailAndRowKeyedByAgentID is #2056. A SendMessage
// resume's task_started and task_notification carry the SendMessage tool_use
// id, not the Agent's. The tail was started under that id, so run 2's calls
// were booked with the SendMessage id as their actor (#1946 says the actor
// names the subagent), and the resume's notification, which finalizes the
// RESOLVED group key, never stopped that tail.
func TestSendMessageResume_TailAndRowKeyedByAgentID(t *testing.T) {
	const (
		agentTU = "toolu_agent_orig"
		sendTU  = "toolu_sendmessage"
		taskID  = "a9b4ab879cb6f02db"
	)
	b, path := resumeFixture(t, taskID)
	b.setAgentLabel(agentTU, "worker")
	m := b.subagentTails()
	// What reaches the ledger, per group and call: a reactivation tail re-reads
	// run 1's lines, which the adapter books once per id.
	var seenMu sync.Mutex
	seen := map[string]map[string]int{}
	feed := m.ledgerLine
	m.ledgerLine = func(group string, raw []byte) {
		if l, _ := parseCCRecord(raw); l != nil && l.complete {
			seenMu.Lock()
			if seen[group] == nil {
				seen[group] = map[string]int{}
			}
			seen[group][l.id] = l.tokens[modelinfo.ClassOutput]
			seenMu.Unlock()
		}
		feed(group, raw)
	}

	// Run 1: the Agent spawn.
	resumeTaskEvent(t, b, "task_started", taskID, agentTU)
	if !resumeTailRunning(m, agentTU) {
		t.Fatal("run 1: no tail under the Agent id")
	}
	resumeTaskEvent(t, b, "task_notification", taskID, agentTU)

	// Run 2: a SendMessage resume of the same task.
	resumeAppendLine(t, path, resumeCompletedLine("msg_run2", 200))
	resumeTaskEvent(t, b, "task_started", taskID, sendTU)
	if resumeTailRunning(m, sendTU) {
		t.Errorf("run 2: tail keyed by the SendMessage id %s, want the Agent id %s", sendTU, agentTU)
	}
	if !resumeTailRunning(m, agentTU) {
		t.Errorf("run 2: no tail under the Agent id %s", agentTU)
	}
	resumeTaskEvent(t, b, "task_notification", taskID, sendTU)
	if resumeTailRunning(m, agentTU) || resumeTailRunning(m, sendTU) {
		t.Error("run 2's task_notification left its tail running")
	}

	seenMu.Lock()
	defer seenMu.Unlock()
	if _, ok := seen[sendTU]; ok {
		t.Errorf("calls handed to the ledger under the SendMessage id: %+v", seen)
	}
	out := 0
	for _, n := range seen[agentTU] {
		out += n
	}
	if out != 300 {
		t.Errorf("calls under the Agent id = %+v (all keys %+v), want both runs' output 300", seen[agentTU], seen)
	}
}

// TestSendMessageResume_NestedTailKeyedByItsAgentID: the same for a nested
// (depth >= 2) subagent, whose own Agent id is its key (#1554).
func TestSendMessageResume_NestedTailKeyedByItsAgentID(t *testing.T) {
	const (
		nestedTU = "toolu_nested_agent"
		sendTU   = "toolu_nested_send"
		taskID   = "anested000000001"
	)
	b, path := resumeFixture(t, taskID)
	b.registerNestedAgent(nestedTU, "toolu_spawner")
	m := b.subagentTails()

	resumeTaskEvent(t, b, "task_started", taskID, nestedTU)
	if !resumeTailRunning(m, nestedTU) {
		t.Fatal("run 1: no tail under the nested Agent id")
	}
	resumeTaskEvent(t, b, "task_notification", taskID, nestedTU)

	resumeAppendLine(t, path, resumeCompletedLine("msg_run2", 200))
	resumeTaskEvent(t, b, "task_started", taskID, sendTU)
	if resumeTailRunning(m, sendTU) || !resumeTailRunning(m, nestedTU) {
		t.Errorf("run 2: tail not keyed by the nested Agent id (send=%v nested=%v)",
			resumeTailRunning(m, sendTU), resumeTailRunning(m, nestedTU))
	}
	resumeTaskEvent(t, b, "task_notification", taskID, sendTU)
	if resumeTailRunning(m, nestedTU) || resumeTailRunning(m, sendTU) {
		t.Error("run 2's task_notification left its tail running")
	}
}
