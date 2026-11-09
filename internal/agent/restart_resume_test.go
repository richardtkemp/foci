package agent

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// Tests for #2999: a restart resumes a user-facing session whose turn it cut,
// and runs the user messages it accepted but never started.

func cutTurnIndex(t *testing.T) *session.SessionIndex {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx
}

func cutTurnAgent(t *testing.T, idx *session.SessionIndex) *Agent {
	t.Helper()
	a := newTestAgent(t)
	a.AgentID = "test"
	a.SessionIndex = idx
	return a
}

// simTurn runs what OrchestrateFullTurn does around a turn's delivery:
// trackCutTurn, then (when delivered) the transport's primary-written call,
// then end.
func simTurn(a *Agent, ctx context.Context, sk, trigger string, texts []string, delivered bool, between func()) {
	ts := &TurnState{Ctx: ctx, SessionKey: sk, Trigger: trigger, Texts: texts, StartedAt: time.Now()}
	end := a.trackCutTurn(ts)
	if delivered {
		OnPrimaryWrittenFromContext(ts.Ctx)()
	}
	if between != nil {
		between()
	}
	end()
}

func hasCutTurn(t *testing.T, a *Agent, sk string) (cutTurnRecord, bool) {
	t.Helper()
	return a.loadCutTurn(sk)
}

// Requirement 4: a turn that finished before the stop leaves nothing to
// resume.
func TestCutTurn_FinishedTurnLeavesNoRecord(t *testing.T) {
	a := cutTurnAgent(t, cutTurnIndex(t))
	const sk = "test/c1"
	var during bool
	simTurn(a, context.Background(), sk, "app", []string{"do the thing"}, true, func() {
		_, during = hasCutTurn(t, a, sk)
	})
	if !during {
		t.Fatal("no cut_turn record while the delivered turn ran")
	}
	if rec, ok := hasCutTurn(t, a, sk); ok {
		t.Fatalf("record left after the turn finished: %+v", rec)
	}
}

// A turn still running when the shutdown cuts it keeps its record, with its
// input.
func TestCutTurn_CutTurnKeepsRecord(t *testing.T) {
	a := cutTurnAgent(t, cutTurnIndex(t))
	const sk = "test/c1"
	simTurn(a, context.Background(), sk, "app", []string{"deploy the fix"}, true, a.MarkTurnsCut)
	rec, ok := hasCutTurn(t, a, sk)
	if !ok {
		t.Fatal("cut turn left no record")
	}
	if rec.Input != "deploy the fix" || rec.Trigger != "app" || rec.Resumes != 0 {
		t.Errorf("record = %+v, want input %q, trigger app, 0 resumes", rec, "deploy the fix")
	}
}

// A turn cut before its input reached the backend never had it: no resume
// (its queued message runs as a message instead).
func TestCutTurn_UndeliveredTurnNoRecord(t *testing.T) {
	a := cutTurnAgent(t, cutTurnIndex(t))
	const sk = "test/c1"
	simTurn(a, context.Background(), sk, "app", []string{"hi"}, false, a.MarkTurnsCut)
	if rec, ok := hasCutTurn(t, a, sk); ok {
		t.Fatalf("undelivered turn left a record: %+v", rec)
	}
}

// Requirement 5 and the periodic passes: no record for a batch turn, a
// keepalive, a memory pass, background work or a depth task/ack (each has
// its own path or reruns).
func TestCutTurn_ExcludedTurnsNoRecord(t *testing.T) {
	a := cutTurnAgent(t, cutTurnIndex(t))
	const sk = "test/c1"
	for _, trig := range []string{TriggerKeepalive, "reflection", "background", "depth_task", "depth_ack", "branch", "session_end_memory"} {
		simTurn(a, context.Background(), sk, trig, []string{"x"}, true, a.MarkTurnsCut)
		if rec, ok := hasCutTurn(t, a, sk); ok {
			t.Errorf("trigger %s left a record: %+v", trig, rec)
			a.dropCutTurn(sk)
		}
	}
	ts := &TurnState{Ctx: context.Background(), SessionKey: sk, Trigger: "user", Purpose: "spawn_character"}
	end := a.trackCutTurn(ts)
	OnPrimaryWrittenFromContext(ts.Ctx)()
	end()
	if rec, ok := hasCutTurn(t, a, sk); ok {
		t.Errorf("batch turn left a record: %+v", rec)
	}
}

// A follow-up folded into the running turn adds its text to the record, so a
// resume names it too (the API transport keeps no record of a fold).
func TestCutTurn_FoldAppendsInput(t *testing.T) {
	a := cutTurnAgent(t, cutTurnIndex(t))
	const sk = "test/c1"
	simTurn(a, context.Background(), sk, "app", []string{"first"}, true, func() {
		fold := &TurnState{Ctx: context.Background(), SessionKey: sk, Trigger: "app", Texts: []string{"second"}}
		endFold := a.trackCutTurn(fold)
		fold.foldedFollowUp = true
		OnPrimaryWrittenFromContext(fold.Ctx)()
		endFold()
		if _, ok := hasCutTurn(t, a, sk); !ok {
			t.Error("the fold's end removed the running turn's record")
		}
		a.MarkTurnsCut()
	})
	rec, ok := hasCutTurn(t, a, sk)
	if !ok || !strings.Contains(rec.Input, "first") || !strings.Contains(rec.Input, "second") {
		t.Fatalf("record = %+v (ok=%v), want both inputs", rec, ok)
	}
}

// A restart during the resume keeps the original input and counts the
// restarts; after maxRestartResumes the session gets a notice and no turn.
func TestCutTurn_RestoreResumesAndGivesUp(t *testing.T) {
	idx := cutTurnIndex(t)
	const sk = "test/c1"
	seed := cutTurnAgent(t, idx)
	seed.saveCutTurn(sk, cutTurnRecord{Trigger: "app", StartedAt: time.Date(2026, 11, 9, 17, 40, 0, 0, time.UTC), Input: "rebuild the index"})

	for restart := 1; restart <= maxRestartResumes+1; restart++ {
		a, cancel := startedAgent(t)
		a.AgentID, a.SessionIndex = "test", idx
		d := &recordingDriver{}
		a.SetTurnObserver(d.recordBatch)
		a.UsageHeldDriver = func(string) Driver { return d }
		var mu sync.Mutex
		var notices []string
		a.RestartResumeNotice = func(gotSK, text string) {
			mu.Lock()
			defer mu.Unlock()
			if gotSK != sk {
				t.Errorf("notice to %q, want %q", gotSK, sk)
			}
			notices = append(notices, text)
		}
		a.RestoreCutTurns()
		if restart <= maxRestartResumes {
			if !waitFor(2*time.Second, func() bool { return d.NumCalls() == 1 }) {
				t.Fatalf("restart %d: resume turns = %d, want 1", restart, d.NumCalls())
			}
			env := d.Calls()[0][0]
			if !env.restartResume || env.Steer != platform.SteerNever || !env.Automated {
				t.Errorf("restart %d: resume envelope = %+v, want hidden, queued, automated", restart, env)
			}
			for _, want := range []string{"[SYSTEM: FOCI RESTARTED]", "2026-11-09 17:40 UTC", "rebuild the index", "check whether it already happened"} {
				if !strings.Contains(env.Text, want) {
					t.Errorf("restart %d: resume text lacks %q:\n%s", restart, want, env.Text)
				}
			}
			mu.Lock()
			if len(notices) != 1 || notices[0] != restartResumeNotice {
				t.Errorf("restart %d: notices = %q, want the resume notice", restart, notices)
			}
			mu.Unlock()
			// The resume turn runs and is cut again: it keeps the record as
			// RestoreCutTurns saved it.
			simTurn(a, context.Background(), sk, "app", []string{env.Text}, true, a.MarkTurnsCut)
			rec, ok := hasCutTurn(t, a, sk)
			if !ok || rec.Resumes != restart || rec.Input != "rebuild the index" {
				t.Fatalf("restart %d: record after the cut resume = %+v (ok=%v), want %d resumes and the original input", restart, rec, ok, restart)
			}
		} else {
			if waitFor(300*time.Millisecond, func() bool { return d.NumCalls() > 0 }) {
				t.Fatalf("restart %d: resumed past maxRestartResumes", restart)
			}
			mu.Lock()
			if len(notices) != 1 || !strings.Contains(notices[0], "not resuming it again") {
				t.Errorf("give-up notices = %q", notices)
			}
			mu.Unlock()
			if rec, ok := hasCutTurn(t, a, sk); ok {
				t.Errorf("record kept after giving up: %+v", rec)
			}
		}
		cancel()
	}
}

// A resume turn that finishes clears the record: the next restart resumes
// nothing.
func TestCutTurn_FinishedResumeClears(t *testing.T) {
	idx := cutTurnIndex(t)
	const sk = "test/c1"
	a := cutTurnAgent(t, idx)
	a.saveCutTurn(sk, cutTurnRecord{Trigger: "app", Input: "x", Resumes: 1})
	simTurn(a, context.Background(), sk, "app", []string{"resume"}, true, nil)
	if rec, ok := hasCutTurn(t, a, sk); ok {
		t.Fatalf("record after the resume finished: %+v", rec)
	}
}

// cutSimDriver runs each batch the way OrchestrateFullTurn tracks it
// (trackCutTurn, delivery, end); a turn on block waits for it or for its ctx.
type cutSimDriver struct {
	recordingDriver
	a       *Agent
	block   chan struct{}
	started chan struct{}
	ended   chan struct{}
	mu      sync.Mutex
	last    []Envelope
}

func (d *cutSimDriver) observe(sk string, batch []Envelope) {
	d.recordBatch(sk, batch)
	d.mu.Lock()
	d.last = batch
	d.mu.Unlock()
}

func (d *cutSimDriver) WrapTurn(ctx context.Context, fn func() error) error {
	d.mu.Lock()
	batch := d.last
	d.mu.Unlock()
	var texts []string
	for _, env := range batch {
		texts = append(texts, env.Text)
	}
	// A cut process's transports deliver nothing: a closed backend or a
	// cancelled API call fails before the input arrives.
	delivered := ctx.Err() == nil
	simTurn(d.a, ctx, batch[0].SessionKey, "app", texts, delivered, func() {
		if d.started != nil && delivered {
			d.started <- struct{}{}
		}
		if d.block != nil {
			select {
			case <-d.block:
			case <-ctx.Done():
			}
		}
	})
	if d.ended != nil && delivered {
		d.ended <- struct{}{}
	}
	return nil
}

func queuedStored(a *Agent, sk string) []usageHeldRecord {
	a.queuedInputMu.Lock()
	defer a.queuedInputMu.Unlock()
	return a.loadQueuedInputLocked(sk)
}

// End to end through the inbox: M1's turn is cut by the shutdown while M2
// waits queued behind it. After the restart the session gets the resume
// (naming M1) first and then M2, each once, and nothing stays stored.
func TestCutTurn_RestartResumesAndRunsQueued(t *testing.T) {
	idx := cutTurnIndex(t)
	const sk = "test/c1"

	a1, cancel1 := startedAgent(t)
	a1.AgentID, a1.SessionIndex = "test", idx
	d1 := &cutSimDriver{a: a1, block: make(chan struct{}), started: make(chan struct{}, 1), ended: make(chan struct{}, 1)}
	a1.SetTurnObserver(d1.observe)
	a1.Enqueue(Envelope{SessionKey: sk, Text: "M1", Driver: d1})
	select {
	case <-d1.started:
	case <-time.After(2 * time.Second):
		t.Fatal("M1's turn did not start")
	}
	if !a1.Enqueue(Envelope{SessionKey: sk, Text: "M2", Steer: platform.SteerNever, Driver: d1}) {
		t.Fatal("M2 not accepted")
	}
	if got := queuedStored(a1, sk); len(got) != 1 || got[0].Text != "M2" {
		t.Fatalf("queued store while M1 runs = %+v, want M2 only (M1 was delivered)", got)
	}
	// The shutdown: drain, the graceful wait runs out, the turn is cut.
	a1.BeginShutdown()
	a1.MarkTurnsCut()
	cancel1()
	select {
	case <-d1.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("M1's turn did not end on the cut")
	}
	if !waitFor(time.Second, func() bool { _, ok := a1.loadCutTurn(sk); return ok }) {
		t.Fatal("no cut_turn record after the cut")
	}
	if got := queuedStored(a1, sk); len(got) != 1 || got[0].Text != "M2" {
		t.Fatalf("queued store after the shutdown = %+v, want M2", got)
	}

	// The next process.
	a2, cancel2 := startedAgent(t)
	defer cancel2()
	a2.AgentID, a2.SessionIndex = "test", idx
	d2 := &cutSimDriver{a: a2}
	a2.SetTurnObserver(d2.observe)
	a2.UsageHeldDriver = func(string) Driver { return d2 }
	a2.RestartResumeNotice = func(string, string) {}
	a2.RestoreCutTurns()
	var all []Envelope
	if !waitFor(2*time.Second, func() bool {
		all = nil
		for _, b := range d2.Calls() {
			all = append(all, b...)
		}
		return len(all) >= 2
	}) {
		t.Fatalf("after the restart got %d input(s), want the resume and M2", len(all))
	}
	time.Sleep(100 * time.Millisecond) // nothing more may run
	all = nil
	for _, b := range d2.Calls() {
		all = append(all, b...)
	}
	if len(all) != 2 || !all[0].restartResume || !strings.Contains(all[0].Text, "M1") || all[1].Text != "M2" {
		t.Fatalf("inputs after the restart = %+v, want the resume (naming M1) then M2", all)
	}
	if !waitFor(time.Second, func() bool { _, ok := a2.loadCutTurn(sk); return !ok && len(queuedStored(a2, sk)) == 0 }) {
		rec, _ := a2.loadCutTurn(sk)
		t.Fatalf("left stored after the turns ran: cut_turn=%+v queued=%+v", rec, queuedStored(a2, sk))
	}
}

// A message whose turn ran (or that /stop dropped) does not come back after
// a restart.
func TestQueuedInput_SettledMessagesNotRestored(t *testing.T) {
	idx := cutTurnIndex(t)
	const sk = "test/c1"
	a, cancel := startedAgent(t)
	defer cancel()
	a.AgentID, a.SessionIndex = "test", idx
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)
	a.Enqueue(Envelope{SessionKey: sk, Text: "ran", Driver: d})
	if !waitFor(2*time.Second, func() bool { return d.NumCalls() == 1 }) {
		t.Fatal("turn did not run")
	}
	if !waitFor(time.Second, func() bool { return len(queuedStored(a, sk)) == 0 }) {
		t.Fatalf("stored after its turn ran: %+v", queuedStored(a, sk))
	}
}

// Through a real API turn (OrchestrateFullTurn): a turn the shutdown cuts
// after its first answered call keeps its record, naming its input.
func TestCutTurn_APITurnRecordedThroughOrchestrator(t *testing.T) {
	idx := cutTurnIndex(t)
	var ag *Agent
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		ag.MarkTurnsCut() // the graceful wait runs out while the call is out
		return &provider.MessageResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: provider.TextContent("done"), StopReason: "end_turn",
			Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	ag = &Agent{
		AgentID:      "test",
		SessionIndex: idx,
		Client:       client,
		Sessions:     session.NewStore(t.TempDir()),
		Tools:        tools.NewRegistry(),
		Bootstrap:    workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:        "claude-haiku-4-5",
	}
	const sk = "test/c1"
	if _, err := ag.hmTest(WithTrigger(context.Background(), "app"), sk, "summarise the logs"); err != nil {
		t.Fatalf("turn: %v", err)
	}
	rec, ok := ag.loadCutTurn(sk)
	if !ok || rec.Input != "summarise the logs" || rec.Trigger != "app" {
		t.Fatalf("record = %+v (ok=%v), want the cut turn's input and trigger", rec, ok)
	}
}
