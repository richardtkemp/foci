package app

import (
	"testing"
	"time"
)

// A resume replay must never kill a healthy socket. Before #1779, replayTo used
// the same enqueue as live frames: a full queue blocked 2s and closed the socket
// "to force resume", whereupon the client reconnected and replayed the identical
// backlog. One device did that 909 times.
//
// The replay push is now bounded by the socket queue itself. What matters is that
// the client is left with a CONTIGUOUS PREFIX and not a hole: a short prefix is
// what GET /app/replay is designed to finish, whereas a gap in the middle is the
// exact thing enqueue's no-drop rule exists to prevent — the client acks past it
// and never asks again.
func TestReplayTo_TruncatesCleanlyInsteadOfClosingTheSocket(t *testing.T) {
	const queue = 3
	c := &wsClient{
		send:     make(chan []byte, queue),
		done:     make(chan struct{}),
		convByID: make(map[string]*convBinding),
	}

	b := &convBinding{convID: "c1", seen: map[string]struct{}{}}
	for sq := int64(1); sq <= 10; sq++ {
		b.buffer = append(b.buffer, bufferedFrame{seq: sq, wire: mkWire(t, "c1", sq), sent: time.Now()})
		b.seq = sq
	}

	done := make(chan bool, 1)
	go func() { done <- b.replayTo(c, 0) }()

	select {
	case ok := <-done:
		if ok {
			t.Error("replayTo reported success after overflowing a 3-slot queue with 10 frames")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replayTo blocked on a full queue — the bulk path must not block, that is the reconnect loop")
	}

	select {
	case <-c.done:
		t.Error("replayTo closed the socket — a slow drain is back-pressure, not a dead client")
	default:
	}

	got := drainEnv(t, c)
	if len(got) != queue {
		t.Fatalf("delivered %d frames into a %d-slot queue, want exactly %d", len(got), queue, queue)
	}
	for i, f := range got {
		if f.seq != int64(i+1) {
			t.Fatalf("frame %d has seq %d, want %d — the prefix is not contiguous, so the client will ack past a hole (%+v)", i, f.seq, i+1, got)
		}
	}
}

// A client that drains keeps receiving: the bound is the queue, not a frame cap,
// so it must not latch a conversation into permanent truncation.
func TestReplayTo_FullDeliveryWhenTheClientDrains(t *testing.T) {
	c := fakeClient() // 64 slots, comfortably more than the backlog
	b := &convBinding{convID: "c1", seen: map[string]struct{}{}}
	for sq := int64(1); sq <= 10; sq++ {
		b.buffer = append(b.buffer, bufferedFrame{seq: sq, wire: mkWire(t, "c1", sq), sent: time.Now()})
		b.seq = sq
	}
	if ok := b.replayTo(c, 0); !ok {
		t.Error("replayTo reported truncation with 10 frames and 64 slots free")
	}
	if got := len(drainEnv(t, c)); got != 10 {
		t.Errorf("delivered %d frames, want all 10 — the bound must be the queue, not a cap", got)
	}
}
