package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"foci/internal/platform"
	"foci/internal/turnevent"
)

// interruptCountingBackend is an adopting backend that counts interrupts.
type interruptCountingBackend struct {
	*adoptingBackend
	interrupts atomic.Int32
}

func (b *interruptCountingBackend) Interrupt(context.Context) error {
	b.interrupts.Add(1)
	return nil
}

// spawnRunSink records the events of a claimed spawn run.
type spawnRunSink struct {
	mu     sync.Mutex
	events []string
}

func (s *spawnRunSink) Emit(_ context.Context, ev turnevent.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e := ev.(type) {
	case turnevent.TurnStart:
		s.events = append(s.events, "start")
	case turnevent.TextBlock:
		s.events = append(s.events, "text:"+e.Text)
	case turnevent.TurnComplete:
		s.events = append(s.events, "complete:"+e.FinalText)
	}
}
func (s *spawnRunSink) DeliversToPlatform() bool { return false }
func (s *spawnRunSink) got() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.events, ",")
}

// TestOpenAutonomousTurn_SpawnBranchReachesNoChat is #2729: Claude Code
// starts a run by itself on a background spawn's branch (a background task
// the spawn started finished after its turn ended). The run goes to the
// spawn's sink (SpawnBranchTurn), which reports it to the spawner, and no
// text reaches the chat connection. The control session, which no spawn
// claims, still delivers as before; it proves the connection would have
// received the text.
func TestOpenAutonomousTurn_SpawnBranchReachesNoChat(t *testing.T) {
	for _, c := range []struct {
		name    string
		sk      string
		claimed bool
	}{
		{"spawn-branch", "helen/c42/b1793521296", true},
		{"control", "helen/c42", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			initTestConvo(t, "helen")
			be := &interruptCountingBackend{adoptingBackend: &adoptingBackend{}}
			be.sessionFile = "/tmp/spawn-autonomous.jsonl"
			a := newBatchTestAgent(t, be)
			conn := &leakConn{}
			a.ResolveLateConn = func(string) platform.Connection { return conn }
			spawnSink := &spawnRunSink{}
			var stop func()
			a.SpawnBranchTurn = func(sk string, s func()) (turnevent.Sink, bool) {
				if sk != "helen/c42/b1793521296" {
					return nil, false
				}
				stop = s
				return spawnSink, true
			}

			a.OpenAutonomousTurn(c.sk, be, []string{"toolu_BASH"})
			if !a.IsTurnInFlight(c.sk) {
				t.Fatal("the autonomous run was not adopted")
			}
			if got := a.IsInFlightDelivering(c.sk); got == c.claimed {
				t.Errorf("IsInFlightDelivering = %v, want %v", got, !c.claimed)
			}
			a.sessionRouter(c.sk).Emit(context.Background(), turnevent.TextBlock{Text: "the suite passed", Phase: turnevent.PhaseIntermediate})
			be.completeAdopted("the suite passed")
			waitNotInFlight(t, a, c.sk)

			delivered := strings.Contains(strings.Join(conn.got(), "\n"), "the suite passed")
			if delivered == c.claimed {
				t.Errorf("text delivered to the chat = %v, want %v (calls: %v)", delivered, !c.claimed, conn.got())
			}
			if !c.claimed {
				return
			}
			if got := spawnSink.got(); got != "start,text:the suite passed,complete:the suite passed" {
				t.Errorf("spawn sink events = %q", got)
			}
			if stop == nil {
				t.Fatal("no stop func handed to SpawnBranchTurn")
			}
			stop()
			if n := be.interrupts.Load(); n != 1 {
				t.Errorf("stop interrupted the backend %d times, want 1", n)
			}
		})
	}
}
