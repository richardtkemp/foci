package tools

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/question"
	"foci/internal/session"
)

// fakeRestore captures reattach calls so a test can drive a "button click" on a
// question whose callback was rebuilt after a simulated restart.
type fakeRestore struct {
	mu                sync.Mutex
	calls             int
	lastMsgID         string
	lastPlatformMsgID string
	onResponse        func(string)
}

func (f *fakeRestore) restore(sessionKey, msgID, platformMsgID string, choices []question.Choice, onResponse func(data string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastMsgID = msgID
	f.lastPlatformMsgID = platformMsgID
	f.onResponse = onResponse
}

func (f *fakeRestore) click(data string) {
	f.mu.Lock()
	cb := f.onResponse
	f.mu.Unlock()
	if cb != nil {
		cb(data)
	}
}

func newStateDB(t *testing.T) *session.SessionIndex {
	t.Helper()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("new session index: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

const twoQuestionAsk = `{"questions":[
	{"question":"Q1","options":[{"label":"A1a"},{"label":"A1b"}]},
	{"question":"Q2","options":[{"label":"A2a"},{"label":"A2b"}]}
]}`

// TestAskPersistOnStart verifies that posting an ask writes it to the session
// index, and that answering advances the persisted index.
func TestAskPersistOnStart(t *testing.T) {
	idx := newStateDB(t)
	p := &fakePresenter{}
	d := &fakeDeliver{}
	tool, _ := NewAskTool(p.present, nil, d.deliver, idx, "test")

	execAsk(t, tool, twoQuestionAsk)

	raw, err := idx.GetAgentMetadata("test", "ask_pending")
	if err != nil || raw == "" {
		t.Fatalf("ask not persisted on start (err=%v raw=%q)", err, raw)
	}
	var saved []persistedAsk
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(saved) != 1 {
		t.Fatalf("persisted asks = %d, want 1", len(saved))
	}
	if saved[0].Idx != 0 || len(saved[0].Questions) != 2 {
		t.Errorf("persisted ask = idx %d / %d questions, want idx 0 / 2 questions", saved[0].Idx, len(saved[0].Questions))
	}

	// Answer the first question; the persisted index must advance to 1.
	p.answer("qa:0")
	raw, _ = idx.GetAgentMetadata("test", "ask_pending")
	_ = json.Unmarshal([]byte(raw), &saved)
	if len(saved) != 1 || saved[0].Idx != 1 {
		t.Errorf("after one answer: persisted = %+v, want one ask at idx 1", saved)
	}
	if saved[0].Answers["Q1"] != "A1a" {
		t.Errorf("persisted answers = %v, want Q1→A1a", saved[0].Answers)
	}
}

// TestAskPersistClearedOnComplete verifies the durable set empties once every
// question is answered and the batch is delivered.
func TestAskPersistClearedOnComplete(t *testing.T) {
	idx := newStateDB(t)
	p := &fakePresenter{}
	d := &fakeDeliver{}
	tool, _ := NewAskTool(p.present, nil, d.deliver, idx, "test")

	execAsk(t, tool, twoQuestionAsk)
	p.answer("qa:0") // Q1
	p.answer("qa:1") // Q2 → done

	if _, ok := d.last(); !ok {
		t.Fatal("answer batch not delivered after completion")
	}
	raw, _ := idx.GetAgentMetadata("test", "ask_pending")
	var saved []persistedAsk
	_ = json.Unmarshal([]byte(raw), &saved)
	if len(saved) != 0 {
		t.Errorf("persisted asks after completion = %d, want 0", len(saved))
	}
}

// TestAskRestoreRoundTrip is the core test: an ask started + partially answered
// on one instance survives a simulated restart on a second instance built from
// the same store. The typed-answer index is rehydrated AND the reattached button
// callback resolves the remaining question.
func TestAskRestoreRoundTrip(t *testing.T) {
	idx := newStateDB(t)

	// Instance 1: post a 2-question ask and answer the first.
	p1 := &fakePresenter{}
	d1 := &fakeDeliver{}
	tool1, _ := NewAskTool(p1.present, nil, d1.deliver, idx, "test")
	execAsk(t, tool1, twoQuestionAsk)
	p1.answer("qa:0") // Q1 → A1a; now positioned on Q2

	// Instance 2: fresh tool over the SAME store = a restart. Construction
	// rehydrates the pending ask and reattaches its (Q2) buttons.
	fr := &fakeRestore{}
	p2 := &fakePresenter{}
	d2 := &fakeDeliver{}
	_, router2 := NewAskTool(p2.present, fr.restore, d2.deliver, idx, "test")

	// Typed-answer routing restored: the session has a pending ask again.
	reqID := router2.PendingForSession(askSession)
	if reqID == "" {
		t.Fatal("pending ask not restored for session after restart")
	}

	// Button routing restored: reattach fired for the current (Q2) question, and
	// clicking the existing button resolves the ask and delivers the batch.
	if fr.calls != 1 {
		t.Fatalf("reattach calls = %d, want 1", fr.calls)
	}
	if want := reqID + "-q1"; fr.lastMsgID != want {
		t.Errorf("reattach msgID = %q, want %q (current question index)", fr.lastMsgID, want)
	}
	fr.click("qa:1") // Q2 → A2b → done

	msg, ok := d2.last()
	if !ok {
		t.Fatal("restored ask did not deliver after final answer")
	}
	if !strings.Contains(msg, "A1a") || !strings.Contains(msg, "A2b") {
		t.Errorf("delivered batch missing carried answers: %q", msg)
	}
	// Durable set must be empty again.
	raw, _ := idx.GetAgentMetadata("test", "ask_pending")
	var saved []persistedAsk
	_ = json.Unmarshal([]byte(raw), &saved)
	if len(saved) != 0 {
		t.Errorf("persisted asks after restored completion = %d, want 0", len(saved))
	}
}

// TestAskRestorePersistsPlatformMsgID verifies the platform-side message id the
// presenter reports is persisted and handed back to restore after a restart, so a
// restored ask can address its on-screen message for cancel/expiry edits.
func TestAskRestorePersistsPlatformMsgID(t *testing.T) {
	idx := newStateDB(t)

	// Instance 1: the presenter reports a platform message id for each question.
	p1 := &fakePresenter{platformMsgID: "tg-42"}
	d1 := &fakeDeliver{}
	tool1, _ := NewAskTool(p1.present, nil, d1.deliver, idx, "test")
	execAsk(t, tool1, twoQuestionAsk)

	// It must be captured in the durable set, not just held in memory.
	raw, _ := idx.GetAgentMetadata("test", "ask_pending")
	var saved []persistedAsk
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].PlatformMsgID != "tg-42" {
		t.Fatalf("persisted platform_msg_id = %+v, want one entry with tg-42", saved)
	}

	// Instance 2 (restart): restore must receive the persisted platform msgID.
	fr := &fakeRestore{}
	p2 := &fakePresenter{}
	d2 := &fakeDeliver{}
	_, _ = NewAskTool(p2.present, fr.restore, d2.deliver, idx, "test")
	if fr.lastPlatformMsgID != "tg-42" {
		t.Errorf("restore platformMsgID = %q, want tg-42", fr.lastPlatformMsgID)
	}
}

// TestAskRestoreDropsStale verifies asks older than pendingAskTTL are not
// restored and are pruned from the durable set.
func TestAskRestoreDropsStale(t *testing.T) {
	idx := newStateDB(t)
	stale := []persistedAsk{{
		RequestID:  "ask-test-99",
		SessionKey: askSession,
		Questions:  []question.Question{{Question: "Q1", Options: []question.Option{{Label: "A"}}}},
		Idx:        0,
		CreatedAt:  time.Now().Add(-2 * pendingAskTTL),
	}}
	data, _ := json.Marshal(stale)
	if err := idx.SetAgentMetadata("test", "ask_pending", string(data)); err != nil {
		t.Fatal(err)
	}

	fr := &fakeRestore{}
	p := &fakePresenter{}
	d := &fakeDeliver{}
	_, router := NewAskTool(p.present, fr.restore, d.deliver, idx, "test")

	if reqID := router.PendingForSession(askSession); reqID != "" {
		t.Errorf("stale ask restored (reqID=%q), want dropped", reqID)
	}
	if fr.calls != 0 {
		t.Errorf("reattach fired for stale ask (%d calls), want 0", fr.calls)
	}
	raw, _ := idx.GetAgentMetadata("test", "ask_pending")
	var saved []persistedAsk
	_ = json.Unmarshal([]byte(raw), &saved)
	if len(saved) != 0 {
		t.Errorf("stale ask not pruned from store: %+v", saved)
	}
}

// TestAskPausePersistsAcrossRestart verifies a /pause set on one instance
// survives a simulated restart: the rehydrated ask is still paused.
func TestAskPausePersistsAcrossRestart(t *testing.T) {
	idx := newStateDB(t)

	// Instance 1: post an ask and pause it.
	p1 := &fakePresenter{}
	d1 := &fakeDeliver{}
	tool1, router1 := NewAskTool(p1.present, nil, d1.deliver, idx, "test")
	execAsk(t, tool1, twoQuestionAsk)
	if !router1.PauseSession(askSession) {
		t.Fatal("PauseSession should succeed for the pending ask")
	}

	// The durable set must carry the paused flag.
	raw, _ := idx.GetAgentMetadata("test", "ask_pending")
	var saved []persistedAsk
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || !saved[0].Paused {
		t.Fatalf("persisted paused flag = %+v, want one entry with paused=true", saved)
	}

	// Instance 2 (restart) over the same store: the ask is restored paused.
	fr := &fakeRestore{}
	p2 := &fakePresenter{}
	d2 := &fakeDeliver{}
	_, router2 := NewAskTool(p2.present, fr.restore, d2.deliver, idx, "test")
	if !router2.IsPaused(askSession) {
		t.Error("restored ask should still be paused after restart")
	}
	// And /resume on the restored instance clears it.
	if !router2.ResumeSession(askSession) {
		t.Fatal("ResumeSession should succeed for the restored ask")
	}
	if router2.IsPaused(askSession) {
		t.Error("ask should not be paused after ResumeSession")
	}
}

// TestAskNoPersistenceWithoutStore verifies a nil store keeps the tool working
// purely in-memory (no panic, no persistence side effects).
func TestAskNoPersistenceWithoutStore(t *testing.T) {
	p := &fakePresenter{}
	d := &fakeDeliver{}
	tool, _ := NewAskTool(p.present, nil, d.deliver, nil, "test")
	execAsk(t, tool, twoQuestionAsk)
	p.answer("qa:0")
	p.answer("qa:1")
	if _, ok := d.last(); !ok {
		t.Error("in-memory ask (nil store) failed to deliver")
	}
}
