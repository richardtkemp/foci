package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/turnevent"
)

// adoptingBackend is a delegated backend that, like ccstream, starts runs on
// its own and lets foci adopt them (delegator.TurnAdopter). The test fires the
// autonomous open itself, standing in for CC's running edge.
type adoptingBackend struct {
	batchTurnBackend

	adoptMu sync.Mutex
	adopted *delegator.TurnEvents
}

func (b *adoptingBackend) SetOnAutonomousOpen(func(triggers []string)) {}
func (b *adoptingBackend) AdoptRunningTurn(turn *delegator.TurnEvents) bool {
	b.adoptMu.Lock()
	defer b.adoptMu.Unlock()
	if b.adopted != nil {
		return false
	}
	b.adopted = turn
	return true
}

// completeAdopted ends the adopted run the way a backend does at CC's idle.
func (b *adoptingBackend) completeAdopted(text string) {
	b.adoptMu.Lock()
	turn := b.adopted
	b.adopted = nil
	b.adoptMu.Unlock()
	if turn != nil && turn.OnTurnComplete != nil {
		turn.OnTurnComplete(&delegator.TurnResult{Text: text, Model: "claude-sonnet-4-5"})
	}
}

func (b *adoptingBackend) events() *delegator.SessionEvents {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionEvents
}

// runSystemTurnWithBackgroundAgent drives a real system turn (no sink on ctx,
// as the periodic runner does) that starts a background subagent: the Agent
// tool_use, its start, and some of its text arrive during the turn. The turn
// then ends while the subagent is still running.
func runSystemTurnWithBackgroundAgent(t *testing.T, trigger, sk string, be *adoptingBackend) (*Agent, *leakConn) {
	t.Helper()
	be.sessionFile = "/tmp/bg-origin.jsonl"
	var a *Agent
	be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
		se := be.events()
		go func() {
			defer h.OnTurnComplete(&delegator.TurnResult{Text: "dispatched", Model: "claude-sonnet-4-5"})
			if !waitRegistered(t, a, sk) {
				return
			}
			se.OnText("dispatched")
			se.OnToolStart("toolu_BG", "Agent", `{"description":"dig","prompt":"check the logs","run_in_background":true}`)
			se.OnSubagentStart("toolu_BG", "dig", "check the logs", 1)
			se.OnSubagentText("toolu_BG", "looking", 1)
		}()
		return nil, nil
	}
	a = newBatchTestAgent(t, be)
	conn := &leakConn{}
	a.ResolveLateConn = func(string) platform.Connection { return conn }

	if err := a.HandleMessage(WithTrigger(context.Background(), trigger), sk, []string{trigger}, nil); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if a.sessionRouter(sk).current.Load() != nil {
		t.Fatal("the system turn's sink is still registered after it ended")
	}
	return a, conn
}

// TestBackgroundSubagentOfSystemTurn_NotDelivered is #2093 part A: a subagent
// a non-delivered turn started in the background keeps running after the turn
// ends and its sink is cleared. Its later output used to fall to the router's
// late-delivery fallback, which delivered it to the chat and recorded it with
// no turn_kind. By ruling it is recorded tagged with the originating turn's
// kind and not delivered: reflection (a recorded kind) and keepalive (whose own
// turn is unrecorded, but whose background follow-ups are recorded).
func TestBackgroundSubagentOfSystemTurn_NotDelivered(t *testing.T) {
	for _, c := range []struct {
		trigger string
		want    []string
	}{
		{"reflection", []string{
			"text||0|reflection|dispatched",
			"subagent_prompt|toolu_BG|1|reflection|[subagent: dig]\ncheck the logs",
			"subagent|toolu_BG|1|reflection|looking",
			"subagent|toolu_BG|1|reflection|the logs are clean",
		}},
		{"keepalive", []string{
			"subagent|toolu_BG|1|keepalive|the logs are clean",
		}},
	} {
		t.Run(c.trigger, func(t *testing.T) {
			path := initTestConvo(t, "helen")
			be := &adoptingBackend{}
			_, conn := runSystemTurnWithBackgroundAgent(t, c.trigger, "helen/c42", be)

			se := be.events()
			se.OnSubagentText("toolu_BG", "the logs are clean", 1)
			se.OnSubagentEnd("toolu_BG", 1)

			if calls := conn.got(); len(calls) != 0 {
				t.Errorf("the %s turn's background subagent reached the chat:\n  %s", c.trigger, strings.Join(calls, "\n  "))
			}
			assertRows(t, convoRows(t, path), c.want)
		})
	}
}

// TestBackgroundSubagentOfSystemTurn_UserTurnUnaffected: while a user turn is
// live, the background subagent of an earlier reflection must not render into
// it (a user turn is never held on pending work), and the user turn's own
// subagents still reach it.
func TestBackgroundSubagentOfSystemTurn_UserTurnUnaffected(t *testing.T) {
	initTestConvo(t, "helen")
	be := &adoptingBackend{}
	a, _ := runSystemTurnWithBackgroundAgent(t, "reflection", "helen/c42", be)

	owner := &subagentTextSink{}
	a.sessionRouter("helen/c42").Register(owner)
	se := be.events()
	se.OnSubagentText("toolu_BG", "reflection subagent text", 1)
	se.OnSubagentText("toolu_USER", "user subagent text", 1)

	evs := owner.got()
	joined := strings.Join(evs, "\n")
	if strings.Contains(joined, "reflection subagent text") {
		t.Errorf("a reflection's background subagent rendered into the live user turn: %v", evs)
	}
	if !strings.Contains(joined, "user subagent text") {
		t.Errorf("the user turn's own subagent did not reach it: %v", evs)
	}
}

// subagentTextSink is a delivering per-turn sink that keeps the subagent text
// it receives.
type subagentTextSink struct {
	mu    sync.Mutex
	texts []string
}

func (s *subagentTextSink) Emit(_ context.Context, ev turnevent.Event) {
	if e, ok := ev.(turnevent.SubagentText); ok {
		s.mu.Lock()
		s.texts = append(s.texts, e.Text)
		s.mu.Unlock()
	}
}
func (s *subagentTextSink) DeliversToPlatform() bool { return true }
func (s *subagentTextSink) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.texts...)
}

// waitNotInFlight blocks until sk has no turn in flight. Syncs on the state;
// the deadline is only a hang guard.
func waitNotInFlight(t *testing.T, a *Agent, sk string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for a.IsTurnInFlight(sk) {
		if time.Now().After(deadline) {
			t.Fatalf("%s still in flight", sk)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestAutonomousRunAfterSystemTurnBackgroundWork is #2093 part B: when the
// background subagent finishes, CC starts a run of its own to react to the
// result. If every task that triggered it came from a non-delivered turn, the
// run is recorded tagged with that turn's kind and not delivered, and it is not
// counted as a delivering turn. A run triggered by anything else (here a task
// with no recorded origin) still delivers, as before.
func TestAutonomousRunAfterSystemTurnBackgroundWork(t *testing.T) {
	for _, c := range []struct {
		name     string
		trigger  string
		triggers []string
		deliver  bool
		wantRow  string
	}{
		{"reflection", "reflection", []string{"toolu_BG"}, false, "text||0|reflection|the subagent says the logs are clean"},
		{"keepalive", "keepalive", []string{"toolu_BG"}, false, "text||0|keepalive|the subagent says the logs are clean"},
		{"mixed-origin", "reflection", []string{"toolu_BG", "toolu_OTHER"}, true, "text||0||the subagent says the logs are clean"},
		{"no-trigger", "reflection", nil, true, "text||0||the subagent says the logs are clean"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := initTestConvo(t, "helen")
			be := &adoptingBackend{}
			sk := "helen/c42"
			a, conn := runSystemTurnWithBackgroundAgent(t, c.trigger, sk, be)

			se := be.events()
			se.OnSubagentEnd("toolu_BG", 1)
			a.OpenAutonomousTurn(sk, be, c.triggers)
			if !a.IsTurnInFlight(sk) {
				t.Fatal("the autonomous run was not adopted")
			}
			if got := a.IsInFlightDelivering(sk); got != c.deliver {
				t.Errorf("IsInFlightDelivering = %v, want %v", got, c.deliver)
			}
			se.OnText("the subagent says the logs are clean")
			be.completeAdopted("the subagent says the logs are clean")
			waitNotInFlight(t, a, sk)

			delivered := strings.Contains(strings.Join(conn.got(), "\n"), "the subagent says the logs are clean")
			if delivered != c.deliver {
				t.Errorf("reply delivered = %v, want %v (calls: %v)", delivered, c.deliver, conn.got())
			}
			rows := convoRows(t, path)
			found := false
			for _, r := range rows {
				if r == c.wantRow {
					found = true
				}
			}
			if !found {
				t.Errorf("rows = %v, want one %q", rows, c.wantRow)
			}
		})
	}
}

// TestBackgroundSubagentOfSystemTurn_ResumedByUserTurnDelivers: a delivering
// turn that resumes a reflection's subagent (SendMessage; a new run of the same
// group) takes it over, so the resumed run's output reaches that turn and CC's
// later reaction to its result is not suppressed as the reflection's.
func TestBackgroundSubagentOfSystemTurn_ResumedByUserTurnDelivers(t *testing.T) {
	initTestConvo(t, "helen")
	be := &adoptingBackend{}
	sk := "helen/c42"
	a, _ := runSystemTurnWithBackgroundAgent(t, "reflection", sk, be)

	se := be.events()
	se.OnSubagentEnd("toolu_BG", 1) // run 1 ends after the reflection
	owner := &subagentTextSink{}
	a.sessionRouter(sk).Register(owner)
	se.OnSubagentStart("toolu_BG", "dig", "and the other logs?", 2)
	se.OnSubagentText("toolu_BG", "resumed answer", 2)
	a.sessionRouter(sk).Clear()

	if got := owner.got(); strings.Join(got, ",") != "resumed answer" {
		t.Errorf("the user turn's resumed run got %v, want [resumed answer]", got)
	}
	if kind := a.takeBgOriginKind(sk, []string{"toolu_BG"}); kind != "" {
		t.Errorf("after the user turn resumed it, the group still classifies as %q", kind)
	}
}
