package agent

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"foci/internal/session"
	"foci/internal/usagehold"
)

// resumeTrigger is the trigger and [meta] via RunTurn gives batch: from its
// driving connection's platform, as for any user turn.
func resumeTrigger(batch []Envelope) (trigger, via string) {
	name := ""
	if c := batch[0].Driver.Connection(); c != nil {
		name = c.PlatformName()
	}
	trigger = batchTrigger(name, batch)
	return trigger, triggerToPlatform(trigger)
}

// TestUsageHold_ResumeLineIsAUserMessage is the 2026-11-02 review point on
// Dick's ruling ("simulate a user message saying \"resume what you were
// doing\" - do not show this in the clients"): the resume line reaches the
// model with a user turn's trigger and via — the session's platform, not an
// external send — both alone and folded with held messages, and it stays
// hidden (the leading-hidden mark).
func TestUsageHold_ResumeLineIsAUserMessage(t *testing.T) {
	for _, withHeld := range []bool{false, true} {
		name := "alone"
		if withHeld {
			name = "with held messages"
		}
		t.Run(name, func(t *testing.T) {
			a, cancel := startedAgent(t)
			defer cancel()
			a.UsageHoldRegistry = usagehold.New()
			a.DelegatedManager = &DelegatedManager{BackendType: "claude-code"}
			a.StartUsageHold()
			defer a.StopUsageHold()
			d := newBlockingDriver()
			a.SetTurnObserver(d.record)
			app := &appSourceDriver{conn: appConnStub{&recordingConn{}}}
			a.UsageHeldDriver = func(string) Driver { return app }
			held := make(chan struct{}, 4)
			a.TurnQueuedNotifyFunc = func(string, string) { held <- struct{}{} }

			const sk = "test/c1"
			a.Enqueue(Envelope{SessionKey: sk, Text: "M0", Driver: d})
			select {
			case <-d.started:
			case <-time.After(2 * time.Second):
				t.Fatal("the first turn never started")
			}
			a.ObserveUsage(weekly(0.98))
			select {
			case <-d.ended:
			case <-time.After(2 * time.Second):
				t.Fatal("the turn in flight was not interrupted")
			}
			if withHeld {
				a.Enqueue(Envelope{SessionKey: sk, Text: "M1", Driver: app})
				select {
				case <-held: // M1 is parked
				case <-time.After(2 * time.Second):
					t.Fatal("M1 was not held")
				}
			}
			a.ObserveUsage(weekly(0.10))
			if !waitFor(2*time.Second, func() bool { return len(d.Calls()) == 2 }) {
				t.Fatalf("turns = %d, want the interrupted one and the resume turn", len(d.Calls()))
			}
			got := d.Calls()[1]
			if !got[0].usageResume || got[0].Text != UsageResumeLine {
				t.Fatalf("resume turn head = %+v, want the resume line", got[0])
			}
			if withHeld && (len(got) != 2 || got[1].Text != "M1") {
				t.Errorf("resume turn = %+v, want the resume line then M1", got)
			}
			userTrigger, userVia := resumeTrigger([]Envelope{{SessionKey: sk, Text: "hi", Driver: app}})
			if trigger, via := resumeTrigger(got); trigger != "app" || trigger != userTrigger || via != userVia || via == "external" {
				t.Errorf("resume turn trigger/via = %q/%q, want a user turn's %q/%q (not an external send)", trigger, via, userTrigger, userVia)
			}
			if leadingUsageResume(got) != 1 {
				t.Error("the resume line is not marked hidden")
			}
		})
	}
}

// TestUsageHold_PausedBackgroundSurvivesRestart is the 2026-11-02 review
// point on the background-work ruling ("must stop and be resumed"): a
// background-work turn the hold paused, with foci restarting during the
// hold, resumes at the lift with the resume line on its own session, and its
// record is cleared once it ran on.
func TestUsageHold_PausedBackgroundSurvivesRestart(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	reg := usagehold.New()
	pb := PausedBackground{Key: "test/c1/b4", Strategy: "fork", BranchType: "background"}

	// First process: the background turn runs, the hold interrupts it, and
	// foci stops during the pause.
	a1, stop1 := heldStoreAgent(t, reg, idx)
	started := make(chan struct{})
	ctx1, cancel1 := context.WithCancel(context.Background())
	ret := make(chan error, 1)
	go func() {
		ret <- a1.RunBackgroundResumable(ctx1, pb, "do the todo", func(c context.Context, _ string) error {
			close(started)
			<-c.Done()
			return c.Err()
		})
	}()
	<-started
	a1.ObserveUsage(weekly(0.98))
	if !waitFor(2*time.Second, func() bool { return a1.PausedBackgroundRecorded(pb.Key) }) {
		t.Fatal("the paused background turn left no record")
	}
	if a1.UsageHoldPaused()[pb.Key] != "background" {
		t.Errorf("paused set = %v, want the branch as background", a1.UsageHoldPaused())
	}
	cancel1() // shutdown during the pause
	if err := <-ret; !errors.Is(err, context.Canceled) {
		t.Errorf("RunBackgroundResumable = %v, want the shutdown's cancel", err)
	}
	stop1()
	if !a1.PausedBackgroundRecorded(pb.Key) {
		t.Fatal("shutdown during the pause dropped the record")
	}

	// Second process: the restore finds it; it waits for the lift.
	a2, stop2 := heldStoreAgent(t, reg, idx)
	defer stop2()
	var mu sync.Mutex
	var texts []string
	resumed := make(chan struct{})
	a2.RestorePausedBackground(func(ctx context.Context, got PausedBackground) {
		if got != pb {
			t.Errorf("restored %+v, want %+v", got, pb)
		}
		go func() {
			_ = a2.ResumePausedBackground(ctx, got, func(_ context.Context, text string) error {
				mu.Lock()
				texts = append(texts, text)
				mu.Unlock()
				close(resumed)
				return nil
			})
		}()
	})
	if !waitFor(time.Second, func() bool { return a2.UsageHoldPaused()[pb.Key] == "background" }) {
		t.Error("the restored paused branch is not in the paused (keepalive) set")
	}
	select {
	case <-resumed:
		t.Fatal("the restored background turn ran during the hold")
	default:
	}
	a2.ObserveUsage(weekly(0.10))
	select {
	case <-resumed:
	case <-time.After(2 * time.Second):
		t.Fatal("the restored background turn did not resume at the lift")
	}
	mu.Lock()
	if len(texts) != 1 || texts[0] != UsageResumeLine {
		t.Errorf("resumed with %q, want the resume line", texts)
	}
	mu.Unlock()
	if !waitFor(time.Second, func() bool { return !a2.PausedBackgroundRecorded(pb.Key) }) {
		t.Error("the record is still there after the resume ran")
	}
}

// TestAgent_WaitTurnIdle: WaitTurnIdle returns when the session's turn ends
// (the in-flight signal), and false when its ctx ends first.
func TestAgent_WaitTurnIdle(t *testing.T) {
	a := &Agent{}
	const sk = "test/c1"
	a.SetTurnInFlightForTest(sk, true)
	done := make(chan bool, 1)
	go func() { done <- a.WaitTurnIdle(context.Background(), sk) }()
	select {
	case <-done:
		t.Fatal("WaitTurnIdle returned with the turn in flight")
	case <-time.After(50 * time.Millisecond):
	}
	a.SetTurnInFlightForTest(sk, false)
	if !<-done {
		t.Error("WaitTurnIdle = false after the turn ended")
	}
	a.SetTurnInFlightForTest(sk, true)
	if a.WaitTurnIdle(canceledCtx(), sk) {
		t.Error("WaitTurnIdle = true with the turn still in flight and ctx done")
	}
}
