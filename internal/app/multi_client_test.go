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

// TestConversationOpenSet_AttachesLateLearnedConvForFanOut proves the #1474 fix: a
// socket that learns a conversation AFTER its hello (minted on another device and
// mirrored via conversation.openSync, then rendered from durable HTTP backfill)
// joins the binding's live client set when it reports the conversation in its
// open-set — so a subsequent server frame fans out to it LIVE instead of waiting
// for its next reconnect/resume. The seeded ack (history came over HTTP) keeps the
// fresh reader from pinning the replay-buffer trim floor at 0.
func TestConversationOpenSet_AttachesLateLearnedConvForFanOut(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()
	const agentID = "arnix"
	h.setupAgent(platform.AgentConnectionParams{AgentID: agentID})

	// A conversation minted on another device, mirrored to this user: the durable
	// binding exists, but THIS socket never attached (its hello predated it).
	b := h.ensureBinding(nil, agentID, "conv-late")
	for i := 0; i < 3; i++ {
		b.send(fap.Notification{ConversationID: "conv-late", MessageID: "m", Text: "n"})
	}
	seqAtLearn := b.currentSeq()

	late := fakeClient()
	h.addClient(late)
	if b.isAttached(late) {
		t.Fatal("precondition: socket must NOT be attached before it reports the open-set")
	}

	h.handleConversationOpenSet(late, fap.ConversationOpenSet{ConversationIDs: []string{"conv-late"}})

	if !b.isAttached(late) {
		t.Fatal("socket must attach to a late-learned conversation on ConversationOpenSet")
	}
	// Ack seeded to the high-water at attach: HTTP backfill covered history, so the
	// socket needs only future frames and must not pin the trim floor at 0.
	b.mu.Lock()
	st := b.clientStates[late]
	b.mu.Unlock()
	if st == nil || st.ackHW != seqAtLearn {
		t.Fatalf("late socket ackHW = %v, want seeded to high-water %d", st, seqAtLearn)
	}

	// The load-bearing assertion: a subsequent frame fans out to the late socket live.
	drain(t, late)
	b.send(fap.ServerMessage{ConversationID: "conv-late", MessageID: "m1", Role: "agent", Text: "live"})
	if got := types(drain(t, late)); len(got) == 0 || got[0] != "message" {
		t.Fatalf("late socket got %v after open-set attach, want a live [message] fan-out", got)
	}
}

// TestConversationOpenSet_IdempotentForAttachedReader proves the attach-on-open-set
// is idempotent: a socket already attached (an established reader that has acked
// frames) keeps its ack — the open-set must not reset it to the high-water and
// re-hold the trim floor an already-caught-up reader had released.
func TestConversationOpenSet_IdempotentForAttachedReader(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()
	const agentID = "arnix"
	h.setupAgent(platform.AgentConnectionParams{AgentID: agentID})
	b := h.ensureBinding(nil, agentID, "conv-1")

	reader := fakeClient()
	h.addClient(reader)
	b.attach(reader)
	for i := 0; i < 5; i++ {
		b.send(fap.Notification{ConversationID: "conv-1", MessageID: "m", Text: "n"})
	}
	drain(t, reader)
	b.ackInbound(reader, 2) // reader has confirmed up to seq 2

	h.handleConversationOpenSet(reader, fap.ConversationOpenSet{ConversationIDs: []string{"conv-1"}})

	b.mu.Lock()
	st := b.clientStates[reader]
	b.mu.Unlock()
	if st == nil || st.ackHW != 2 {
		t.Fatalf("attached reader ackHW = %v, want 2 preserved (open-set must not reset it)", st)
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

// TestRemoveClient_PromptSurvivesWhileAnotherClientLive: a registered prompt must
// not be purged when ONE of several attached devices disconnects — the surviving
// device's Allow/Deny taps would otherwise hit an unknown prompt id and hang the
// backend permission request (#1). It is purged only when the last device leaves.
func TestRemoveClient_PromptSurvivesWhileAnotherClientLive(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()
	const agentID = "arnix"
	h.setupAgent(platform.AgentConnectionParams{AgentID: agentID})
	b := h.ensureBinding(nil, agentID, "conv-1")

	phone := fakeClient()
	tablet := fakeClient()
	h.addClient(phone)
	h.addClient(tablet)
	b.attach(phone)
	b.attach(tablet)
	h.registerPrompt("p1", b)

	h.removeClient(phone)
	if h.bindingForPrompt("p1") == nil {
		t.Fatal("prompt purged while tablet still attached — surviving device's buttons would break")
	}
	h.removeClient(tablet)
	if h.bindingForPrompt("p1") != nil {
		t.Error("prompt not purged after the last device disconnected")
	}
}

// TestSend_PerClientAck: each attached device is acked for ITS OWN inbound
// high-water, never the max across devices (#2) — a lagging device must not be told
// we received seqs it never sent. The buffered canonical copy stays ack=0 and keeps
// the same envelope id, so client-side dedup still matches across the copies.
func TestSend_PerClientAck(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()
	const agentID = "arnix"
	h.setupAgent(platform.AgentConnectionParams{AgentID: agentID})
	b := h.ensureBinding(nil, agentID, "conv-1")

	phone := fakeClient()
	tablet := fakeClient()
	b.attach(phone)
	b.attach(tablet)
	b.acceptInbound(phone, "p9", 9)
	b.acceptInbound(tablet, "t3", 3)

	b.send(fap.Activity{ConversationID: "conv-1", Kind: "typing"})

	pe := drainEnv(t, phone)
	te := drainEnv(t, tablet)
	if len(pe) != 1 || pe[0].ack != 9 {
		t.Fatalf("phone ack = %v, want 9 (its own inbound high-water)", pe)
	}
	if len(te) != 1 || te[0].ack != 3 {
		t.Fatalf("tablet ack = %v, want 3 (its own inbound high-water)", te)
	}
	if pe[0].id == "" || pe[0].id != te[0].id {
		t.Errorf("stamped copies must share envelope id, got phone=%q tablet=%q", pe[0].id, te[0].id)
	}
	b.mu.Lock()
	bufWire := b.buffer[len(b.buffer)-1].wire
	b.mu.Unlock()
	in, err := fap.Decode(bufWire)
	if err != nil {
		t.Fatalf("decode buffered wire: %v", err)
	}
	if in.ID != pe[0].id {
		t.Errorf("buffered id %q != sent id %q — dedup would break", in.ID, pe[0].id)
	}
	if in.Ack != 0 {
		t.Errorf("buffered canonical copy ack = %d, want 0", in.Ack)
	}
}

// TestFeatures_UnionAcrossClients: capability gating is the UNION across attached
// devices, order-independent, so an incapable device can't switch off a feature the
// capable one supports (#3). When the capable device detaches with another still
// live, the union recomputes (feature drops); when the LAST device leaves, the last
// union is retained for offline gating.
func TestFeatures_UnionAcrossClients(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()
	const agentID = "arnix"
	h.setupAgent(platform.AgentConnectionParams{AgentID: agentID})
	newCapable := func() *wsClient {
		c := fakeClient()
		c.features = map[string]struct{}{featureWizard: {}}
		return c
	}

	// Incapable attaches first, capable second: union still has the feature.
	b1 := h.ensureBinding(nil, agentID, "conv-1")
	b1.attach(fakeClient())
	cap1 := newCapable()
	b1.attach(cap1)
	if !b1.supportsFeature(featureWizard) {
		t.Error("incapable-first: union must include the capable client's feature")
	}
	// Capable detaches, incapable remains: union recomputes, feature drops.
	b1.detachIf(cap1)
	if b1.supportsFeature(featureWizard) {
		t.Error("after capable detached (incapable still live), feature must drop from the union")
	}

	// Reverse order: capable first, incapable second — still true.
	b2 := h.ensureBinding(nil, agentID, "conv-2")
	b2.attach(newCapable())
	b2.attach(fakeClient())
	if !b2.supportsFeature(featureWizard) {
		t.Error("capable-first: incapable second must not narrow the union")
	}

	// Sole capable client detaches (goes offline): last union retained.
	b3 := h.ensureBinding(nil, agentID, "conv-3")
	cap3 := newCapable()
	b3.attach(cap3)
	b3.detachIf(cap3)
	if !b3.supportsFeature(featureWizard) {
		t.Error("last client detached → last union must be retained for offline gating")
	}
}

// TestAckInbound_IgnoresUnattachedSocket: an ack from a socket not attached to the
// binding must be ignored, not recorded — a ghost entry would pin the trim floor
// forever (#4).
func TestAckInbound_IgnoresUnattachedSocket(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()
	const agentID = "arnix"
	h.setupAgent(platform.AgentConnectionParams{AgentID: agentID})
	b := h.ensureBinding(nil, agentID, "conv-1")

	b.attach(fakeClient())
	ghost := fakeClient() // never attached
	b.ackInbound(ghost, 5)

	b.mu.Lock()
	_, exists := b.clientStates[ghost]
	b.mu.Unlock()
	if exists {
		t.Error("ack from unattached socket created a clientState — would pin the trim floor")
	}
}

// TestResume_SeedsReaderAckSoTrimProceeds: a pure-reader device attached via resume
// is ack-seeded from its resume point, so once the other clients catch up the buffer
// trims — without the seed the reader's ackHW=0 would block trimming forever (#4).
func TestResume_SeedsReaderAckSoTrimProceeds(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()
	const agentID = "arnix"
	h.setupAgent(platform.AgentConnectionParams{AgentID: agentID})
	b := h.ensureBinding(nil, agentID, "conv-1")

	sender := fakeClient()
	b.attach(sender)
	for i := 0; i < 5; i++ {
		b.send(fap.Notification{ConversationID: "conv-1", MessageID: "m", Text: "n"})
	}
	drain(t, sender)

	reader := fakeClient()
	reader.hub = h
	h.resumeConversations(reader, []fap.ResumePoint{{ConversationID: "conv-1", Ack: 5}})
	drain(t, reader)

	b.ackInbound(sender, 5)
	b.mu.Lock()
	n := len(b.buffer)
	b.mu.Unlock()
	if n != 0 {
		t.Errorf("buffer = %d after both clients at ack 5, want 0 (reader was seeded, not blocking)", n)
	}
}
