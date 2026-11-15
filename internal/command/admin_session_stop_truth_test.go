package command

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/tools"
)

// stopTruthBackend is a delegated backend with a turn in flight (any turn: a
// user turn or a system turn such as a spawn result's). honour decides whether
// Interrupt ends it.
type stopTruthBackend struct {
	mockPassBackend
	inFlight    atomic.Bool
	honour      bool
	interrupted atomic.Int32
}

func (b *stopTruthBackend) IsTurnInFlight() bool { return b.inFlight.Load() }
func (b *stopTruthBackend) Interrupt(context.Context) error {
	b.interrupted.Add(1)
	if b.honour {
		b.inFlight.Store(false)
	}
	return nil
}

func runStopOn(t *testing.T, be delegator.Delegator) Response {
	t.Helper()
	const sk = "alpha/c3120"
	dm := &agent.DelegatedManager{NewBackend: func() (delegator.Delegator, error) { return be, nil }}
	if _, err := dm.Get(context.Background(), sk); err != nil {
		t.Fatalf("seed backend: %v", err)
	}
	cc := CommandContext{Agent: &agent.Agent{DelegatedManager: dm}}
	resp, err := StopCommand().Execute(tools.WithSessionKey(context.Background(), sk), Request{}, cc)
	if err != nil {
		t.Fatalf("/stop: %v", err)
	}
	return resp
}

// TestStopCommand_TruthfulReply (#3120 Q59: "/stop interrupts whatever turn
// runs, system turns included", and its "Stopped." must be true): a turn the
// backend ends on interrupt gets "Stopped."; a turn the backend keeps running
// (codex ignoring its interrupt before #3117) gets a reply that says so,
// instead of "Stopped.".
func TestStopCommand_TruthfulReply(t *testing.T) {
	prev := stopConfirmTimeout
	stopConfirmTimeout = 50 * time.Millisecond // the ignored interrupt never ends the turn: any bound gives the same answer
	t.Cleanup(func() { stopConfirmTimeout = prev })

	honoured := &stopTruthBackend{honour: true}
	honoured.inFlight.Store(true)
	if resp := runStopOn(t, honoured); resp.Text != "Stopped." || honoured.interrupted.Load() != 1 {
		t.Fatalf("honoured interrupt: reply %q, interrupts %d; want \"Stopped.\" after one interrupt", resp.Text, honoured.interrupted.Load())
	}

	ignored := &stopTruthBackend{honour: false}
	ignored.inFlight.Store(true)
	if resp := runStopOn(t, ignored); resp.Text != stillRunningText {
		t.Fatalf("ignored interrupt: reply %q, want %q (the turn is still running)", resp.Text, stillRunningText)
	}

	idle := &stopTruthBackend{}
	if resp := runStopOn(t, idle); resp.Text != "Stopped." {
		t.Fatalf("idle session: reply %q, want \"Stopped.\"", resp.Text)
	}
}
