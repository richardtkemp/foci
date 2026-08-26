package app

import (
	"testing"
	"time"

	"foci/internal/app/fap"
	"foci/internal/platform"
)

// A hello names every conversation the device has ever seen, archived ones
// included. Replaying an archived conversation produces frames the app cannot
// display — it is hidden from the roster — and it is not free: each replayTo
// pushes up to maxResumeStoreReplay frames into a sendBuffer-slot queue, so a
// long-lived device overflows that queue by construction and the socket is then
// closed "to force resume", replaying the identical backlog forever (#1779).
//
// Measured 2026-08-26: one device sent 169 resume points against 11 live
// conversations and reconnected 909 times, pinned at the same resume mark.
func TestResumeConversations_SkipsArchivedButStaysAttached(t *testing.T) {
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: newTestIndex(t)}

	mkBinding := func(convID string, chatID int64, seqs ...int64) *convBinding {
		b := &convBinding{
			convID:     convID,
			sessionKey: "ag/" + convID,
			agentID:    "ag",
			chatID:     chatID,
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

	// Distinct seq ranges so a drained frame names its origin without extra plumbing.
	arch := mkBinding("c1", 42, 1, 2)
	live := mkBinding("c2", 43, 11, 12)

	if err := h.deps.SessionIndex.SetArchivedChat("ag", "app", 42, true); err != nil {
		t.Fatalf("SetArchivedChat: %v", err)
	}

	c := fakeClientFor(h)
	h.clients[c] = struct{}{}
	points := []fap.ResumePoint{
		{ConversationID: "c1", Ack: 0},
		{ConversationID: "c2", Ack: 0},
	}
	h.resumeConversations(c, points)

	for _, f := range drainEnv(t, c) {
		if f.seq < 11 {
			t.Errorf("archived conversation replayed seq %d — an archived backlog must not be pushed", f.seq)
		}
	}

	// Attach is NOT skipped: a live frame arriving on an archived conversation
	// must still reach the device. Only the historical replay is withheld.
	arch.mu.Lock()
	_, attached := arch.clients[c]
	arch.mu.Unlock()
	if !attached {
		t.Error("archived conversation did not attach the client — live delivery would be lost, which is a different bug from the one being fixed")
	}

	// The live conversation is unaffected.
	if got := len(drainEnv(t, c)); got != 0 {
		t.Errorf("expected the queue drained, got %d leftover frames", got)
	}
	_ = live

	// Unarchiving restores replay — the skip is a filter, not a latch.
	if err := h.deps.SessionIndex.SetArchivedChat("ag", "app", 42, false); err != nil {
		t.Fatalf("SetArchivedChat: %v", err)
	}
	h.resumeConversations(c, points)
	sawArchived := false
	for _, f := range drainEnv(t, c) {
		if f.seq < 11 {
			sawArchived = true
		}
	}
	if !sawArchived {
		t.Error("unarchived conversation did not replay — the skip latched instead of filtering")
	}
}
