package ccstream

import (
	"bytes"
	"testing"
	"time"

	"foci/internal/delegator"
)

// newGateTestBackend builds a backend with a recorder writer, typing hook and
// logger, the shape the gate tests need.
func newGateTestBackend() (*Backend, *bytes.Buffer) {
	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}
	return b, &buf
}

// TestImmediateInject_SystemPendingWorkExempt drives the full Inject routing
// (#2233): a SourceSystem inject flagged PendingWorkExempt (a keepalive begun
// by RunInference) STARTS while background work is pending — the pending-work
// refusal is the one it skips. The non-exempt control is refused, as pinned by
// TestTryBeginTurn_RejectsWhilePending.
func TestImmediateInject_SystemPendingWorkExempt(t *testing.T) {
	t.Parallel()

	b, _ := newGateTestBackend()
	b.agents.Add("sub1", "explore")

	err := b.ImmediateInject(t.Context(), delegator.Inject{
		Source:            delegator.SourceSystem,
		Text:              "keepalive",
		Turn:              &delegator.TurnEvents{},
		PendingWorkExempt: true,
	})
	if err != nil {
		t.Fatalf("exempt SourceSystem inject while pending = %v, want nil (a keepalive must not wait out the background-work window)", err)
	}
	if !b.IsTurnInFlight() {
		t.Fatal("exempt inject did not begin a turn")
	}

	// Control on the same backend state: non-exempt system input is refused.
	b2, _ := newGateTestBackend()
	b2.agents.Add("sub1", "explore")
	if err := b2.tryBeginTurn(&delegator.TurnEvents{}); err != delegator.ErrTurnInFlight {
		t.Fatalf("non-exempt tryBeginTurn while pending = %v, want ErrTurnInFlight", err)
	}
}

// TestTryBeginTurn_ExemptStillRefusedWhileTurnActive pins that the exemption
// skips ONLY the pending-work refusal: with a turn active the exempt begin is
// refused like any other (turn exclusivity is not negotiable).
func TestTryBeginTurn_ExemptStillRefusedWhileTurnActive(t *testing.T) {
	t.Parallel()

	b, _ := newGateTestBackend()
	b.beginTurn(nil) // a turn is active
	if err := b.tryBeginTurnIgnoringPending(&delegator.TurnEvents{}); err != delegator.ErrTurnInFlight {
		t.Fatalf("exempt begin while a turn is active = %v, want ErrTurnInFlight", err)
	}
}

// TestTryBeginTurn_ExemptStillRefusedInGrace pins the #1048 half: within the
// post-run chain grace the exempt begin is refused too — the grace guards the
// autonomous chain gap, not the pending-work window.
func TestTryBeginTurn_ExemptStillRefusedInGrace(t *testing.T) {
	t.Parallel()

	b, _ := newGateTestBackend()
	b.turnMu.Lock()
	b.lastAutonomousEnd = time.Now()
	b.turnMu.Unlock()
	if err := b.tryBeginTurnIgnoringPending(&delegator.TurnEvents{}); err != delegator.ErrTurnInFlight {
		t.Fatalf("exempt begin inside the post-run grace = %v, want ErrTurnInFlight", err)
	}
}
