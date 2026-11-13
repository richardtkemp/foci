package app

import (
	"testing"
	"time"
)

// A store backlog longer than one Range page must not leave a hole (#3075,
// #2968 F14). replayTo reads ONE store page (maxResumeStoreReplay frames) and
// then the in-memory buffer. When that page stops short of the buffer's floor,
// sending the buffer anyway skips the frames between, and the client acks past
// the hole and never asks for them. Replay must report success only after a
// contiguous emitted prefix that reaches the end; anything short of that is a
// truncation the client finishes via GET /app/replay.

// bigClient has room for every frame these tests produce, so the socket queue
// is never the bound: only the store page can stop the replay.
func bigClient() *wsClient {
	return &wsClient{
		send:     make(chan []byte, 3*maxResumeStoreReplay),
		done:     make(chan struct{}),
		convByID: make(map[string]*convBinding),
	}
}

func assertContiguousFromOne(t *testing.T, got []envFrame) {
	t.Helper()
	for i, f := range got {
		if f.seq != int64(i+1) {
			t.Fatalf("frame %d has seq %d, want %d: the replay skipped frames (%d emitted)", i, f.seq, i+1, len(got))
		}
	}
}

func TestReplayTo_StorePageShortOfMemFloorIsTruncationNotAGap(t *testing.T) {
	s := tempFrameStore(t)
	now := time.Now().UnixMilli()
	const gap = 5
	memFloor := int64(maxResumeStoreReplay + gap + 1) // store page ends gap frames below it
	last := memFloor + 2
	for i := int64(1); i <= last; i++ {
		s.insert(frameWrite{convID: "c1", seq: i, wire: mkWire(t, "c1", i), sentMs: now, visible: true})
	}
	b := &convBinding{convID: "c1", store: s, seq: last, seen: map[string]struct{}{}}
	for i := memFloor; i <= last; i++ {
		b.buffer = append(b.buffer, bufferedFrame{seq: i, wire: mkWire(t, "c1", i), sent: time.Now()})
	}

	c := bigClient()
	ok := b.replayTo(c, 0)
	got := drainEnv(t, c)
	assertContiguousFromOne(t, got)
	if ok && got[len(got)-1].seq != last {
		t.Errorf("replayTo reported success but stopped at seq %d of %d", got[len(got)-1].seq, last)
	}
	if ok {
		t.Error("replayTo reported success though one store page could not reach the in-memory floor")
	}
}

func TestReplayTo_StoreOnlyBacklogOverOnePageIsTruncation(t *testing.T) {
	s := tempFrameStore(t)
	now := time.Now().UnixMilli()
	last := int64(maxResumeStoreReplay + 5)
	for i := int64(1); i <= last; i++ {
		s.insert(frameWrite{convID: "c1", seq: i, wire: mkWire(t, "c1", i), sentMs: now, visible: true})
	}
	b := &convBinding{convID: "c1", store: s, seq: last, seen: map[string]struct{}{}} // no in-memory buffer (after a restart)

	c := bigClient()
	ok := b.replayTo(c, 0)
	got := drainEnv(t, c)
	assertContiguousFromOne(t, got)
	if ok {
		t.Errorf("replayTo reported success after %d of %d frames", len(got), last)
	}
}

// The boundary: a store backlog of EXACTLY one page, with nothing after it,
// is complete, and must not be reported as truncated.
func TestReplayTo_StoreOnlyBacklogOfExactlyOnePageIsComplete(t *testing.T) {
	s := tempFrameStore(t)
	now := time.Now().UnixMilli()
	last := int64(maxResumeStoreReplay)
	for i := int64(1); i <= last; i++ {
		s.insert(frameWrite{convID: "c1", seq: i, wire: mkWire(t, "c1", i), sentMs: now, visible: true})
	}
	b := &convBinding{convID: "c1", store: s, seq: last, seen: map[string]struct{}{}}

	c := bigClient()
	ok := b.replayTo(c, 0)
	got := drainEnv(t, c)
	assertContiguousFromOne(t, got)
	if len(got) != int(last) {
		t.Fatalf("emitted %d frames, want all %d", len(got), last)
	}
	if !ok {
		t.Error("replayTo reported truncation for a backlog that fit in one page")
	}
}
