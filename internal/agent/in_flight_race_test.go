package agent

import (
	"context"
	"testing"

	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/turnevent"
)

// racedAdoptionBackend models the losing side of an adoption race: by the time
// the autonomous open tries to adopt the run, a foci-initiated turn has already
// won it and registered its own sink over the open's registration. onAdopt runs
// inside AdoptRunningTurn — the only code between the open's Register and its
// unwind — which is exactly the window the racing turn lands in production.
type racedAdoptionBackend struct {
	*adoptingBackend
	onAdopt func()
}

func (b *racedAdoptionBackend) AdoptRunningTurn(turn *delegator.TurnEvents) bool {
	if b.onAdopt != nil {
		b.onAdopt()
	}
	return false // the race was lost
}

// raceWinnerSink records the text blocks it receives — the racing turn's sink.
type raceWinnerSink struct {
	texts []string
}

func (s *raceWinnerSink) Emit(_ context.Context, ev turnevent.Event) {
	if e, ok := ev.(turnevent.TextBlock); ok {
		s.texts = append(s.texts, e.Text)
	}
}
func (s *raceWinnerSink) DeliversToPlatform() bool { return true }

// TestOpenAutonomousTurn_LostAdoptionRaceSparesWinnersRegistration pins the
// unwind of a lost adoption race (#2233): the open registers its sink
// synchronously before adopting, so when adoption loses to a foci-initiated
// turn whose registration already replaced it, the unwind must release ONLY
// the open's own registration. The bare router.Clear() it replaced wiped the
// winner's sink mid-run, diverting the run's output to the late-delivery
// fallback — the same hazard the keepalive exemption analysed for the
// completion path, on the refusal path.
func TestOpenAutonomousTurn_LostAdoptionRaceSparesWinnersRegistration(t *testing.T) {
	initTestConvo(t, "helen")
	const sk = "helen/c42"

	inner := &adoptingBackend{}
	inner.sessionFile = "/tmp/adoption-race.jsonl"
	winner := &raceWinnerSink{}

	var a *Agent
	be := &racedAdoptionBackend{adoptingBackend: inner}
	be.onAdopt = func() {
		a.sessionRouter(sk).Register(winner) // the racing foci turn takes over
	}

	a = newBatchTestAgent(t, be)
	a.ResolveLateConn = func(string) platform.Connection { return &leakConn{} }

	a.OpenAutonomousTurn(sk, be, nil)

	if a.IsTurnInFlight(sk) {
		t.Fatal("a refused adoption still left the session with a turn in flight")
	}

	// The racing turn's registration survived the loser's unwind.
	a.sessionRouter(sk).Emit(context.Background(), turnevent.TextBlock{Text: "run text"})
	if len(winner.texts) != 1 || winner.texts[0] != "run text" {
		t.Fatalf("emit after the lost race went to %v, want the racing turn's sink — the unwind wiped it (#2233)", winner.texts)
	}
}
