package agent

import (
	"testing"

	"foci/internal/delegator"
)

func TestToolTally_CountsDistinctCalls(t *testing.T) {
	tt := newToolTally()
	tt.start("a", "Read")
	tt.start("a", "Read") // re-emitted start (opencode)
	tt.end("a", "Read")
	tt.start("b", "Bash") // still running
	tt.end("c", "Edit")   // end with no start
	tt.end("c", "Edit")   // ended from two paths (ccstream denial)
	tt.start("k", delegator.CompactionToolName)
	tt.end("k", delegator.CompactionToolName)

	if got := tt.calls(); got != 3 {
		t.Errorf("calls = %d, want 3 (a, b, c; compaction skipped)", got)
	}
	if got := tt.completed(); got != 2 {
		t.Errorf("completed = %d, want 2 (a, c)", got)
	}
}

func TestToolTally_IdlessCallsCountByEvent(t *testing.T) {
	tt := newToolTally()
	tt.start("", "x")
	tt.start("", "y")
	tt.end("", "x")
	if got := tt.calls(); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
	if got := tt.completed(); got != 1 {
		t.Errorf("completed = %d, want 1", got)
	}
}

// TestAttachDelivery_FeedsToolTally: the SessionEvents AttachDelivery installs
// feed the same tally a turn resets and reads, and a reset starts the next
// turn at zero.
func TestAttachDelivery_FeedsToolTally(t *testing.T) {
	a := &Agent{}
	be := &mockBackendDT{}
	const sk = "test/s"
	a.AttachDelivery(be, sk)
	tt := a.resetToolTally(sk)

	be.sessionEvents.OnToolStart("t1", "Read", "{}")
	be.sessionEvents.OnToolEnd("t1", "Read", "", false)
	if tt.calls() != 1 || tt.completed() != 1 {
		t.Fatalf("after one tool: calls=%d completed=%d, want 1/1", tt.calls(), tt.completed())
	}

	if again := a.resetToolTally(sk); again != tt || tt.calls() != 0 {
		t.Errorf("reset: same tally=%v calls=%d, want true/0", again == tt, tt.calls())
	}
}
