package app

import (
	"testing"
	"time"

	"foci/internal/app/fap"
)

// The hello's resume list is BOUNDED (#1737) — the app sends its open tabs plus the
// most recent, not one entry per conversation, because ~65 bytes x 163 conversations
// is an ~11KB frame that a 1492-MTU path with broken PMTUD silently black-holes.
//
// That makes the resume list the wrong thing to key live delivery on. A conversation
// the client did not name must still get its NEXT message pushed, or it stops badging
// as unread until the device happens to reconnect — the #1834 failure shape, where
// the roster preview and the FCM notification both update and only the chat itself
// stays empty (two replies invisible for 90 minutes, observed 2026-09-02).
//
// So attachment is now independent of the resume points: replay uses them (it needs
// the client's ack), fan-out does not.
func TestResumeConversations_AttachesConversationsNotInTheHello(t *testing.T) {
	h := newTestHub()

	mkBinding := func(convID string, seqs ...int64) *convBinding {
		b := &convBinding{
			convID:     convID,
			sessionKey: "ag/" + convID,
			agentID:    "ag",
			seen:       map[string]struct{}{},
			clients:    map[*wsClient]struct{}{},
		}
		for _, sq := range seqs {
			b.buffer = append(b.buffer, bufferedFrame{seq: sq, wire: mkWire(t, convID, sq), sent: time.Now()})
			b.seq = sq
		}
		h.convs[convID] = b
		return b
	}

	named := mkBinding("c-named", 1, 2)
	unnamed := mkBinding("c-unnamed", 11, 12)

	c := fakeClientFor(h)
	h.clients[c] = struct{}{}
	// A capped hello: only one of the two conversations is named.
	h.resumeConversations(c, []fap.ResumePoint{{ConversationID: "c-named", Ack: 0}})

	// The omitted conversation must NOT be replayed — that is what the cap buys, and
	// the client reconciles that backlog over GET /app/replay off the roster's lastSeq.
	for _, f := range drainEnv(t, c) {
		if f.seq >= 11 {
			t.Errorf("unnamed conversation replayed seq %d — a conversation absent from the hello must not push its backlog", f.seq)
		}
	}

	unnamed.mu.Lock()
	_, attached := unnamed.clients[c]
	ackHW := int64(-1)
	if st := unnamed.clientStates[c]; st != nil {
		ackHW = st.ackHW
	}
	unnamed.mu.Unlock()
	if !attached {
		t.Fatal("a conversation absent from the hello did not attach — its next message would never reach the device, so it would never badge as unread")
	}
	// Seeded at the high-water, not 0: this socket is not owed the backlog (it pulls it
	// over HTTP), and a floor of 0 would pin the replay-buffer trim for as long as the
	// device stays connected.
	if ackHW != 12 {
		t.Errorf("unresumed attach seeded ackHW = %d, want 12 (the current high-water) — a 0 floor pins the replay buffer forever", ackHW)
	}

	// The point of attaching: the next live frame lands.
	unnamed.send(fap.ServerMessage{ConversationID: "c-unnamed", MessageID: "m1", Role: "agent", Text: "arrived after a capped hello"})
	ds := drain(t, c)
	if len(ds) != 1 || ds[0].t != fap.TypeMessage || ds[0].d["text"] != "arrived after a capped hello" {
		t.Fatalf("live frame on an unnamed conversation did not reach the client, got %v", types(ds))
	}

	// Idempotent: a named conversation's ack must survive the attach pass rather than
	// being reset to the high-water by it.
	named.mu.Lock()
	namedAck := int64(-1)
	if st := named.clientStates[c]; st != nil {
		namedAck = st.ackHW
	}
	named.mu.Unlock()
	if namedAck != 0 {
		t.Errorf("named conversation's seeded ack = %d, want 0 from its resume point — the attach pass overwrote it", namedAck)
	}
}
