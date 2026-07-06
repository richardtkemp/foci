package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	w, err := fap.Encode(fap.Activity{ConversationID: convID, Kind: "typing"}, seq, 0, "", "")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return w
}

// insert synchronously (bypassing the async writer) for deterministic content tests.
func seed(s *frameStore, convID string, seq int64, sentMs int64) {
	s.insert(frameWrite{convID: convID, seq: seq, wire: "wire-" + convID, sentMs: sentMs, visible: true})
}

func TestFrameStore_LastVisible(t *testing.T) {
	s := tempFrameStore(t)
	s.insert(frameWrite{convID: "c1", seq: 1, wire: "w1", sentMs: 100, visible: true, preview: "first"})
	s.insert(frameWrite{convID: "c1", seq: 2, wire: "w2", sentMs: 200, visible: true, preview: "second"})
	s.insert(frameWrite{convID: "c1", seq: 3, wire: "w3", sentMs: 300, visible: false, preview: ""}) // typing etc.

	preview, sentMs, ok := s.LastVisible("c1")
	if !ok || preview != "second" || sentMs != 200 {
		t.Fatalf("LastVisible(c1) = (%q, %d, %v), want (second, 200, true)", preview, sentMs, ok)
	}
	if _, _, ok := s.LastVisible("ghost"); ok {
		t.Error("LastVisible(ghost) should report ok=false")
	}
}

func TestFrameStore_PromptIndex(t *testing.T) {
	s := tempFrameStore(t)
	s.PutPrompt("p1", "c1", "clutch", 100)

	convID, agentID, ok := s.PromptConv("p1")
	if !ok || convID != "c1" || agentID != "clutch" {
		t.Fatalf("PromptConv(p1) = (%q, %q, %v), want (c1, clutch, true)", convID, agentID, ok)
	}
	s.DeletePrompt("p1")
	if _, _, ok := s.PromptConv("p1"); ok {
		t.Error("PromptConv(p1) should miss after DeletePrompt")
	}
}

func TestFrameStore_TrimDropsOldPrompts(t *testing.T) {
	s := tempFrameStore(t)
	s.PutPrompt("old", "c1", "ag", 100)
	s.PutPrompt("new", "c1", "ag", 5000)
	s.TrimOlderThan(1000)
	if _, _, ok := s.PromptConv("old"); ok {
		t.Error("old prompt should be trimmed")
	}
	if _, _, ok := s.PromptConv("new"); !ok {
		t.Error("new prompt should survive trim")
	}
}

func TestFrameStore_LegacyOpenAsks(t *testing.T) {
	s := tempFrameStore(t)
	enc := func(f fap.ServerFrame, seq int64) string {
		w, err := fap.Encode(f, seq, 0, "", "")
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return w
	}
	// p1: interactive, never resolved, not indexed → legacy open.
	s.insert(frameWrite{convID: "c1", agentID: "clutch", seq: 1, sentMs: 1,
		wire: enc(fap.Interactive{ConversationID: "c1", PromptID: "p1", Text: "allow?"}, 1)})
	// p2: interactive + a later resolve → resolved.
	s.insert(frameWrite{convID: "c1", agentID: "clutch", seq: 2, sentMs: 2,
		wire: enc(fap.Interactive{ConversationID: "c1", PromptID: "p2"}, 2)})
	s.insert(frameWrite{convID: "c1", agentID: "clutch", seq: 3, sentMs: 3,
		wire: enc(fap.InteractiveEdit{ConversationID: "c1", PromptID: "p2", Text: "done"}, 3)})
	// p3: interactive but tracked in app_prompts → current-gen, leave alone.
	s.insert(frameWrite{convID: "c1", agentID: "clutch", seq: 4, sentMs: 4,
		wire: enc(fap.Interactive{ConversationID: "c1", PromptID: "p3"}, 4)})
	s.PutPrompt("p3", "c1", "clutch", 4)

	if !s.NeedsLegacyAskSweep() {
		t.Fatal("sweep should be needed before it runs")
	}
	got := s.LegacyOpenAsks()
	if len(got) != 1 || got[0].promptID != "p1" || got[0].convID != "c1" ||
		got[0].agentID != "clutch" || got[0].text != "allow?" {
		t.Fatalf("LegacyOpenAsks = %+v, want just p1 (c1/clutch/allow?)", got)
	}
	s.MarkLegacyAsksSwept()
	if s.NeedsLegacyAskSweep() {
		t.Error("sweep should be marked done after MarkLegacyAsksSwept")
	}
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
		s.Append("c1", "clutch", i, "w", time.Now().UnixMilli(), true, "")
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
	b.send(fap.Activity{ConversationID: "c1", Kind: "typing"}) // must be seq 6, not 1
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

// GET /app/replay returns the durably-stored frames > fromSeq as verbatim wires,
// with `more` signalling pagination when the page hits the limit.
func TestServeReplay_ReturnsStoredFrames(t *testing.T) {
	h := newTestHub()
	d := h.devices.pair("dev", "")
	s := tempFrameStore(t)
	h.frames = s
	now := time.Now().UnixMilli()
	for i := int64(1); i <= 5; i++ {
		s.insert(frameWrite{convID: "c1", seq: i, wire: mkWire(t, "c1", i), sentMs: now, visible: true})
	}

	// Page from seq 2 with limit 2 → frames 3,4 and more=true.
	req := httptest.NewRequest(http.MethodGet, "/app/replay?conversationId=c1&fromSeq=2&limit=2", nil)
	req.Header.Set("Authorization", "Bearer "+d.Token)
	w := httptest.NewRecorder()
	h.ServeReplay(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("replay code = %d", w.Code)
	}
	var res struct {
		Frames []struct {
			Seq  int64  `json:"seq"`
			Wire string `json:"wire"`
		} `json:"frames"`
		More bool `json:"more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Frames) != 2 || res.Frames[0].Seq != 3 || res.Frames[1].Seq != 4 {
		t.Fatalf("frames = %+v, want seq 3,4", res.Frames)
	}
	if !res.More {
		t.Errorf("more = false, want true (seq 5 still pending)")
	}
	if res.Frames[0].Wire == "" {
		t.Errorf("frame wire empty — app cannot render it")
	}

	// Final page returns the tail and more=false.
	req = httptest.NewRequest(http.MethodGet, "/app/replay?conversationId=c1&fromSeq=4&limit=2", nil)
	req.Header.Set("Authorization", "Bearer "+d.Token)
	w = httptest.NewRecorder()
	h.ServeReplay(w, req)
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if len(res.Frames) != 1 || res.Frames[0].Seq != 5 || res.More {
		t.Fatalf("final page = %+v more=%v, want [seq 5] more=false", res.Frames, res.More)
	}
}
