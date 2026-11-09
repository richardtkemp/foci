package app

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/delegator/accounting"
	"foci/internal/fap"
	"foci/internal/modelinfo"
	"foci/internal/turnevent"
	"foci/internal/voice"
)

// TestTurnInfoMeter (#2992): the sink's per-turn measurement. Tool time is the
// UNION of the tool intervals (two parallel tools count once), a tool still
// open at the end counts up to it, and generation time is the rest.
func TestTurnInfoMeter(t *testing.T) {
	at := func(ms int) time.Time { return time.UnixMilli(int64(1_000_000 + ms)) }
	var m turnInfoMeter
	m.observe(turnevent.TurnStart{}, at(0))
	m.observe(turnevent.ThinkingDelta{}, at(400)) // first output
	m.observe(turnevent.ToolCall{ID: "a"}, at(1000))
	m.observe(turnevent.ToolCall{ID: "b"}, at(1500)) // parallel with a
	m.observe(turnevent.ToolResult{ID: "a"}, at(2000))
	m.observe(turnevent.ToolResult{ID: "b"}, at(3000)) // tools ran 1000..3000
	m.observe(turnevent.TextDelta{}, at(3500))
	m.observe(turnevent.ToolCall{ID: "c"}, at(4000)) // never finishes
	r := m.record(turnevent.TurnComplete{TurnID: "a/c1@1", Trigger: "voice", Via: "voice", Effort: "high", DispatchedAt: at(250)}, at(5000))
	if r.dispatchedMs-r.startMs != 250 {
		t.Errorf("dispatched %d ms after the start, want 250", r.dispatchedMs-r.startMs)
	}

	if !r.measured || r.toolCalls != 3 {
		t.Fatalf("record = %+v, want measured with 3 tool calls", r)
	}
	if r.toolMs != 2000+1000 {
		t.Errorf("tool time = %d ms, want 3000 (1000..3000 once, plus the open tool 4000..5000)", r.toolMs)
	}
	if r.firstOutputMs-r.startMs != 400 || r.endMs-r.startMs != 5000 {
		t.Errorf("first output %d ms, wall %d ms; want 400, 5000", r.firstOutputMs-r.startMs, r.endMs-r.startMs)
	}
	if r.turnID != "a/c1@1" || r.trigger != "voice" || r.via != "voice" || r.effort != "high" {
		t.Errorf("identity = %+v", r)
	}

	// No TurnStart seen: the identity is kept, no timing is made up.
	var none turnInfoMeter
	if r := none.record(turnevent.TurnComplete{TurnID: "x"}, at(1)); r.measured || r.turnID != "x" {
		t.Errorf("unstarted record = %+v, want identity only", r)
	}
}

// TestAppSink_RecordsMessageInfo (#2992): a turn that delivered two bubbles
// files its info under both, the voice clip's synthesis under the bubble it
// reads, and the answer joins the ledger for usage and cost — naming the
// unpriced call instead of pricing it at $0.
func TestAppSink_RecordsMessageInfo(t *testing.T) {
	h := newTestHub()
	h.frames = tempFrameStore(t)
	b := h.ensureBinding(nil, "arnix", "conv-1")
	client := fakeClient()
	b.attach(client)
	tts := &voice.FallbackTTS{Chain: []voice.ChainEntry{
		{ID: "groq", TTS: &mockVoiceTTS{err: errors.New("down")}},
		{ID: "edge-tts", TTS: &mockVoiceTTS{data: []byte("synth")}},
	}}
	conn := &appConn{hub: h, agentID: "arnix", tts: tts}
	sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: b.key()})
	ctx := agent.WithTrigger(context.Background(), "voice")
	turnID := b.key() + "@1"
	dispatched := time.Now()
	for _, ev := range []turnevent.Event{
		turnevent.TurnStart{},
		turnevent.ToolCall{ID: "t1", Name: "Bash"},
		turnevent.ToolResult{ID: "t1"},
		turnevent.TextBlock{Text: "first bubble", Phase: turnevent.PhaseIntermediate},
		turnevent.TextBlock{Text: "second bubble", Phase: turnevent.PhaseIntermediate},
		turnevent.TurnComplete{FinalText: "first bubble\n\nsecond bubble", TurnID: turnID, Trigger: "voice", Via: "voice", Effort: "high",
			DispatchedAt: dispatched},
	} {
		sink.Emit(ctx, ev)
	}
	cleanup()

	var bubbles []string
	for _, f := range drain(t, client) {
		if f.t == fap.TypeMessage {
			id, _ := f.d["messageId"].(string)
			bubbles = append(bubbles, id)
		}
	}
	if len(bubbles) != 2 {
		t.Fatalf("bubbles = %v, want 2", bubbles)
	}

	// With no ledger: the sink's own figures, nothing invented for the rest.
	info := buildMessageInfo(h.frames, nil, fap.MessageInfoGet{RequestID: "r", ConversationID: "conv-1", MessageID: bubbles[1]}, b.key())
	tr := info.Turn
	if tr == nil || tr.TurnID != turnID || tr.Messages != 2 || tr.Trigger != "voice" || tr.Via != "voice" || tr.Effort != "high" {
		t.Fatalf("turn = %+v, want %s with 2 messages", tr, turnID)
	}
	if tr.ToolCalls == nil || *tr.ToolCalls != 1 || tr.GenerationMs == nil || tr.FirstOutputMs == nil || tr.WaitMs == nil {
		t.Errorf("measured figures = calls %v gen %v first %v wait %v", tr.ToolCalls, tr.GenerationMs, tr.FirstOutputMs, tr.WaitMs)
	}
	if tr.CostUsd != nil || tr.Calls != nil || tr.Tokens != nil || tr.Model != "" {
		t.Errorf("ledger figures without a ledger: %+v", tr)
	}
	if info.Voice == nil || info.Voice.Provider != "edge-tts" {
		t.Errorf("voice = %+v, want the edge-tts fallback that spoke", info.Voice)
	}

	// With the ledger: usage and cost of that turn.
	l, _, err := accounting.Open(filepath.Join(t.TempDir(), "api.db"), accounting.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	start := time.Now().Add(-time.Minute)
	tt := accounting.Turn{TurnID: turnID, Session: b.key(), Backend: accounting.BackendAPI, Source: accounting.SourceUser,
		StartedAt: start, EndedAt: start.Add(10 * time.Second), StopReason: "end_turn", FinalModel: "claude-opus-5", ActivityClosedAt: start.Add(10 * time.Second)}
	for i, c := range []accounting.APIResponse{
		{ID: "m1", Model: "claude-opus-5", Tokens: modelinfo.Tokens{modelinfo.ClassInput: 10, modelinfo.ClassCacheRead: 900, modelinfo.ClassOutput: 40}},
		{ID: "m2", Model: "no-such-model-anywhere", Tokens: modelinfo.Tokens{modelinfo.ClassInput: 1, modelinfo.ClassOutput: 1}},
	} {
		c.Kind, c.Session, c.TurnID, c.Start = accounting.KindCall, b.key(), turnID, start.Add(time.Duration(i+1)*time.Second)
		if _, err := l.RecordCall(tt, c.Call(), nil); err != nil {
			t.Fatal(err)
		}
	}
	tr = buildMessageInfo(h.frames, l, fap.MessageInfoGet{RequestID: "r", ConversationID: "conv-1", MessageID: bubbles[0]}, b.key()).Turn
	if tr == nil || tr.Model != "claude-opus-5" || tr.StopReason != "end_turn" || tr.Backend != accounting.BackendAPI {
		t.Fatalf("ledger turn = %+v", tr)
	}
	if tr.Calls == nil || *tr.Calls != 2 || tr.Tokens["output"] != 41 || tr.Tokens["cache_read"] != 900 {
		t.Errorf("calls %v tokens %v, want 2 calls, 41 output, 900 cache read", tr.Calls, tr.Tokens)
	}
	want, _ := modelinfo.CostAsOf("claude-opus-5", start, modelinfo.Tokens{modelinfo.ClassInput: 10, modelinfo.ClassCacheRead: 900, modelinfo.ClassOutput: 40})
	if tr.CostUsd == nil || math.Abs(*tr.CostUsd-want) > 1e-12 || tr.UnpricedCalls != 1 {
		t.Errorf("cost %v (%d unpriced), want $%v and 1 unpriced", tr.CostUsd, tr.UnpricedCalls, want)
	}
	if !tr.SpawnsRecorded || len(tr.Spawns) != 0 {
		t.Errorf("spawns = %+v (recorded %v), want a record of none", tr.Spawns, tr.SpawnsRecorded)
	}
	if err := l.RecordSpawn(accounting.SpawnLink{SpawnID: "spawn-1", ParentTurnID: turnID, ParentSession: b.key(), Name: "scout", Mode: "character", StartedAt: start}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetSpawnBooking("spawn-1", "", "", true); err != nil {
		t.Fatal(err)
	}
	tr = buildMessageInfo(h.frames, l, fap.MessageInfoGet{RequestID: "r", ConversationID: "conv-1", MessageID: bubbles[0]}, b.key()).Turn
	if len(tr.Spawns) != 1 || tr.Spawns[0].Name != "scout" || !tr.Spawns[0].InTurnCost || tr.Spawns[0].CostUsd != nil {
		t.Errorf("spawns = %+v, want scout, booked on the turn", tr.Spawns)
	}
	if info := buildMessageInfo(h.frames, l, fap.MessageInfoGet{RequestID: "r", ConversationID: "conv-1", MessageID: "never-sent"}, b.key()); info.Turn != nil || info.Note == "" {
		t.Errorf("unknown message = %+v, want only a note", info)
	}
}

// TestUserMessageInfo (#2992): a user message's receipt and the delivery the
// agent recorded for it. A message that waited and then began a turn stays
// "queued" with that turn; any other outcome replaces what was there; no
// outcome recorded reads as none.
func TestUserMessageInfo(t *testing.T) {
	store := tempFrameStore(t)
	at := time.UnixMilli(1_800_000_000_000)
	for _, tc := range []struct {
		name              string
		reports           [][2]string // outcome, turn
		wantHow, wantTurn string
	}{
		{"started", [][2]string{{"started", "s@1"}}, "started", "s@1"},
		{"queued then started", [][2]string{{"queued", ""}, {"started", "s@2"}}, "queued", "s@2"},
		{"still queued", [][2]string{{"queued", ""}}, "queued", ""},
		{"queued then joined", [][2]string{{"queued", ""}, {"joined", "s@1"}}, "joined", "s@1"},
		{"steered", [][2]string{{"steered", "s@1"}}, "steered", "s@1"},
		{"not recorded", nil, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "u-" + tc.name
			store.PutUserMessageReceipt("c1", id, at.UnixMilli()-50, at.UnixMilli())
			store.PutUserMessageReceipt("c1", id, 1, at.UnixMilli()+999) // a replayed copy moves nothing
			for i, r := range tc.reports {
				store.PutUserDelivery("c1", id, r[0], r[1], at.Add(time.Duration(i+1)*time.Second))
			}
			info := buildMessageInfo(store, nil, fap.MessageInfoGet{RequestID: "r", ConversationID: "c1", MessageID: id}, "ag/c1")
			u := info.User
			if u == nil || info.Turn != nil {
				t.Fatalf("info = %+v, want a user answer", info)
			}
			if u.DeviceSentMs != at.UnixMilli()-50 || u.ServerReceivedMs != at.UnixMilli() {
				t.Errorf("sent/received = %d/%d, want the first record", u.DeviceSentMs, u.ServerReceivedMs)
			}
			if u.Delivery != tc.wantHow || u.TurnID != tc.wantTurn {
				t.Errorf("delivery = %q in %q, want %q in %q", u.Delivery, u.TurnID, tc.wantHow, tc.wantTurn)
			}
			wantAt := int64(0)
			for i, r := range tc.reports {
				if r[0] != "queued" {
					wantAt = at.Add(time.Duration(i+1) * time.Second).UnixMilli()
				}
			}
			if u.DeliveredMs != wantAt {
				t.Errorf("delivered at %d, want %d (the last non-queued decision)", u.DeliveredMs, wantAt)
			}
		})
	}
	// A message that never came through the app has no receipt row: nothing
	// is invented for it.
	store.PutUserDelivery("c1", "elsewhere", "started", "s@1", at)
	if info := buildMessageInfo(store, nil, fap.MessageInfoGet{RequestID: "r", ConversationID: "c1", MessageID: "elsewhere"}, "ag/c1"); info.User != nil {
		t.Errorf("unknown message got a delivery: %+v", info.User)
	}
}

// TestAppSink_NoDispatchNoModelTime (#2992): a turn whose dispatch moment was
// not recorded (an adopted autonomous run) has no first-output, wait or
// generation time: they are never measured from the sink's TurnStart.
func TestAppSink_NoDispatchNoModelTime(t *testing.T) {
	h := newTestHub()
	h.frames = tempFrameStore(t)
	b := h.ensureBinding(nil, "arnix", "conv-1")
	b.attach(fakeClient())
	sink, cleanup := (&appConn{hub: h, agentID: "arnix"}).NewTurnSink(agent.Envelope{SessionKey: b.key()})
	ctx := context.Background()
	for _, ev := range []turnevent.Event{
		turnevent.TurnStart{},
		turnevent.TextBlock{Text: "reply", Phase: turnevent.PhaseIntermediate},
		turnevent.TurnComplete{FinalText: "reply", TurnID: b.key() + "@9"},
	} {
		sink.Emit(ctx, ev)
	}
	cleanup()
	var id string
	if err := h.frames.db.QueryRow(`SELECT message_id FROM app_message_info WHERE turn_id = ?`, b.key()+"@9").Scan(&id); err != nil {
		t.Fatal(err)
	}
	tr := buildMessageInfo(h.frames, nil, fap.MessageInfoGet{RequestID: "r", ConversationID: "conv-1", MessageID: id}, b.key()).Turn
	if tr == nil || tr.ToolCalls == nil {
		t.Fatalf("turn = %+v, want the sink's tool count", tr)
	}
	if tr.FirstOutputMs != nil || tr.GenerationMs != nil || tr.WaitMs != nil {
		t.Errorf("model times without a dispatch: first %v gen %v wait %v", tr.FirstOutputMs, tr.GenerationMs, tr.WaitMs)
	}
}
