package app

import (
	"testing"

	"foci/internal/app/fap"
	"foci/internal/platform"
)

// snapshotClients returns all currently-attached live sockets on the binding.
// Test-only helper that complements snapshotClient (singular) for cases that
// need to assert on the full multi-device set.
func (b *convBinding) snapshotClients() []*wsClient {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*wsClient, 0, len(b.clients))
	for c := range b.clients {
		out = append(out, c)
	}
	return out
}

// TestAttach_SecondDeviceDoesNotEvictFirst verifies the multi-client model: a
// second device attaching to the same conversation coexists with the first.
// Both stay live; both receive subsequent sends.
//
// Before the multi-client refactor, attach overwrote b.client and silently
// orphaned the first socket — it stayed in h.clients and kept referencing
// the convID in its convByID map, but received nothing further on that
// conversation until its own ping-timeout minutes later.
func TestAttach_SecondDeviceDoesNotEvictFirst(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()

	const agentID = "arnix"
	h.setupAgent(platform.AgentConnectionParams{AgentID: agentID})
	b := h.ensureBinding(nil, agentID, "conv-1")

	first := fakeClient()
	second := fakeClient()
	b.attach(first)

	// Sanity: first is bound and live.
	if got := b.snapshotClient(); got != first {
		t.Fatalf("after first attach, snapshotClient = %p, want first (%p)", got, first)
	}
	select {
	case <-first.done:
		t.Fatal("first socket closed before second attach")
	default:
	}

	// Second device opens the same conversation.
	b.attach(second)

	// First socket is STILL alive (the bug: it used to be deaf-but-alive; the
	// previous eviction fix closed it; the multi-client refactor keeps it).
	select {
	case <-first.done:
		t.Fatal("first socket was closed by second attach — multi-client must coexist")
	default:
	}

	// Both sockets should be members of the binding's client set.
	clients := b.snapshotClients()
	if len(clients) != 2 {
		t.Fatalf("after second attach, binding has %d clients, want 2", len(clients))
	}
}

// TestSend_FansOutToAllAttachedClients verifies that an outbound frame reaches
// every attached device, not just the most-recently-attached one. This is the
// load-bearing guarantee of the multi-client model.
func TestSend_FansOutToAllAttachedClients(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()
	const agentID = "arnix"
	h.setupAgent(platform.AgentConnectionParams{AgentID: agentID})
	b := h.ensureBinding(nil, agentID, "conv-1")

	phone := fakeClient()
	tablet := fakeClient()
	b.attach(phone)
	b.attach(tablet)
	drain(t, phone) // discard any setup frames
	drain(t, tablet)

	b.send(fap.ServerMessage{ConversationID: "conv-1", MessageID: "m1", Role: "agent", Text: "hi"})

	gotPhone := types(drain(t, phone))
	gotTablet := types(drain(t, tablet))
	if len(gotPhone) == 0 || gotPhone[0] != "message" {
		t.Errorf("phone got %v, want [message]", gotPhone)
	}
	if len(gotTablet) == 0 || gotTablet[0] != "message" {
		t.Errorf("tablet got %v, want [message]", gotTablet)
	}
}

// TestDetachIf_OneClientLeavesOthersLive verifies that when one device
// disconnects, the others keep receiving. The binding stays live as long as
// any client is attached.
func TestDetachIf_OneClientLeavesOthersLive(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()
	const agentID = "arnix"
	h.setupAgent(platform.AgentConnectionParams{AgentID: agentID})
	b := h.ensureBinding(nil, agentID, "conv-1")

	phone := fakeClient()
	tablet := fakeClient()
	b.attach(phone)
	b.attach(tablet)
	drain(t, phone)
	drain(t, tablet)

	b.detachIf(phone)
	if len(b.snapshotClients()) != 1 {
		t.Fatalf("after detaching phone, %d clients, want 1 (tablet)", len(b.snapshotClients()))
	}

	// Tablet should still receive sends.
	b.send(fap.ServerMessage{ConversationID: "conv-1", MessageID: "m2", Role: "agent", Text: "still here"})
	gotTablet := types(drain(t, tablet))
	if len(gotTablet) == 0 || gotTablet[0] != "message" {
		t.Errorf("tablet got %v after phone detached, want [message]", gotTablet)
	}
}

// TestAckInbound_PerClientTrim verifies the trimming safety property: a
// second client with a low ack holds the buffer (its replay history can't be
// discarded just because the first client has acked past it).
func TestAckInbound_PerClientTrim(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()
	const agentID = "arnix"
	h.setupAgent(platform.AgentConnectionParams{AgentID: agentID})
	b := h.ensureBinding(nil, agentID, "conv-1")

	phone := fakeClient()
	tablet := fakeClient()
	b.attach(phone)
	b.attach(tablet)

	// Send 5 frames, drain so the channel doesn't back up.
	for i := 0; i < 5; i++ {
		b.send(fap.Notification{ConversationID: "conv-1", MessageID: "m", Text: "n"})
	}
	drain(t, phone)
	drain(t, tablet)

	// Phone acks seq 5 (everything). Buffer should NOT trim — tablet hasn't acked.
	b.ackInbound(phone, 5)
	b.mu.Lock()
	afterPhoneAck := len(b.buffer)
	b.mu.Unlock()
	if afterPhoneAck != 5 {
		t.Errorf("after phone-only ack, buffer = %d, want 5 (tablet unacked holds it)", afterPhoneAck)
	}

	// Tablet acks seq 3. NOW we can trim up to min(5,3) = 3.
	b.ackInbound(tablet, 3)
	b.mu.Lock()
	afterTabletAck := len(b.buffer)
	firstSeq := int64(0)
	if afterTabletAck > 0 {
		firstSeq = b.buffer[0].seq
	}
	b.mu.Unlock()
	if afterTabletAck != 2 || firstSeq != 4 {
		t.Errorf("after tablet ack=3, buffer = %d starting at seq %d, want 2 starting at seq 4", afterTabletAck, firstSeq)
	}
}

// TestAttach_NilClearsLiveSet verifies the socketless-restore path: attach(nil)
// drops all live sockets without otherwise touching durable state. Used at
// startup to ensure a stale set never carries over from a previous process.
func TestAttach_NilClearsLiveSet(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()
	const agentID = "arnix"
	h.setupAgent(platform.AgentConnectionParams{AgentID: agentID})
	b := h.ensureBinding(nil, agentID, "conv-1")

	c := fakeClient()
	b.attach(c)
	if len(b.snapshotClients()) != 1 {
		t.Fatal("expected 1 client after attach")
	}
	b.attach(nil)
	if got := b.snapshotClient(); got != nil {
		t.Errorf("after attach(nil), snapshotClient = %p, want nil", got)
	}
	if len(b.snapshotClients()) != 0 {
		t.Errorf("after attach(nil), client set non-empty")
	}
}

// TestAttach_SameSocketReattachIsHarmless verifies re-attaching the same
// socket (which can happen in resume paths) doesn't duplicate it in the set.
func TestAttach_SameSocketReattachIsHarmless(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()
	const agentID = "arnix"
	h.setupAgent(platform.AgentConnectionParams{AgentID: agentID})
	b := h.ensureBinding(nil, agentID, "conv-1")

	c := fakeClient()
	b.attach(c)
	b.attach(c) // idempotent

	if got := len(b.snapshotClients()); got != 1 {
		t.Fatalf("after re-attaching same socket, %d clients, want 1", got)
	}
}
