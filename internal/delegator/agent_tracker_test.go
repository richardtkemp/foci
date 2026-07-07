package delegator

import (
	"encoding/json"
	"testing"
	"time"
)

// --- SubagentTracker tests ---

// TestSubagentTracker_Add verifies that adding an agent fires a running
// status message with the description.
func TestSubagentTracker_Add(t *testing.T) {
	t.Parallel()
	var got string
	tr := &SubagentTracker{OnStatus: func(text string) { got = text }}
	tr.Add("ag1", "search files")

	if tr.Pending() != 1 {
		t.Fatalf("Pending() = %d, want 1", tr.Pending())
	}
	want := "search files"
	if got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
}

// TestSubagentTracker_AddDuplicate verifies that adding the same ID twice
// is a no-op.
func TestSubagentTracker_AddDuplicate(t *testing.T) {
	t.Parallel()
	var calls int
	tr := &SubagentTracker{OnStatus: func(string) { calls++ }}
	tr.Add("ag1", "task")
	tr.Add("ag1", "task") // duplicate

	if tr.Pending() != 1 {
		t.Fatalf("Pending() = %d, want 1 (duplicate should be ignored)", tr.Pending())
	}
	if calls != 1 {
		t.Errorf("OnStatus called %d times, want 1", calls)
	}
}

// TestSubagentTracker_AddMultiple verifies accumulation of multiple agents.
func TestSubagentTracker_AddMultiple(t *testing.T) {
	t.Parallel()
	var got string
	tr := &SubagentTracker{OnStatus: func(text string) { got = text }}
	tr.Add("ag1", "search files")
	tr.Add("ag2", "run tests")

	if tr.Pending() != 2 {
		t.Fatalf("Pending() = %d, want 2", tr.Pending())
	}
	want := "search files, run tests"
	if got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
}

// TestSubagentTracker_AddNoDescriptions verifies the fallback message
// when agents have empty descriptions.
func TestSubagentTracker_AddNoDescriptions(t *testing.T) {
	t.Parallel()
	var got string
	tr := &SubagentTracker{OnStatus: func(text string) { got = text }}
	tr.Add("ag1", "")
	tr.Add("ag2", "")

	want := "2 subagent(s) running"
	if got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
}

// TestSubagentTracker_Remove verifies that removing by ID works and fires
// an updated status.
func TestSubagentTracker_Remove(t *testing.T) {
	t.Parallel()
	var got string
	tr := &SubagentTracker{OnStatus: func(text string) { got = text }}
	tr.Add("ag1", "task A")
	tr.Add("ag2", "task B")

	if !tr.Remove("ag1") {
		t.Fatal("Remove returned false for known ID")
	}
	if tr.Pending() != 1 {
		t.Fatalf("Pending() = %d, want 1", tr.Pending())
	}
	want := "task B"
	if got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
}

// TestSubagentTracker_RemoveUnknown verifies that removing an unknown ID
// returns false and doesn't fire status.
func TestSubagentTracker_RemoveUnknown(t *testing.T) {
	t.Parallel()
	statusCalled := false
	tr := &SubagentTracker{OnStatus: func(string) { statusCalled = true }}
	tr.Add("ag1", "task")
	statusCalled = false

	if tr.Remove("unknown") {
		t.Error("Remove returned true for unknown ID")
	}
	if statusCalled {
		t.Error("OnStatus should not be called for unmatched Remove")
	}
}

// TestSubagentTracker_RemoveLast verifies that removing the last agent
// fires a completion message.
func TestSubagentTracker_RemoveLast(t *testing.T) {
	t.Parallel()
	var got string
	tr := &SubagentTracker{OnStatus: func(text string) { got = text }}
	tr.Add("ag1", "task")
	tr.Remove("ag1")

	if tr.Pending() != 0 {
		t.Fatalf("Pending() = %d, want 0", tr.Pending())
	}
	if got != "" {
		t.Errorf("expected empty completion detail, got %q", got)
	}
}

// TestSubagentTracker_RemoveOne verifies that RemoveOne removes the first
// pending agent.
func TestSubagentTracker_RemoveOne(t *testing.T) {
	t.Parallel()
	var got string
	tr := &SubagentTracker{OnStatus: func(text string) { got = text }}
	tr.Add("ag1", "task A")
	tr.Add("ag2", "task B")

	if !tr.RemoveOne() {
		t.Fatal("RemoveOne returned false with pending agents")
	}
	if tr.Pending() != 1 {
		t.Fatalf("Pending() = %d, want 1", tr.Pending())
	}
	want := "task B"
	if got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
}

// TestSubagentTracker_RemoveOneEmpty verifies that RemoveOne is a safe
// no-op when there are no pending agents.
func TestSubagentTracker_RemoveOneEmpty(t *testing.T) {
	t.Parallel()
	tr := &SubagentTracker{}
	if tr.RemoveOne() {
		t.Error("RemoveOne returned true with no pending agents")
	}
}

// TestSubagentTracker_PruneExpired verifies a spawn with no completion signal is
// dropped after defaultAgentMaxAge, so Pending() can't stay stuck > 0 now that the
// tracker survives turn boundaries.
func TestSubagentTracker_PruneExpired(t *testing.T) {
	t.Parallel()
	tr := &SubagentTracker{}
	tr.pending = []TrackedSubagent{
		{ID: "stale", added: time.Now().Add(-defaultAgentMaxAge - time.Minute)},
		{ID: "live", added: time.Now()},
	}
	if n := tr.Pending(); n != 1 {
		t.Fatalf("Pending() = %d, want 1 (stale spawn pruned)", n)
	}
}

// TestSubagentTracker_MaxAgeConfigurable verifies a custom MaxAge overrides the
// 30m default — the unwedge backstop is tunable for long background jobs
// ([cc_backend].background_task_max_age).
func TestSubagentTracker_MaxAgeConfigurable(t *testing.T) {
	t.Parallel()
	tr := &SubagentTracker{MaxAge: time.Minute}
	tr.pending = []TrackedSubagent{
		{ID: "stale", added: time.Now().Add(-2 * time.Minute)}, // older than custom 1m
		{ID: "live", added: time.Now()},
	}
	if n := tr.Pending(); n != 1 {
		t.Fatalf("Pending() = %d, want 1 (spawn older than MaxAge=1m pruned)", n)
	}
	// A spawn younger than the custom MaxAge but older than nothing survives.
	tr2 := &SubagentTracker{MaxAge: time.Hour}
	tr2.pending = []TrackedSubagent{
		{ID: "old-but-within", added: time.Now().Add(-40 * time.Minute)}, // pruned at 30m default, kept at 1h
	}
	if n := tr2.Pending(); n != 1 {
		t.Fatalf("Pending() = %d, want 1 (40m spawn kept under MaxAge=1h)", n)
	}
}

// TestExtractBashBackground verifies detection of CC's run_in_background Bash flag.
func TestExtractBashBackground(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"backgrounded", `{"command":"sleep 60","run_in_background":true}`, true},
		{"foreground explicit", `{"command":"ls","run_in_background":false}`, false},
		{"flag absent", `{"command":"ls"}`, false},
		{"malformed", `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractBashBackground(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("ExtractBashBackground(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestSubagentTracker_ClearAll verifies that ClearAll removes all agents
// and fires a completion message.
func TestSubagentTracker_ClearAll(t *testing.T) {
	t.Parallel()
	var got string
	tr := &SubagentTracker{OnStatus: func(text string) { got = text }}
	tr.Add("ag1", "task A")
	tr.Add("ag2", "task B")

	tr.ClearAll()

	if tr.Pending() != 0 {
		t.Fatalf("Pending() = %d, want 0", tr.Pending())
	}
	if got != "" {
		t.Errorf("expected empty completion detail, got %q", got)
	}
}

// TestSubagentTracker_ClearAllEmpty verifies ClearAll is a safe no-op
// when there are no pending agents.
func TestSubagentTracker_ClearAllEmpty(t *testing.T) {
	t.Parallel()
	statusCalled := false
	tr := &SubagentTracker{OnStatus: func(string) { statusCalled = true }}
	tr.ClearAll()

	if statusCalled {
		t.Error("OnStatus should not be called when ClearAll has nothing to clear")
	}
}

// TestSubagentTracker_NilCallback verifies that all operations are safe
// when OnStatus is nil.
func TestSubagentTracker_NilCallback(t *testing.T) {
	t.Parallel()
	tr := &SubagentTracker{} // OnStatus is nil
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
