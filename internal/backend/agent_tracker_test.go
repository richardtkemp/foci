package backend

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// --- AgentTracker tests ---

// TestAgentTracker_Add verifies that adding an agent fires a running
// status message with the description.
func TestAgentTracker_Add(t *testing.T) {
	t.Parallel()
	var got string
	tr := &AgentTracker{OnStatus: func(text string) { got = text }}
	tr.Add("ag1", "search files")

	if tr.Pending() != 1 {
		t.Fatalf("Pending() = %d, want 1", tr.Pending())
	}
	want := "🔄 1 agent(s) running: search files"
	if got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
}

// TestAgentTracker_AddDuplicate verifies that adding the same ID twice
// is a no-op.
func TestAgentTracker_AddDuplicate(t *testing.T) {
	t.Parallel()
	var calls int
	tr := &AgentTracker{OnStatus: func(string) { calls++ }}
	tr.Add("ag1", "task")
	tr.Add("ag1", "task") // duplicate

	if tr.Pending() != 1 {
		t.Fatalf("Pending() = %d, want 1 (duplicate should be ignored)", tr.Pending())
	}
	if calls != 1 {
		t.Errorf("OnStatus called %d times, want 1", calls)
	}
}

// TestAgentTracker_AddMultiple verifies accumulation of multiple agents.
func TestAgentTracker_AddMultiple(t *testing.T) {
	t.Parallel()
	var got string
	tr := &AgentTracker{OnStatus: func(text string) { got = text }}
	tr.Add("ag1", "search files")
	tr.Add("ag2", "run tests")

	if tr.Pending() != 2 {
		t.Fatalf("Pending() = %d, want 2", tr.Pending())
	}
	want := "🔄 2 agent(s) running: search files, run tests"
	if got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
}

// TestAgentTracker_AddNoDescriptions verifies the fallback message
// when agents have empty descriptions.
func TestAgentTracker_AddNoDescriptions(t *testing.T) {
	t.Parallel()
	var got string
	tr := &AgentTracker{OnStatus: func(text string) { got = text }}
	tr.Add("ag1", "")
	tr.Add("ag2", "")

	want := fmt.Sprintf("🔄 %d agent(s) running", 2)
	if got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
}

// TestAgentTracker_Remove verifies that removing by ID works and fires
// an updated status.
func TestAgentTracker_Remove(t *testing.T) {
	t.Parallel()
	var got string
	tr := &AgentTracker{OnStatus: func(text string) { got = text }}
	tr.Add("ag1", "task A")
	tr.Add("ag2", "task B")

	if !tr.Remove("ag1") {
		t.Fatal("Remove returned false for known ID")
	}
	if tr.Pending() != 1 {
		t.Fatalf("Pending() = %d, want 1", tr.Pending())
	}
	want := "🔄 1 agent(s) running: task B"
	if got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
}

// TestAgentTracker_RemoveUnknown verifies that removing an unknown ID
// returns false and doesn't fire status.
func TestAgentTracker_RemoveUnknown(t *testing.T) {
	t.Parallel()
	statusCalled := false
	tr := &AgentTracker{OnStatus: func(string) { statusCalled = true }}
	tr.Add("ag1", "task")
	statusCalled = false

	if tr.Remove("unknown") {
		t.Error("Remove returned true for unknown ID")
	}
	if statusCalled {
		t.Error("OnStatus should not be called for unmatched Remove")
	}
}

// TestAgentTracker_RemoveLast verifies that removing the last agent
// fires a completion message.
func TestAgentTracker_RemoveLast(t *testing.T) {
	t.Parallel()
	var got string
	tr := &AgentTracker{OnStatus: func(text string) { got = text }}
	tr.Add("ag1", "task")
	tr.Remove("ag1")

	if tr.Pending() != 0 {
		t.Fatalf("Pending() = %d, want 0", tr.Pending())
	}
	if !strings.Contains(got, "complete") {
		t.Errorf("expected completion message, got %q", got)
	}
}

// TestAgentTracker_RemoveOne verifies that RemoveOne removes the first
// pending agent.
func TestAgentTracker_RemoveOne(t *testing.T) {
	t.Parallel()
	var got string
	tr := &AgentTracker{OnStatus: func(text string) { got = text }}
	tr.Add("ag1", "task A")
	tr.Add("ag2", "task B")

	if !tr.RemoveOne() {
		t.Fatal("RemoveOne returned false with pending agents")
	}
	if tr.Pending() != 1 {
		t.Fatalf("Pending() = %d, want 1", tr.Pending())
	}
	want := "🔄 1 agent(s) running: task B"
	if got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
}

// TestAgentTracker_RemoveOneEmpty verifies that RemoveOne is a safe
// no-op when there are no pending agents.
func TestAgentTracker_RemoveOneEmpty(t *testing.T) {
	t.Parallel()
	tr := &AgentTracker{}
	if tr.RemoveOne() {
		t.Error("RemoveOne returned true with no pending agents")
	}
}

// TestAgentTracker_ClearAll verifies that ClearAll removes all agents
// and fires a completion message.
func TestAgentTracker_ClearAll(t *testing.T) {
	t.Parallel()
	var got string
	tr := &AgentTracker{OnStatus: func(text string) { got = text }}
	tr.Add("ag1", "task A")
	tr.Add("ag2", "task B")

	tr.ClearAll()

	if tr.Pending() != 0 {
		t.Fatalf("Pending() = %d, want 0", tr.Pending())
	}
	if !strings.Contains(got, "complete") {
		t.Errorf("expected completion message, got %q", got)
	}
}

// TestAgentTracker_ClearAllEmpty verifies ClearAll is a safe no-op
// when there are no pending agents.
func TestAgentTracker_ClearAllEmpty(t *testing.T) {
	t.Parallel()
	statusCalled := false
	tr := &AgentTracker{OnStatus: func(string) { statusCalled = true }}
	tr.ClearAll()

	if statusCalled {
		t.Error("OnStatus should not be called when ClearAll has nothing to clear")
	}
}

// TestAgentTracker_NilCallback verifies that all operations are safe
// when OnStatus is nil.
func TestAgentTracker_NilCallback(t *testing.T) {
	t.Parallel()
	tr := &AgentTracker{} // OnStatus is nil
	tr.Add("ag1", "task")
	tr.Remove("ag1")
	tr.Add("ag2", "task")
	tr.RemoveOne()
	tr.Add("ag3", "task")
	tr.ClearAll()
	// Should not panic.
}

// --- ExtractAgentDescription tests ---

// TestExtractAgentDescription verifies extraction of the description
// field from Agent tool_use input JSON.
func TestExtractAgentDescription(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input json.RawMessage
		want  string
	}{
		{
			name:  "valid description",
			input: json.RawMessage(`{"description":"search for patterns"}`),
			want:  "search for patterns",
		},
		{
			name:  "empty description",
			input: json.RawMessage(`{"description":""}`),
			want:  "",
		},
		{
			name:  "no description field",
			input: json.RawMessage(`{"task":"do something"}`),
			want:  "",
		},
		{
			name:  "invalid JSON",
			input: json.RawMessage(`not json`),
			want:  "",
		},
		{
			name:  "null input",
			input: nil,
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ExtractAgentDescription(tt.input)
			if got != tt.want {
				t.Errorf("ExtractAgentDescription = %q, want %q", got, tt.want)
			}
		})
	}
}
