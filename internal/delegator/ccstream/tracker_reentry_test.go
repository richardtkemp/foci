package ccstream

import (
	"encoding/json"
	"testing"
	"time"

	"foci/internal/delegator"
)

// TestTaskNotification_StatusCallbackMayReadAwaiting is the backend half of
// #3169: a task_notification retires a background task, the tracker reports
// the new running list, and the receiver (the app hub, through the delegated
// manager) asks this same backend whether it is awaiting an autonomous run.
// That read takes the tracker's lock; the tracker held it while it ran the
// callback, so the reader goroutine deadlocked on itself and every later
// stream event and app frame for the gateway stalled behind it.
func TestTaskNotification_StatusCallbackMayReadAwaiting(t *testing.T) {
	t.Parallel()

	b := &Backend{}
	applyHandler(b, &testHandler{})
	b.OnAssistant(&AssistantMessage{Message: BetaMessage{
		Content: []ContentBlock{{Type: "tool_use", ID: "bg1", Name: "Bash",
			Input: json.RawMessage(`{"command":"sleep 60","run_in_background":true}`)}},
	}})
	if n := b.agents.Pending(); n != 1 {
		t.Fatalf("setup: Pending() = %d, want 1", n)
	}

	awaited := make(chan bool, 4)
	b.SetOnSubagentRunning(func([]delegator.RunningSubagent) { awaited <- b.AwaitingAutonomousRun() })
	b.SetOnSubagentStatus(func(string) { awaited <- b.AwaitingAutonomousRun() })

	raw, _ := json.Marshal(TaskEvent{Subtype: "task_notification", Status: "completed", TaskID: "bg1abcd", ToolUseID: "bg1"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.OnSystem("task_notification", raw)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("OnSystem(task_notification) did not return: the status callback's AwaitingAutonomousRun deadlocked on the tracker lock")
	}
	for range 2 {
		if <-awaited {
			t.Error("AwaitingAutonomousRun() = true inside the callback for the last task's end, want false")
		}
	}
}
