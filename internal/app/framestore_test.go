package app

import (
	"path/filepath"
	"testing"
	"time"

	"foci/internal/app/fap"
)

func tempFrameStore(t *testing.T) *frameStore {
	t.Helper()
	s, err := newFrameStore(filepath.Join(t.TempDir(), "frames.db"), 30*24*time.Hour)
	if err != nil {
		t.Fatalf("newFrameStore: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// mkWire encodes a real frame so drainEnv can decode the seq back out.
func mkWire(t *testing.T, convID string, seq int64) string {
	t.Helper()
	w, err := fap.Encode(fap.Typing{ConversationID: convID, On: true}, seq, 0, "", "")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return w
}

// insert synchronously (bypassing the async writer) for deterministic content tests.
func seed(s *frameStore, convID string, seq int64, sentMs int64) {
	s.insert(frameWrite{convID: convID, seq: seq, wire: "wire-" + convID, sentMs: sentMs, visible: true})
}

func TestFrameStore_InsertMaxSeqRange(t *testing.T) {
	s := tempFrameStore(t)
	now := time.Now().UnixMilli()
	for i := int64(1); i <= 5; i++ {
		seed(s, "c1", i, now)
	}
	seed(s, "other", 99, now) // different conversation must not bleed in

	if got := s.MaxSeq("c1"); got != 5 {
		t.Errorf("MaxSeq(c1) = %d, want 5", got)
	}
	if got := s.MaxSeq("ghost"); got != 0 {
		t.Errorf("MaxSeq(ghost) = %d, want 0", got)
	}
	got := s.Range("c1", 2, 100)
	if len(got) != 3 || got[0].seq != 3 || got[2].seq != 5 {
		t.Fatalf("Range(c1, from=2) = %+v, want seq 3,4,5", got)
	}
}

func TestFrameStore_TrimOlderThan(t *testing.T) {
	s := tempFrameStore(t)
	old := time.Now().Add(-48 * time.Hour).UnixMilli()
	fresh := time.Now().UnixMilli()
	seed(s, "c1", 1, old)
	seed(s, "c1", 2, old)
	seed(s, "c1", 3, fresh)

	cutoff := time.Now().Add(-24 * time.Hour).UnixMilli()
	if n := s.TrimOlderThan(cutoff); n != 2 {
		t.Errorf("trimmed %d, want 2", n)
	}
	got := s.Range("c1", 0, 100)
	if len(got) != 1 || got[0].seq != 3 {
		t.Fatalf("after trim Range = %+v, want only seq 3", got)
	}
}

// Append is async; Close must drain it so a graceful restart loses nothing.
// Reopening the same path simulates the post-restart process reading durable state.
func TestFrameStore_AppendDrainsOnClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frames.db")
	s, err := newFrameStore(path, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("newFrameStore: %v", err)
	}
	for i := int64(1); i <= 3; i++ {
		s.Append("c1", i, "w", time.Now().UnixMilli(), true)
	}
	s.Close() // drains the queue + closes

	s2, err := newFrameStore(path, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(s2.Close)
	if got := s2.MaxSeq("c1"); got != 3 {
		t.Errorf("after drain+reopen MaxSeq = %d, want 3 (frames lost across restart)", got)
	}
}

// Seq rehydration: a binding (re)created after a restart seeds its seq from the
// store's high-water, so the per-conversation counter does not reset to 0.
func TestBinding_SeqRehydratesFromStore(t *testing.T) {
	s := tempFrameStore(t)
	now := time.Now().UnixMilli()
	for i := int64(1); i <= 5; i++ {
		seed(s, "c1", i, now)
	}
	// Mimic the hub's binding-creation seeding (hub.go ensureBinding).
	b := &convBinding{convID: "c1", store: s, seq: s.MaxSeq("c1"), seen: map[string]struct{}{}}
	c := fakeClient()
	b.attach(c)
	b.send(fap.Typing{ConversationID: "c1", On: true}) // must be seq 6, not 1
	ds := drainEnv(t, c)
	if len(ds) != 1 || ds[0].seq != 6 {
		t.Fatalf("post-restart send = %v, want one frame at seq 6", ds)
	}
}

// replayTo must backfill the gap below the in-memory buffer's floor from the
// durable store, then the in-memory frames — in order, no dupes, no gaps.
func TestReplayTo_BackfillsFromStoreBelowMemFloor(t *testing.T) {
	s := tempFrameStore(t)
	now := time.Now().UnixMilli()
	// Store holds the full history seq 1..5 (real wires so drainEnv reads seq).
	for i := int64(1); i <= 5; i++ {
		s.insert(frameWrite{convID: "c1", seq: i, wire: mkWire(t, "c1", i), sentMs: now, visible: true})
	}
	// In-memory buffer retains only the tail (seq 4,5) — the rest was trimmed.
	b := &convBinding{convID: "c1", store: s, seq: 5, seen: map[string]struct{}{}}
	b.buffer = []bufferedFrame{
		{seq: 4, wire: mkWire(t, "c1", 4), sent: time.Now()},
		{seq: 5, wire: mkWire(t, "c1", 5), sent: time.Now()},
	}

	c := fakeClient()
	b.replayTo(c, 0) // client has rendered nothing → wants 1..5
	ds := drainEnv(t, c)

	if len(ds) != 5 {
		t.Fatalf("replay emitted %d frames, want 5 (%+v)", len(ds), ds)
	}
	for i, f := range ds {
		if f.seq != int64(i+1) {
			t.Fatalf("frame %d has seq %d, want %d — gap/dupe/misorder (%+v)", i, f.seq, i+1, ds)
		}
	}
}

// When the client's cursor is already at/above the in-memory floor, replayTo must
// NOT touch the store (no redundant backfill) and emit only the in-memory tail.
func TestReplayTo_NoStoreBackfillWhenMemoryCovers(t *testing.T) {
	s := tempFrameStore(t)
	now := time.Now().UnixMilli()
	for i := int64(1); i <= 5; i++ {
		s.insert(frameWrite{convID: "c1", seq: i, wire: mkWire(t, "c1", i), sentMs: now, visible: true})
	}
	b := &convBinding{convID: "c1", store: s, seq: 5, seen: map[string]struct{}{}}
	b.buffer = []bufferedFrame{
		{seq: 4, wire: mkWire(t, "c1", 4), sent: time.Now()},
		{seq: 5, wire: mkWire(t, "c1", 5), sent: time.Now()},
	}
	c := fakeClient()
	b.replayTo(c, 4) // already rendered through 4 → wants only 5
	ds := drainEnv(t, c)
	if len(ds) != 1 || ds[0].seq != 5 {
		t.Fatalf("replay = %v, want only seq 5 from memory", ds)
	}
}
