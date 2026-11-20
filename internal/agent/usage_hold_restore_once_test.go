package agent

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/session"
	"foci/internal/usagehold"
)

// #3197 (work Mac, 2026-11-20): after a restart under a usage hold, the
// queued messages the startup re-queue put back (RestoreCutTurns) were parked
// by the worker under new held ids; RestoreUsageHeld then read those new
// records back and parked each a second time, so the lift ran them twice.
// And an ask answer waiting at a gate had no stored form at all: a restart
// lost it.

func restoreOnceIdx(t *testing.T) *session.SessionIndex {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx
}

func storedHeld(a *Agent, sk string) []usageHeldRecord {
	a.usageHold.storeMu.Lock()
	defer a.usageHold.storeMu.Unlock()
	return a.loadUsageHeldLocked(sk)
}

// TestUsageHold_QueuedInputRestoredOnceUnderHold: three queued messages a
// restart stranded, restored by the gateway's startup order (RestoreCutTurns,
// then RestoreUsageHeld) while the hold is on, run once each at the lift. The
// worker parks all three before RestoreUsageHeld reads the store: the worst
// case of the race seen on the work Mac.
func TestUsageHold_QueuedInputRestoredOnceUnderHold(t *testing.T) {
	idx := restoreOnceIdx(t)
	reg := usagehold.New()
	const sk = "test/c1"
	const n = 3

	// The last process left three queued-input records.
	seed, stopSeed := heldStoreAgent(t, reg, idx)
	var recs []usageHeldRecord
	for i := 0; i < n; i++ {
		recs = append(recs, usageHeldRecord{ID: fmt.Sprintf("qi-1-%d", i), Kind: usageHeldUser, Text: fmt.Sprintf("M%d", i), UserID: "u1", ReceivedAt: time.Now()})
	}
	seed.queuedInputMu.Lock()
	seed.saveQueuedInputLocked(sk, recs)
	seed.queuedInputMu.Unlock()
	stopSeed()

	a, stop := heldStoreAgent(t, reg, idx)
	defer stop()
	d := &recordingDriver{}
	a.SetTurnObserver(d.recordBatch)
	a.UsageHeldDriver = func(string) Driver { return d }
	a.RestartResumeNotice = func(string, string) {}
	testAcct(a).ObserveUsage(weekly(0.98)) // the hold is on

	a.RestoreCutTurns()
	if !waitFor(2*time.Second, func() bool { return len(storedHeld(a, sk)) == n }) {
		t.Fatalf("restored queued input not parked: stored=%d want %d", len(storedHeld(a, sk)), n)
	}
	a.RestoreUsageHeld()
	time.Sleep(200 * time.Millisecond) // a second park, if any, lands now
	if got := a.lookupInbox(sk).usageHeldLen(); got != n {
		t.Errorf("held in memory = %d, want %d", got, n)
	}

	testAcct(a).ObserveUsage(weekly(0.10)) // lift
	if !waitFor(2*time.Second, func() bool { return d.NumCalls() >= 1 }) {
		t.Fatal("nothing ran at the lift")
	}
	time.Sleep(200 * time.Millisecond)
	var texts []string
	for _, b := range d.Calls() {
		for _, e := range b {
			texts = append(texts, e.Text)
		}
	}
	if fmt.Sprint(texts) != "[M0 M1 M2]" {
		t.Errorf("lift ran %q, want each queued message once: [M0 M1 M2]", texts)
	}
	if left := storedHeld(a, sk); len(left) != 0 {
		t.Errorf("stored held input after the lift = %+v, want none", left)
	}
}

// TestParkUsageHeld_DropsDuplicateRestoredCopy: a restored copy of a held id
// already parked is not parked again, whatever brought it back twice.
func TestParkUsageHeld_DropsDuplicateRestoredCopy(t *testing.T) {
	idx := restoreOnceIdx(t)
	reg := usagehold.New()
	const sk = "test/c1"
	a, stop := heldStoreAgent(t, reg, idx)
	defer stop()
	d := &recordingDriver{}
	testAcct(a).ObserveUsage(weekly(0.98))

	env := Envelope{SessionKey: sk, Text: "M", Driver: d}
	env.SetUsageHeldID("uh-1-1")
	a.Enqueue(env)
	a.Enqueue(env)
	if !waitFor(2*time.Second, func() bool { return a.lookupInbox(sk).usageHeldLen() >= 1 }) {
		t.Fatal("the restored copy was not parked")
	}
	time.Sleep(200 * time.Millisecond)
	if got := a.lookupInbox(sk).usageHeldLen(); got != 1 {
		t.Errorf("held in memory = %d, want 1", got)
	}
}

// heldAskAnswer is an ask answer as the gateway builds it (asUserInput and
// userInputDurable): user input with a stored form.
func heldAskAnswer(sk string, ran *atomic.Int32) Envelope {
	return Envelope{SessionKey: sk, Text: "ANSWER", Inject: &InjectMeta{
		Trigger: "ask_grader", UserInput: true, AskReqID: "ask-1",
		Durable: &DurableInject{Kind: "session_chat", Trigger: "ask_grader", Text: "ANSWER", Args: map[string]string{"user_input": "true", "ask_req_id": "ask-1"}},
		Run:     func() { ran.Add(1) },
	}}
}

// restoreAskAnswer wires a's inject restore as the gateway does for an ask
// answer: rebuilt as user input under its stored id. It records what it
// rebuilt.
func restoreAskAnswer(a *Agent, ran *atomic.Int32) func() []DurableInject {
	var mu sync.Mutex
	var rebuilt []DurableInject
	a.UsageHeldInjectRestore = func(sk string, d DurableInject, id string) {
		mu.Lock()
		rebuilt = append(rebuilt, d)
		mu.Unlock()
		env := Envelope{SessionKey: sk, Text: d.Text, Inject: &InjectMeta{
			Trigger: d.Trigger, UserInput: d.Args["user_input"] == "true", AskReqID: d.Args["ask_req_id"],
			Durable: &d, Run: func() { ran.Add(1) },
		}}
		env.SetUsageHeldID(id)
		a.Enqueue(env)
	}
	return func() []DurableInject {
		mu.Lock()
		defer mu.Unlock()
		return append([]DurableInject(nil), rebuilt...)
	}
}

// TestUsageHold_HeldAskAnswerSurvivesRestart: an ask answer the hold parks is
// stored, comes back after a restart as user input, runs once at the lift and
// leaves the store.
func TestUsageHold_HeldAskAnswerSurvivesRestart(t *testing.T) {
	idx := restoreOnceIdx(t)
	reg := usagehold.New()
	const sk = "test/c1"

	a1, stop1 := heldStoreAgent(t, reg, idx)
	testAcct(a1).ObserveUsage(weekly(0.98))
	var ran1 atomic.Int32
	if !a1.Enqueue(heldAskAnswer(sk, &ran1)) {
		t.Fatal("the answer was refused")
	}
	if !waitFor(2*time.Second, func() bool { return a1.lookupInbox(sk).userInjectsHeld.Load() == 1 }) {
		t.Fatal("the answer was not parked by the hold")
	}
	if got := storedHeld(a1, sk); len(got) != 1 {
		t.Fatalf("stored = %+v, want the held answer", got)
	}
	stop1()
	if ran1.Load() != 0 {
		t.Fatal("the answer ran during the hold")
	}

	a2, stop2 := heldStoreAgent(t, reg, idx)
	defer stop2()
	var ran2 atomic.Int32
	rebuilt := restoreAskAnswer(a2, &ran2)
	a2.RestoreUsageHeld()
	got := rebuilt()
	if len(got) != 1 || got[0].Text != "ANSWER" || got[0].Args["user_input"] != "true" || got[0].Args["ask_req_id"] != "ask-1" {
		t.Fatalf("rebuilt = %+v, want the answer as user input with its ask id", got)
	}
	if waitFor(300*time.Millisecond, func() bool { return ran2.Load() > 0 }) {
		t.Fatal("the restored answer ran while the hold is still on")
	}
	testAcct(a2).ObserveUsage(weekly(0.10))
	if !waitFor(3*time.Second, func() bool { return ran2.Load() == 1 }) {
		t.Fatal("the restored answer did not run at the lift")
	}
	if !waitFor(time.Second, func() bool { return len(storedHeld(a2, sk)) == 0 }) {
		t.Errorf("stored after it ran = %+v, want none", storedHeld(a2, sk))
	}
	time.Sleep(200 * time.Millisecond)
	if ran2.Load() != 1 {
		t.Errorf("the restored answer ran %d times, want 1", ran2.Load())
	}
}

// TestAskAnswerWaitingAtCompactionSurvivesRestart: with no usage hold, an
// ask answer waiting out a compaction is stored too (Dick 2026-11-20: "Any
// gate"), and comes back after a restart.
func TestAskAnswerWaitingAtCompactionSurvivesRestart(t *testing.T) {
	idx := restoreOnceIdx(t)
	reg := usagehold.New()
	const sk = "test/c1"

	a1, stop1 := heldStoreAgent(t, reg, idx)
	a1.markCompacting(sk)
	var ran1 atomic.Int32
	if !a1.Enqueue(heldAskAnswer(sk, &ran1)) {
		t.Fatal("the answer was refused")
	}
	if !waitFor(2*time.Second, func() bool { return !a1.InboxHasPendingInput(sk) }) {
		t.Fatal("the worker never took the answer")
	}
	if got := storedHeld(a1, sk); len(got) != 1 {
		t.Fatalf("stored while it waits at the compaction gate = %+v, want the answer", got)
	}
	stop1()
	if ran1.Load() != 0 {
		t.Fatal("the answer ran during the compaction")
	}

	a2, stop2 := heldStoreAgent(t, reg, idx)
	defer stop2()
	var ran2 atomic.Int32
	rebuilt := restoreAskAnswer(a2, &ran2)
	a2.RestoreUsageHeld()
	if got := rebuilt(); len(got) != 1 {
		t.Fatalf("rebuilt = %+v, want the answer", got)
	}
	if !waitFor(3*time.Second, func() bool { return ran2.Load() == 1 }) {
		t.Fatal("the restored answer did not run")
	}
	if !waitFor(time.Second, func() bool { return len(storedHeld(a2, sk)) == 0 }) {
		t.Errorf("stored after it ran = %+v, want none", storedHeld(a2, sk))
	}
}

// TestAskAnswerRunNowLeavesNothingStored: an answer no gate holds runs and
// leaves no record, so the next restart does not run it again.
func TestAskAnswerRunNowLeavesNothingStored(t *testing.T) {
	idx := restoreOnceIdx(t)
	reg := usagehold.New()
	const sk = "test/c1"
	a, stop := heldStoreAgent(t, reg, idx)
	defer stop()
	var ran atomic.Int32
	a.Enqueue(heldAskAnswer(sk, &ran))
	if !waitFor(2*time.Second, func() bool { return ran.Load() == 1 }) {
		t.Fatal("the answer did not run")
	}
	if got := storedHeld(a, sk); len(got) != 0 {
		t.Errorf("stored after it ran = %+v, want none", got)
	}
}
