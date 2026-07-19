package opencode

import (
	"encoding/json"
	"testing"

	"foci/internal/delegator"
)

func TestHandleToolPart_TaskTool_FiresSubagentLifecycle(t *testing.T) {
	var starts, ends []string
	var agentPending int

	b := &Backend{
		sessionID:   "sess-test",
		readyCh:     make(chan struct{}),
		outstanding: delegator.NewOutstandingRegistry(),
	}
	b.agents.OnStatus = func(detail string) {
		if detail == "" {
			agentPending = 0
		} else {
			agentPending = 1
		}
	}
	b.AttachSessionEvents(&delegator.SessionEvents{
		OnSubagentStart: func(groupKey, label, prompt string, runIndex int) { starts = append(starts, groupKey+":"+label) },
		OnSubagentEnd:   func(groupKey string, runIndex int) { ends = append(ends, groupKey) },
	})

	// Simulate task tool start
	inputJSON, _ := json.Marshal(map[string]string{
		"description": "fix the bug",
		"prompt":      "find and fix the null pointer",
	})
	b.handleToolPart(Part{
		CallID: "task-1",
		Tool:   taskTool,
		State: &ToolState{
			Status: ToolStateRunning,
			Input:  inputJSON,
		},
	})

	if len(starts) != 1 {
		t.Fatalf("expected 1 OnSubagentStart, got %d", len(starts))
	}
	if starts[0] != "task-1:fix the bug" {
		t.Errorf("OnSubagentStart = %q, want %q", starts[0], "task-1:fix the bug")
	}
	if agentPending != 1 {
		t.Errorf("expected agentPending=1 after start, got %d", agentPending)
	}

	// Simulate task tool completion
	b.handleToolPart(Part{
		CallID: "task-1",
		Tool:   taskTool,
		State: &ToolState{
			Status: ToolStateCompleted,
			Output: `"done"`,
		},
	})

	if len(ends) != 1 {
		t.Fatalf("expected 1 OnSubagentEnd, got %d", len(ends))
	}
	if ends[0] != "task-1" {
		t.Errorf("OnSubagentEnd = %q, want %q", ends[0], "task-1")
	}
	if agentPending != 0 {
		t.Errorf("expected agentPending=0 after end, got %d", agentPending)
	}
}

func TestHandleToolPart_TaskToolError_FiresSubagentEnd(t *testing.T) {
	var ends []string

	b := &Backend{
		sessionID:   "sess-test",
		readyCh:     make(chan struct{}),
		outstanding: delegator.NewOutstandingRegistry(),
	}
	b.AttachSessionEvents(&delegator.SessionEvents{
		OnSubagentStart: func(groupKey, label, prompt string, runIndex int) {},
		OnSubagentEnd:   func(groupKey string, runIndex int) { ends = append(ends, groupKey) },
	})

	// Start
	b.handleToolPart(Part{
		CallID: "task-err",
		Tool:   taskTool,
		State: &ToolState{
			Status: ToolStateRunning,
			Input:  json.RawMessage(`{"description":"will fail"}`),
		},
	})

	// Error
	b.handleToolPart(Part{
		CallID: "task-err",
		Tool:   taskTool,
		State: &ToolState{
			Status: ToolStateError,
			Error:  "agent not found",
		},
	})

	if len(ends) != 1 {
		t.Fatalf("expected 1 OnSubagentEnd on error, got %d", len(ends))
	}
	if ends[0] != "task-err" {
		t.Errorf("OnSubagentEnd = %q, want %q", ends[0], "task-err")
	}
}

func TestHandleToolPart_NonTaskTool_DoesNotFireSubagentEvents(t *testing.T) {
	var starts, ends []string

	b := &Backend{
		sessionID:   "sess-test",
		readyCh:     make(chan struct{}),
		outstanding: delegator.NewOutstandingRegistry(),
	}
	b.AttachSessionEvents(&delegator.SessionEvents{
		OnSubagentStart: func(groupKey, label, prompt string, runIndex int) { starts = append(starts, groupKey) },
		OnSubagentEnd:   func(groupKey string, runIndex int) { ends = append(ends, groupKey) },
	})

	// Regular bash tool — should NOT fire subagent events
	b.handleToolPart(Part{
		CallID: "bash-1",
		Tool:   "bash",
		State: &ToolState{
			Status: ToolStateRunning,
			Input:  json.RawMessage(`{"command":"ls"}`),
		},
	})
	b.handleToolPart(Part{
		CallID: "bash-1",
		Tool:   "bash",
		State: &ToolState{
			Status: ToolStateCompleted,
			Output: `"output"`,
		},
	})

	if len(starts) != 0 {
		t.Errorf("expected 0 OnSubagentStart for non-task tool, got %d", len(starts))
	}
	if len(ends) != 0 {
		t.Errorf("expected 0 OnSubagentEnd for non-task tool, got %d", len(ends))
	}
	if b.agents.Pending() != 0 {
		t.Errorf("expected 0 pending agents, got %d", b.agents.Pending())
	}
}
