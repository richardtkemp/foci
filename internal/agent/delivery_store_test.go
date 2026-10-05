package agent

import (
	"context"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// trackingMockDM is a mockBackendDM that implements delegator.DeliveryTracker,
// exposing the hooks the manager installs so a test can play the backend's
// side of delivery tracking (#2050).
type trackingMockDM struct {
	*mockBackendDM
	hmu   sync.Mutex
	hooks delegator.DeliveryHooks
	// onClose, if set, runs inside Close — where a real backend's
	// finalizeExit hands back what the process never consumed.
	onClose func(h delegator.DeliveryHooks)
}

func (m *trackingMockDM) SetDeliveryHooks(h delegator.DeliveryHooks) {
	m.hmu.Lock()
	m.hooks = h
	m.hmu.Unlock()
}

func (m *trackingMockDM) h() delegator.DeliveryHooks {
	m.hmu.Lock()
	defer m.hmu.Unlock()
	return m.hooks
}

func (m *trackingMockDM) Close() error {
	if m.onClose != nil {
		m.onClose(m.h())
	}
	return m.mockBackendDM.Close()
}

type redeliveryLog struct {
	mu       sync.Mutex
	redeliv  []Redelivery
	consumed [][]delegator.InputRef
}

func newDeliveryManager(t *testing.T) (*DelegatedManager, *[]*trackingMockDM, *redeliveryLog) {
	t.Helper()
	var mocks []*trackingMockDM
	lg := &redeliveryLog{}
	mgr, _ := newTestManager(t, newTestSessionIndex(t))
	mgr.NewBackend = func() (delegator.Delegator, error) {
		be := &trackingMockDM{mockBackendDM: &mockBackendDM{running: true}}
		mocks = append(mocks, be)
		return be, nil
	}
	mgr.Redeliver = func(_ string, r Redelivery) {
		lg.mu.Lock()
		lg.redeliv = append(lg.redeliv, r)
		lg.mu.Unlock()
	}
	mgr.InputConsumed = func(_ string, refs []delegator.InputRef) {
		lg.mu.Lock()
		lg.consumed = append(lg.consumed, refs)
		lg.mu.Unlock()
	}
	return mgr, &mocks, lg
}

const deliverySK = "test-agent/c1"

func TestDeliveryStore_PendingThenConsumed(t *testing.T) {
	mgr, mocks, lg := newDeliveryManager(t)
	if _, err := mgr.Get(context.Background(), deliverySK); err != nil {
		t.Fatal(err)
	}
	h := (*mocks)[0].h()
	ref := delegator.InputRef{ConversationID: "conv", MessageID: "msg"}
	h.OnPending(delegator.PendingInput{ID: "a", Text: "hello", Refs: []delegator.InputRef{ref}})
	h.OnPending(delegator.PendingInput{ID: "b", Text: "nudge"})
	if !mgr.stillUndelivered(deliverySK, "a") || !mgr.stillUndelivered(deliverySK, "b") {
		t.Fatal("pending inputs were not persisted")
	}

	h.OnConsumed(delegator.PendingInput{ID: "a", Refs: []delegator.InputRef{ref}})
	h.OnConsumed(delegator.PendingInput{ID: "b"})
	if mgr.stillUndelivered(deliverySK, "a") || mgr.stillUndelivered(deliverySK, "b") {
		t.Fatal("consumed inputs are still persisted")
	}
	if len(lg.consumed) != 1 || lg.consumed[0][0] != ref {
		t.Fatalf("InputConsumed calls = %v, want exactly one, for the input with a ref", lg.consumed)
	}
}

func TestDeliveryStore_UndeliveredIsRedelivered(t *testing.T) {
	mgr, mocks, lg := newDeliveryManager(t)
	if _, err := mgr.Get(context.Background(), deliverySK); err != nil {
		t.Fatal(err)
	}
	h := (*mocks)[0].h()
	p := delegator.PendingInput{ID: "s", Text: "steer", Source: "steer"}
	h.OnPending(p)
	p.Redeliveries = 1 // the backend bumps it when it hands back
	h.OnUndelivered([]delegator.PendingInput{p})

	if len(lg.redeliv) != 1 || lg.redeliv[0].Input.ID != "s" || lg.redeliv[0].Reason != "cc-exit" {
		t.Fatalf("redeliveries = %+v, want one cc-exit redelivery of s", lg.redeliv)
	}
	// Still persisted until the redelivery is written and consumed.
	if !mgr.stillUndelivered(deliverySK, "s") {
		t.Fatal("handed-back input no longer persisted — a restart before its redelivery would lose it")
	}
}

// /reset means stop: inputs its process never consumed are dropped, and one
// already handed back and queued for redelivery no longer counts as pending.
func TestDeliveryStore_ResetDrops(t *testing.T) {
	mgr, mocks, lg := newDeliveryManager(t)
	if _, err := mgr.Get(context.Background(), deliverySK); err != nil {
		t.Fatal(err)
	}
	be := (*mocks)[0]
	h := be.h()
	queued := delegator.PendingInput{ID: "queued", Text: "handed back earlier"}
	h.OnPending(queued)
	held := delegator.PendingInput{ID: "held", Text: "still in the process"}
	h.OnPending(held)
	be.onClose = func(h delegator.DeliveryHooks) { h.OnUndelivered([]delegator.PendingInput{held}) }

	mgr.ResetSession(deliverySK)

	if len(lg.redeliv) != 0 {
		t.Fatalf("reset redelivered %+v, want nothing", lg.redeliv)
	}
	if mgr.stillUndelivered(deliverySK, "held") || mgr.stillUndelivered(deliverySK, "queued") {
		t.Fatal("reset left inputs persisted")
	}
}

func TestDeliveryStore_RemapMovesPending(t *testing.T) {
	mgr, mocks, _ := newDeliveryManager(t)
	if _, err := mgr.Get(context.Background(), deliverySK); err != nil {
		t.Fatal(err)
	}
	(*mocks)[0].h().OnPending(delegator.PendingInput{ID: "x"})
	mgr.RemapSession(deliverySK, "test-agent/c2")
	if mgr.stillUndelivered(deliverySK, "x") || !mgr.stillUndelivered("test-agent/c2", "x") {
		t.Fatal("pending input did not follow the remapped session")
	}
}

func TestRestoreUndelivered(t *testing.T) {
	mgr, _, lg := newDeliveryManager(t)
	ref := delegator.InputRef{ConversationID: "conv", MessageID: "folded-msg"}
	mgr.upsertUndelivered(deliverySK, delegator.PendingInput{ID: "folded", WorkDir: "/w", SessionID: "s1", Refs: []delegator.InputRef{ref}})
	mgr.upsertUndelivered(deliverySK, delegator.PendingInput{ID: "lost", WorkDir: "/w", SessionID: "s1", Text: "t"})
	mgr.upsertUndelivered(deliverySK, delegator.PendingInput{ID: "spent", WorkDir: "/w", SessionID: "s1", Redeliveries: delegator.MaxRedeliveries})
	mgr.upsertUndelivered("other-agent/c1", delegator.PendingInput{ID: "not-mine"})
	var checked []string
	mgr.Spec.TranscriptChecker = func(workDir, sessionID, id string) (bool, error) {
		checked = append(checked, workDir+"|"+sessionID+"|"+id)
		return id == "folded", nil
	}

	mgr.RestoreUndelivered()

	if len(lg.consumed) != 1 || lg.consumed[0][0] != ref {
		t.Errorf("InputConsumed = %v, want the folded input's ref", lg.consumed)
	}
	if len(lg.redeliv) != 1 || lg.redeliv[0].Input.ID != "lost" || lg.redeliv[0].Reason != "restart" || lg.redeliv[0].Input.Redeliveries != 1 {
		t.Fatalf("redeliveries = %+v, want lost (restart, attempt 1)", lg.redeliv)
	}
	if mgr.stillUndelivered(deliverySK, "folded") || mgr.stillUndelivered(deliverySK, "spent") {
		t.Error("folded / capped inputs still persisted")
	}
	if !mgr.stillUndelivered(deliverySK, "lost") {
		t.Error("redelivered input no longer persisted before its re-write")
	}
	if !mgr.stillUndelivered("other-agent/c1", "not-mine") {
		t.Error("restore touched another agent's session")
	}
	if len(checked) == 0 || checked[0] != "/w|s1|folded" {
		t.Errorf("transcript checks = %v, want the input's own workdir and session", checked)
	}
}
