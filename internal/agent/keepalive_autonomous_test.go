package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/turnevent"
)

// captureSink records every event's type — a minimal per-turn sink for router
// ordering assertions.
type captureSink struct {
	name  string
	texts []string
}

func (s *captureSink) Emit(_ context.Context, ev turnevent.Event) {
	if e, ok := ev.(turnevent.TextBlock); ok {
		s.texts = append(s.texts, s.name+":"+e.Text)
	}
}
func (s *captureSink) DeliversToPlatform() bool { return true }

// TestSessionRouter_ReleaseSparesNewerRegistration pins the Register release
// semantics (#2233): a release clears only while its own registration is still
// current, so a turn whose sink a later Register replaced (an autonomous run
// adopted while the turn still ran) cannot wipe the newer one mid-run — the
// old bare Clear() did exactly that and diverted the rest of the run to the
// late-delivery fallback.
func TestSessionRouter_ReleaseSparesNewerRegistration(t *testing.T) {
	r := newSessionRouter(&captureSink{name: "fallback"})

	sys := &captureSink{name: "system"}
	releaseSys := r.Register(sys)
	auto := &captureSink{name: "autonomous"}
	releaseAuto := r.Register(auto) // the adopted run registers over the system turn's sink

	releaseSys() // the system turn ends — must NOT clear the autonomous run's sink

	r.Emit(context.Background(), turnevent.TextBlock{Text: "run text"})
	if len(auto.texts) != 1 || auto.texts[0] != "autonomous:run text" {
		t.Fatalf("emit after the older release = %v, want it on the autonomous sink", auto.texts)
	}
	if len(sys.texts) != 0 {
		t.Fatalf("the replaced sink still received events: %v", sys.texts)
	}

	// The newer registration's own release still clears normally.
	releaseAuto()
	r.Emit(context.Background(), turnevent.TextBlock{Text: "late"})
	if len(auto.texts) != 1 {
		t.Fatalf("release did not clear the current registration: %v", auto.texts)
	}

	// The refused and nil arms hand back no-op releases, and a release is
	// idempotent.
	r.Register(nil)()     // nil sink == Clear; its release is a no-op
	noop := r.Register(r) // self-registration refused — release must be safe
	noop()
	noop()
}

// TestSystemTurn_ReleaseKeepsAutonomousRegistration reproduces the #1068
// interleaving the keepalive exemption re-opens (#2233 hazard analysis): a
// keepalive turn runs while background work is pending; the task ends during
// the turn and CC opens the chained run, which foci adopts (no bgOrigin stamp
// — the task was started by a delivering turn — so the run DELIVERS). The
// keepalive turn's deferred registration release must leave the adopted run's
// sink registered, and the run's output must reach the chat rather than the
// keepalive's silent sink.
func TestSystemTurn_ReleaseKeepsAutonomousRegistration(t *testing.T) {
	initTestConvo(t, "helen")
	const sk = "helen/c42"
	be := &adoptingBackend{}
	be.sessionFile = "/tmp/ka.jsonl"

	var a *Agent
	be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
		go func() {
			if !waitRegistered(t, a, sk) {
				return
			}
			// The background task ends while the keepalive turn runs; CC opens
			// the chained run and foci adopts it with a delivering sink.
			a.OpenAutonomousTurn(sk, be, nil)
			// The keepalive turn now completes; its deferred release runs
			// after this — the adopted registration must survive it.
			h.OnTurnComplete(&delegator.TurnResult{Text: "ka", Model: "claude-sonnet-4-5"})
		}()
		return nil, nil
	}
	a = newBatchTestAgent(t, be)
	conn := &leakConn{}
	a.ResolveLateConn = func(string) platform.Connection { return conn }

	if err := a.HandleMessage(WithTrigger(context.Background(), TriggerKeepalive), sk, []string{"keepalive"}, nil); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// The adopted run's registration survived the turn's release.
	if a.sessionRouter(sk).current.Load() == nil {
		t.Fatal("the keepalive turn's release wiped the adopted run's registration — the run's tail would divert to late delivery (#2233 hazard)")
	}

	// The run's output reaches the chat (adopted delivering sink), not the
	// keepalive's silent one.
	be.events().OnText("the chained result")
	if !waitFor(time.Second, func() bool {
		return len(conn.got()) > 0
	}) {
		t.Fatal("the adopted run's text reached no chat — swallowed by the keepalive's silent sink")
	}
	joined := strings.Join(conn.got(), "\n")
	if !strings.Contains(joined, "the chained result") {
		t.Fatalf("chat got %q, want the chained run's text", joined)
	}

	be.completeAdopted("final")
	waitNotInFlight(t, a, sk)
	if a.sessionRouter(sk).current.Load() != nil {
		t.Fatal("the adopted run's registration outlived the run")
	}
}

// TestRunTurn_ReleaseKeepsAdoptedRegistration is the platform-turn sibling of
// TestSystemTurn_ReleaseKeepsAutonomousRegistration: a USER turn driven
// through the inbox registers its sink inside RunTurn (run_turn.go) — the
// nil-sink test drivers never reach that registration. An autonomous run
// adopted while the turn still runs registers over it; the turn's deferred
// cleanup must release only its own registration. The bare router.Clear() it
// replaced wiped the adopted run's sink mid-run, diverting the run's output
// to the late-delivery fallback.
func TestRunTurn_ReleaseKeepsAdoptedRegistration(t *testing.T) {
	initTestConvo(t, "helen")
	const sk = "helen/c42"
	be := &adoptingBackend{}
	be.sessionFile = "/tmp/rt-adopt.jsonl"

	var a *Agent
	be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
		go func() {
			if !waitRegistered(t, a, sk) {
				return
			}
			// CC opens a chained run while the user turn still runs; foci
			// adopts it with a delivering sink (no bgOrigin stamp).
			a.OpenAutonomousTurn(sk, be, nil)
			// The user turn now completes; RunTurn's deferred release runs
			// after this — the adopted registration must survive it.
			h.OnTurnComplete(&delegator.TurnResult{Text: "user reply", Model: "claude-sonnet-4-5"})
		}()
		return nil, nil
	}
	a = newBatchTestAgent(t, be)
	conn := &leakConn{}
	a.ResolveLateConn = func(string) platform.Connection { return conn }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartInbox(ctx)

	hook := make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	d := &sinkDriver{recordingDriver: recordingDriver{hookCh: hook, doneCh: done}, sink: &raceWinnerSink{}}
	a.Enqueue(Envelope{SessionKey: sk, Text: "user speaks", Driver: d})
	<-hook
	if !waitRegistered(t, a, sk) {
		return
	}
	<-done // the whole RunTurn — including its deferred release — has returned

	// The adopted run's registration survived the turn's release.
	if a.sessionRouter(sk).current.Load() == nil {
		t.Fatal("the user turn's release wiped the adopted run's registration — the run's tail diverts to late delivery (#2233)")
	}

	// The run's output reaches the chat (adopted delivering sink), not the
	// late-delivery fallback a wiped registration would divert it to.
	be.events().OnText("chained reply")
	if !waitFor(time.Second, func() bool {
		return strings.Contains(strings.Join(conn.got(), "\n"), "chained reply")
	}) {
		t.Fatal("the adopted run's text reached no chat — the turn's release diverted it to late delivery")
	}

	be.completeAdopted("final")
	waitNotInFlight(t, a, sk)
	if a.sessionRouter(sk).current.Load() != nil {
		t.Fatal("the adopted run's registration outlived the run")
	}
}

// TestUserInputFold_KeepsAdoptedRegistration is the ask-answer sibling of
// TestRunTurn_ReleaseKeepsAdoptedRegistration (#2233 review F3): an ask
// answer arriving while an adopted autonomous run is in flight (steer mode
// off, so it queues rather than steers) folds into the run as a SourceUser
// follow-up — and the folded turn must not register its sink over the run's.
// Phase 3.5 skips the registration via TurnState.foldedFollowUp, exactly the
// guard RunTurn's fold branch gives a typed message; without it the answer's
// registration replaced the run's and its release then cleared the router
// mid-run, diverting the run's output to the late-delivery fallback.
func TestUserInputFold_KeepsAdoptedRegistration(t *testing.T) {
	initTestConvo(t, "helen")
	const sk = "helen/c42"
	be := &adoptingBackend{}
	be.sessionFile = "/tmp/answer-adopt.jsonl"
	be.setTurnInFlight(true) // the adopted run is the backend's in-flight turn

	a := newBatchTestAgent(t, be)
	conn := &leakConn{}
	a.ResolveLateConn = func(string) platform.Connection { return conn }

	// The adopted autonomous run registers its delivering sink synchronously.
	a.OpenAutonomousTurn(sk, be, nil)
	if a.sessionRouter(sk).current.Load() == nil {
		t.Fatal("the adopted run never registered its sink")
	}

	// The ask answer, delivered the way deliverToSessionChatThen does: its
	// own turn sink on ctx (NOT the router), the user-input marker, trigger
	// ask_grader.
	answerSink := &captureSink{name: "answer"}
	ctx := WithTrigger(WithUserInput(turnevent.WithSink(context.Background(), answerSink)), "ask_grader")
	if err := a.HandleMessage(ctx, sk, []string{"[SYSTEM: the user answered]"}, nil); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// The answer folded into the run as a SourceUser follow-up.
	injects := be.InjectsRecorded()
	if len(injects) != 1 || injects[0].Source != delegator.SourceUser {
		t.Fatalf("injects = %+v, want exactly one SourceUser fold", injects)
	}
	if len(answerSink.texts) != 0 {
		t.Fatalf("the folded answer turn delivered its own output: %v", answerSink.texts)
	}

	// The adopted run's registration survived the folded answer — the answer
	// turn registered nothing over it.
	if a.sessionRouter(sk).current.Load() == nil {
		t.Fatal("the ask-answer fold wiped the adopted run's registration — the run's output diverts to late delivery (#2233 F3)")
	}

	// The run's output still reaches the chat through the ADOPTED sink, not
	// the late-delivery fallback.
	be.events().OnText("chained reply")
	if !waitFor(time.Second, func() bool {
		return strings.Contains(strings.Join(conn.got(), "\n"), "chained reply")
	}) {
		t.Fatal("the adopted run's text reached no chat — the fold diverted it to late delivery")
	}

	be.completeAdopted("final")
	waitNotInFlight(t, a, sk)
	if a.sessionRouter(sk).current.Load() != nil {
		t.Fatal("the adopted run's registration outlived the run")
	}
}

// TestKeepaliveDuringPending_AutonomousRunDelivers is the characterisation
// half of the same hazard premise: with no bgOrigin stamp (a task started by
// a delivering turn), adoption takes the DELIVERING branch — recorded and
// sent to the chat — never the record-only one. The keepalive exemption
// relies on this for every run that chains while a keepalive turn runs.
func TestKeepaliveDuringPending_AutonomousRunDelivers(t *testing.T) {
	initTestConvo(t, "helen")
	const sk = "helen/c42"
	be := &adoptingBackend{}
	be.sessionFile = "/tmp/ka.jsonl"
	be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
		h.OnTurnComplete(&delegator.TurnResult{Text: "ka", Model: "claude-sonnet-4-5"})
		return nil, nil
	}
	a := newBatchTestAgent(t, be)
	conn := &leakConn{}
	a.ResolveLateConn = func(string) platform.Connection { return conn }

	// A keepalive turn runs and ends with no stamped background tool calls.
	if err := a.HandleMessage(WithTrigger(context.Background(), TriggerKeepalive), sk, []string{"keepalive"}, nil); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	waitNotInFlight(t, a, sk)

	// The chained run opens with triggers that match no stamp.
	a.OpenAutonomousTurn(sk, be, []string{"toolu_unstamped"})

	be.events().OnText("chained text")
	if !waitFor(time.Second, func() bool {
		return strings.Contains(strings.Join(conn.got(), "\n"), "chained text")
	}) {
		t.Fatal("an unstamped autonomous run after a keepalive turn was not delivered — the hazard premise failed")
	}
	be.completeAdopted("final")
	waitNotInFlight(t, a, sk)
}
