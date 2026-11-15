package app

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/fap"
)

// #1646: the frame store keeps only the newest stored copy of a whole-state
// frame (depth.feed per conversation, interactive.progressEdit per ask).

func storedTypesBySeq(t *testing.T, fs *frameStore, convID string) map[int64]string {
	t.Helper()
	out := map[int64]string{}
	for _, r := range fs.Range(convID, 0, 1000) {
		var env fap.Envelope
		if err := json.Unmarshal([]byte(r.wire), &env); err != nil {
			t.Fatal(err)
		}
		out[r.seq] = env.T
	}
	return out
}

func sendStored(t *testing.T, b *convBinding, f fap.ServerFrame) {
	t.Helper()
	done := make(chan struct{})
	b.sendThen(f, func() { close(done) })
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s not stored", f.Type())
	}
}

func TestSupersede_KeepsNewestWholeStateCopy(t *testing.T) {
	fs := tempFrameStore(t)
	b := &convBinding{convID: "c1", store: fs, seen: map[string]struct{}{}}
	other := &convBinding{convID: "c2", store: fs, seen: map[string]struct{}{}}

	sendStored(t, b, fap.DepthFeed{ConversationID: "c1", RootState: "active"})                                                   // 1
	sendStored(t, b, fap.ServerMessage{ConversationID: "c1", MessageID: "m1", Role: "agent", Text: "hi"})                        // 2
	sendStored(t, b, fap.InteractiveProgressEdit{ConversationID: "c1", PromptID: "p1", Answers: []string{"a", ""}})              // 3
	sendStored(t, b, fap.InteractiveProgressEdit{ConversationID: "c1", PromptID: "p2", Answers: []string{"x"}})                  // 4
	sendStored(t, b, fap.DepthFeed{ConversationID: "c1", RootState: "fanned"})                                                   // 5
	sendStored(t, b, fap.InteractiveProgressEdit{ConversationID: "c1", PromptID: "p1", Answers: []string{"a", "b"}, Done: true}) // 6
	sendStored(t, other, fap.DepthFeed{ConversationID: "c2", RootState: "active"})                                               // c2 seq 1

	got := storedTypesBySeq(t, fs, "c1")
	want := map[int64]string{
		2: fap.TypeMessage,
		4: fap.TypeInteractiveProgressEdit, // p2's only copy
		5: fap.TypeDepthFeed,
		6: fap.TypeInteractiveProgressEdit, // p1's newest
	}
	if len(got) != len(want) {
		t.Fatalf("stored c1 = %v, want %v", got, want)
	}
	for seq, typ := range want {
		if got[seq] != typ {
			t.Errorf("seq %d = %q, want %q (stored %v)", seq, got[seq], typ, got)
		}
	}
	if g := storedTypesBySeq(t, fs, "c2"); len(g) != 1 {
		t.Errorf("another conversation's depth.feed was touched: %v", g)
	}
	if got := fs.MaxSeq("c1"); got != 6 {
		t.Errorf("MaxSeq = %d, want 6", got)
	}

	// Seqs continue past the high-water after a restart.
	b2 := &convBinding{convID: "c1", store: fs, seq: fs.MaxSeq("c1"), seen: map[string]struct{}{}}
	c := fakeClient()
	b2.attach(c)
	b2.send(fap.DepthFeed{ConversationID: "c1", RootState: "active"})
	if ds := drainEnv(t, c); len(ds) != 1 || ds[0].seq != 7 {
		t.Errorf("next send = %v, want seq 7", ds)
	}
}

// The one-off prune removes copies stored before #1646, keeps each newest,
// keeps every conversation's top seq, and runs once.
func TestSupersede_UpgradePrunesExistingCopies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frames.db")
	fs, err := newFrameStore(path, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.db.Exec(`DELETE FROM app_migrations WHERE name = ?`, migrationLatestOnly1646); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	put := func(conv string, seq int64, f fap.ServerFrame) {
		w, err := fap.Encode(f, seq, 0, "", "")
		if err != nil {
			t.Fatal(err)
		}
		// Raw insert: the pre-#1646 store held every copy.
		if _, err := fs.db.Exec(`INSERT INTO app_frames (conv_id, seq, wire, sent_ms, visible, agent_id) VALUES (?, ?, ?, ?, 0, 'ag')`, conv, seq, w, now); err != nil {
			t.Fatal(err)
		}
	}
	put("c1", 1, fap.DepthFeed{ConversationID: "c1", RootState: "active"})
	put("c1", 2, fap.ServerMessage{ConversationID: "c1", MessageID: "m", Role: "agent", Text: "x"})
	put("c1", 3, fap.InteractiveProgressEdit{ConversationID: "c1", PromptID: "p1", Answers: []string{""}})
	put("c1", 4, fap.DepthFeed{ConversationID: "c1", RootState: "fanned"})
	put("c1", 5, fap.InteractiveProgressEdit{ConversationID: "c1", PromptID: "p1", Answers: []string{"a"}})
	put("c1", 6, fap.DepthFeed{ConversationID: "c1", RootState: "active"}) // top
	fs.Close()

	fs2, err := newFrameStore(path, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got := storedTypesBySeq(t, fs2, "c1")
	if len(got) != 3 || got[2] != fap.TypeMessage || got[5] != fap.TypeInteractiveProgressEdit || got[6] != fap.TypeDepthFeed {
		t.Errorf("after prune c1 = %v, want seqs 2, 5, 6", got)
	}
	var hw int64
	if err := fs2.db.QueryRow(`SELECT max_seq FROM app_conv_seq WHERE conv_id = 'c1'`).Scan(&hw); err != nil || hw != 6 {
		t.Errorf("app_conv_seq = %d (%v), want 6", hw, err)
	}
	// Rows put back by hand (as a rolled-back binary would) stay: it ran once.
	if _, err := fs2.db.Exec(`INSERT INTO app_frames (conv_id, seq, wire, sent_ms, visible, agent_id) SELECT 'c9', 1, wire, sent_ms, 0, 'ag' FROM app_frames WHERE conv_id='c1' AND seq=6`); err != nil {
		t.Fatal(err)
	}
	if _, err := fs2.db.Exec(`INSERT INTO app_frames (conv_id, seq, wire, sent_ms, visible, agent_id) SELECT 'c9', 2, wire, sent_ms, 0, 'ag' FROM app_frames WHERE conv_id='c1' AND seq=6`); err != nil {
		t.Fatal(err)
	}
	fs2.Close()
	fs3, err := newFrameStore(path, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer fs3.Close()
	if g := storedTypesBySeq(t, fs3, "c9"); len(g) != 2 {
		t.Errorf("second open pruned again: %v", g)
	}
}
