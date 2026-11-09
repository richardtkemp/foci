package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
)

// deliveryRecorder records Agent.OnInputDelivery reports.
type deliveryRecorder struct {
	mu  sync.Mutex
	got []string // "<msgID> <outcome> <turn>"
}

func (d *deliveryRecorder) hook(_ string, refs []delegator.InputRef, outcome, turnID string, _ time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range refs {
		d.got = append(d.got, r.MessageID+" "+outcome+" "+turnID)
	}
}

func (d *deliveryRecorder) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.got...)
}

func ref(id string) delegator.InputRef {
	return delegator.InputRef{ConversationID: "c1", MessageID: id}
}

// TestLiveTurn (#2992): the registry names the turn running on a session, and
// a turn clears only its own id, so a newer turn survives an older one ending.
func TestLiveTurn(t *testing.T) {
	a := &Agent{}
	a.setLiveTurn("s", "s@1")
	if got := a.LiveTurnID("s"); got != "s@1" {
		t.Fatalf("live = %q", got)
	}
	a.setLiveTurn("s", "s@2")
	a.clearLiveTurn("s", "s@1")
	if got := a.LiveTurnID("s"); got != "s@2" {
		t.Errorf("an older turn's end cleared the newer one: live = %q", got)
	}
	a.clearLiveTurn("s", "s@2")
	if got := a.LiveTurnID("s"); got != "" {
		t.Errorf("live after the end = %q", got)
	}
}

// TestInbox_ReportsDelivery (#2992): the inbox records how a message reached
// the agent where it decides it. An urgent steer names the running turn; a
// message buffered on an API agent is queued, then steered when the buffer is
// drained at a tool boundary; a message that waits for a running turn is
// queued; an idle message reports nothing until its turn starts.
func TestInbox_ReportsDelivery(t *testing.T) {
	t.Run("urgent steer", func(t *testing.T) {
		a, cancel := startedAgent(t)
		defer cancel()
		var rec deliveryRecorder
		a.OnInputDelivery = rec.hook
		a.SetInboxSteerMode(true)
		a.SetInboxBackend(func(context.Context, string) (delegator.Delegator, error) { return &recordingBackend{}, nil })
		a.getOrCreateInbox("test/s").turnActive.Store(true)
		a.setLiveTurn("test/s", "test/s@1")
		a.Enqueue(Envelope{SessionKey: "test/s", Text: "ping", Ref: ref("m1")})
		if got := rec.all(); len(got) != 1 || got[0] != "m1 steered test/s@1" {
			t.Errorf("reports = %v, want m1 steered into test/s@1", got)
		}
	})
	t.Run("API buffer, drained", func(t *testing.T) {
		a, cancel := startedAgent(t)
		defer cancel()
		var rec deliveryRecorder
		a.OnInputDelivery = rec.hook
		a.SetInboxSteerMode(true)
		inb := a.getOrCreateInbox("test/s")
		inb.turnActive.Store(true)
		a.setLiveTurn("test/s", "test/s@1")
		a.Enqueue(Envelope{SessionKey: "test/s", Text: "buffered", Ref: ref("m2")})
		if texts := a.DrainInboxSteerTexts("test/s"); len(texts) != 1 {
			t.Fatalf("drained %v", texts)
		}
		if got := rec.all(); len(got) != 2 || got[0] != "m2 queued " || got[1] != "m2 steered test/s@1" {
			t.Errorf("reports = %v, want queued then steered into test/s@1", got)
		}
	})
	t.Run("waits for the running turn", func(t *testing.T) {
		a := newTestAgent(t) // no worker: the message stays queued
		var rec deliveryRecorder
		a.OnInputDelivery = rec.hook
		a.getOrCreateInbox("test/s").turnActive.Store(true) // steer mode off
		a.Enqueue(Envelope{SessionKey: "test/s", Text: "later", Ref: ref("m3")})
		if got := rec.all(); len(got) != 1 || got[0] != "m3 queued " {
			t.Errorf("reports = %v, want m3 queued", got)
		}
	})
	t.Run("idle", func(t *testing.T) {
		a := newTestAgent(t)
		var rec deliveryRecorder
		a.OnInputDelivery = rec.hook
		a.Enqueue(Envelope{SessionKey: "test/s", Text: "hello", Ref: ref("m4")})
		if got := rec.all(); len(got) != 0 {
			t.Errorf("reports = %v, want none before its turn starts", got)
		}
	})
}

// TestRunInference_ReportsDelivery (#2992): the delegated transport records
// the outcome where it chooses it: a fresh turn is started (and is the live
// turn while it runs), a message arriving while the backend runs a user turn
// joins it, and an answer to the running turn's prompt joins it.
func TestRunInference_ReportsDelivery(t *testing.T) {
	run := func(t *testing.T, a *Agent, msgID string) *TurnState {
		t.Helper()
		ctx := WithInputRefs(interactiveTestCtx(), []delegator.InputRef{ref(msgID)})
		ts := NewTurnState(ctx, "test/s", []string{"hi"}, nil)
		ts.Prompt, ts.Trigger, ts.StartedAt = "hi", TriggerFromContext(ctx), time.Now()
		if err := (&DelegatedTransport{sharedTurnOps{agent: a}}).RunInference(ts); err != nil {
			t.Fatalf("RunInference: %v", err)
		}
		return ts
	}
	t.Run("started", func(t *testing.T) {
		var liveDuring string
		be := &mockBackendDT{}
		var a *Agent
		be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
			liveDuring = a.LiveTurnID("test/s")
			if h != nil && h.OnTurnComplete != nil {
				h.OnTurnComplete(&delegator.TurnResult{Text: "done"})
			}
			return nil, nil
		}
		a = &Agent{Model: "m", DelegatedManager: newMockDelegatedManager(t, be)}
		var rec deliveryRecorder
		a.OnInputDelivery = rec.hook
		ts := run(t, a, "m5")
		if got := rec.all(); len(got) != 1 || got[0] != "m5 started "+ts.RowID() {
			t.Errorf("reports = %v, want m5 started %s", got, ts.RowID())
		}
		if liveDuring != ts.RowID() {
			t.Errorf("live turn while the backend ran = %q, want %q (a spawn needs its parent)", liveDuring, ts.RowID())
		}
	})
	t.Run("joined", func(t *testing.T) {
		be := &mockBackendDT{turnInFlight: true}
		a := &Agent{Model: "m", DelegatedManager: newMockDelegatedManager(t, be)}
		var rec deliveryRecorder
		a.OnInputDelivery = rec.hook
		a.setLiveTurn("test/s", "test/s@1")
		ts := run(t, a, "m6")
		if !ts.foldedFollowUp {
			t.Fatal("the message did not fold into the running turn")
		}
		if got := rec.all(); len(got) != 1 || got[0] != "m6 joined test/s@1" {
			t.Errorf("reports = %v, want m6 joined test/s@1", got)
		}
	})
	t.Run("answered the prompt", func(t *testing.T) {
		be := &refusingQuestionBackend{pending: "req-1", answered: make(chan string, 1)}
		a := &Agent{Model: "m", DelegatedManager: newMockDelegatedManager(t, be)}
		var rec deliveryRecorder
		a.OnInputDelivery = rec.hook
		a.setLiveTurn("test/s", "test/s@1")
		run(t, a, "m7")
		if got := rec.all(); len(got) != 1 || got[0] != "m7 joined test/s@1" {
			t.Errorf("reports = %v, want m7 joined test/s@1", got)
		}
	})
}

// TestAPIRegisterTurn_ReportsStarted (#2992): on the API transport a turn's
// input is handed to the model once the session lock is held; that is where
// it is reported started and becomes the live turn.
func TestAPIRegisterTurn_ReportsStarted(t *testing.T) {
	a := &Agent{}
	var rec deliveryRecorder
	a.OnInputDelivery = rec.hook
	ctx := WithInputRefs(context.Background(), []delegator.InputRef{ref("m8")})
	ts := NewTurnState(ctx, "test/s", []string{"hi"}, nil)
	ts.StartedAt = time.Now()
	unreg := (&APITransport{sharedTurnOps{agent: a}}).RegisterTurn(ts)
	defer unreg()
	if got := rec.all(); len(got) != 1 || got[0] != "m8 started "+ts.RowID() {
		t.Errorf("reports = %v, want m8 started %s", got, ts.RowID())
	}
	if a.LiveTurnID("test/s") != ts.RowID() {
		t.Errorf("live = %q, want %q", a.LiveTurnID("test/s"), ts.RowID())
	}
	if a.turnDispatchedAt(ts).IsZero() {
		t.Error("an API turn has no dispatch time after its lock")
	}
}
