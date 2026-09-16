package ccstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestOnSystem_TaskStarted_StartsTheTailForAnAgent drives the HANDLER, not the
// tail manager.
//
// The pre-existing tail tests call mgr.maybeStart directly, so they build the
// very precondition under test and cannot fail when the handler never calls it
// (#1934). This one feeds a real system/task_started event — the shape captured
// live from CC 2.1.261, carrying BOTH tool_use_id and task_id — and asserts a
// tail is actually running for the group afterwards.
func TestOnSystem_TaskStarted_StartsTheTailForAnAgent(t *testing.T) {
	withFastTail(t)

	home := t.TempDir()
	t.Setenv("HOME", home)

	const (
		toolUseID = "toolu_agenttail"
		agentID   = "a73209ebdb865f004"
		sessionID = "6bd15e3c-0000-0000-0000-000000000000"
		workDir   = "/home/foci/clutch"
	)

	b := &Backend{workDir: workDir}
	b.sessionID = sessionID

	// The transcript CC writes for a real Agent subagent.
	path := b.subagentFilePath(agentID, ".jsonl")
	if path == "" {
		t.Fatal("subagentFilePath returned empty — test cannot exercise the tail")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(assistantLine("SUBAGENT-TEXT")), 0o644); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(TaskEvent{
		Type:      "system",
		Subtype:   "task_started",
		TaskID:    agentID,
		ToolUseID: toolUseID,
	})
	b.OnSystem("task_started", raw)

	m := b.subagentTails()
	waitFor(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		_, running := m.tails[toolUseID]
		return running
	})
	m.finalize(toolUseID)
}

// TestOnSystem_TaskStarted_NoTailWithoutASessionID pins the branch that is
// silent in production: subagentFilePath returns "" when the parent session id
// is not yet known, so no tail starts and no usage is ever collected for that
// subagent. Before #1934 this produced no log line of any kind, which is why
// "the tail did not start" and "no subagent ran" were indistinguishable.
func TestOnSystem_TaskStarted_NoTailWithoutASessionID(t *testing.T) {
	withFastTail(t)
	t.Setenv("HOME", t.TempDir())

	const toolUseID = "toolu_nosession"

	b := &Backend{workDir: "/home/foci/clutch"} // sessionID deliberately unset

	if p := b.subagentFilePath("a73209ebdb865f004", ".jsonl"); p != "" {
		t.Fatalf("expected empty path without a session id, got %q", p)
	}

	raw, _ := json.Marshal(TaskEvent{
		Type:      "system",
		Subtype:   "task_started",
		TaskID:    "a73209ebdb865f004",
		ToolUseID: toolUseID,
	})
	b.OnSystem("task_started", raw)

	m := b.subagentTails()
	m.mu.Lock()
	_, running := m.tails[toolUseID]
	m.mu.Unlock()
	if running {
		t.Fatal("a tail was started with no transcript path — it can never open a file")
	}
}
