package agent

import (
	"bytes"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/usagehold"
)

// heldStoreAgent is a started delegated agent on reg whose state lives in
// idx, as one gateway process would have it.
func heldStoreAgent(t *testing.T, reg *usagehold.Registry, idx *session.SessionIndex) (*Agent, func()) {
	t.Helper()
	a, cancel := startedAgent(t)
	a.AgentID = "test"
	a.SessionIndex = idx
	a.UsageHoldRegistry = reg
	a.DelegatedManager = &DelegatedManager{BackendType: "claude-code"}
	a.StartUsageHold()
	return a, func() { a.StopUsageHold(); cancel() }
}

// TestUsageHold_HeldInputSurvivesRestart is holdout H7: held user messages —
// two texts and one attachment — held across a simulated foci restart reach
// the lift turn whole, and leave nothing stored behind once they ran.
func TestUsageHold_HeldInputSurvivesRestart(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()
	reg := usagehold.New()
	const sk = "test/c1"
	pic := platform.Attachment{Type: "photo", Data: []byte{0x89, 'P', 'N', 'G'}, MimeType: "image/png"}

	// First process: the hold parks M1 (with the attachment) and M2.
	a1, stop1 := heldStoreAgent(t, reg, idx)
	d1 := &recordingDriver{}
	a1.SetTurnObserver(d1.recordBatch)
	a1.ObserveUsage(weekly(0.98))
	a1.Enqueue(Envelope{SessionKey: sk, Text: "M1", Attachments: []platform.Attachment{pic}, UserID: "u1", SenderName: "Dick", Driver: d1})
	a1.Enqueue(Envelope{SessionKey: sk, Text: "M2", UserID: "u1", Driver: d1})
	stored := func() int {
		a1.usageHold.storeMu.Lock()
		defer a1.usageHold.storeMu.Unlock()
		return len(a1.loadUsageHeldLocked(sk))
	}
	if !waitFor(time.Second, func() bool { return stored() == 2 }) {
		t.Fatalf("stored held input = %d, want 2", stored())
	}
	stop1() // the process goes away with both messages held
	if d1.NumCalls() != 0 {
		t.Fatalf("a held message ran before the restart: %d turns", d1.NumCalls())
	}

	// Second process: the restore puts both back; still held, nothing runs.
	a2, stop2 := heldStoreAgent(t, reg, idx)
	defer stop2()
	d2 := &recordingDriver{}
	a2.SetTurnObserver(d2.recordBatch)
	a2.UsageHeldDriver = func(string) Driver { return d2 }
	a2.RestoreUsageHeld()
	if waitFor(300*time.Millisecond, func() bool { return d2.NumCalls() > 0 }) {
		t.Fatal("a restored message ran while the hold is still on")
	}

	a2.ObserveUsage(weekly(0.10))
	if !waitFor(2*time.Second, func() bool { return d2.NumCalls() == 1 }) {
		t.Fatalf("restored messages did not run at lift; turns=%d", d2.NumCalls())
	}
	got := d2.Calls()[0]
	if len(got) != 2 || got[0].Text != "M1" || got[1].Text != "M2" {
		t.Fatalf("lift turn = %+v, want M1 then M2 in one turn", got)
	}
	if len(got[0].Attachments) != 1 || !bytes.Equal(got[0].Attachments[0].Data, pic.Data) || got[0].Attachments[0].MimeType != pic.MimeType {
		t.Errorf("M1 attachments after restart = %+v, want the original photo", got[0].Attachments)
	}
	if got[0].SenderName != "Dick" || got[0].UserID != "u1" {
		t.Errorf("M1 sender after restart = %q/%q, want Dick/u1", got[0].SenderName, got[0].UserID)
	}
	a2.usageHold.storeMu.Lock()
	left := a2.loadUsageHeldLocked(sk)
	a2.usageHold.storeMu.Unlock()
	if len(left) != 0 {
		t.Errorf("stored held input after the lift turn = %+v, want none", left)
	}
}

// TestUsageHold_DurableInjectionSurvivesRestart: a parked injection with a
// serialisable form is stored, rebuilt through UsageHeldInjectRestore after a
// restart under its id, and leaves the store once it runs.
func TestUsageHold_DurableInjectionSurvivesRestart(t *testing.T) {
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()
	reg := usagehold.New()
	const sk = "test/c1"

	a1, stop1 := heldStoreAgent(t, reg, idx)
	a1.ObserveUsage(weekly(0.98))
	durable := &DurableInject{Kind: "session_chat", Trigger: "agent", Text: "hello from B"}
	a1.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{Trigger: "agent", Durable: durable, Run: func() {}}})
	stored := func(a *Agent) []usageHeldRecord {
		a.usageHold.storeMu.Lock()
		defer a.usageHold.storeMu.Unlock()
		return a.loadUsageHeldLocked(sk)
	}
	if !waitFor(time.Second, func() bool { return len(stored(a1)) == 1 }) {
		t.Fatalf("stored = %+v, want the held injection", stored(a1))
	}
	stop1()

	a2, stop2 := heldStoreAgent(t, reg, idx)
	defer stop2()
	var mu sync.Mutex
	var rebuilt []DurableInject
	var ran bool
	a2.UsageHeldInjectRestore = func(sk string, d DurableInject, id string) {
		mu.Lock()
		rebuilt = append(rebuilt, d)
		mu.Unlock()
		env := Envelope{SessionKey: sk, Inject: &InjectMeta{Trigger: d.Trigger, Durable: &d, Run: func() { mu.Lock(); ran = true; mu.Unlock() }}}
		env.SetUsageHeldID(id)
		a2.Enqueue(env)
	}
	a2.RestoreUsageHeld()
	mu.Lock()
	if len(rebuilt) != 1 || rebuilt[0].Kind != durable.Kind || rebuilt[0].Trigger != durable.Trigger || rebuilt[0].Text != durable.Text {
		t.Fatalf("rebuilt = %+v, want %+v", rebuilt, *durable)
	}
	mu.Unlock()
	if got := stored(a2); len(got) != 1 {
		t.Errorf("parked again after restore: stored %d records, want 1 (no duplicate)", len(got))
	}
	a2.ObserveUsage(weekly(0.10))
	if !waitFor(2*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return ran }) {
		t.Fatal("the restored injection did not run at lift")
	}
	if !waitFor(time.Second, func() bool { return len(stored(a2)) == 0 }) {
		t.Errorf("stored after it ran = %+v, want none", stored(a2))
	}
}
