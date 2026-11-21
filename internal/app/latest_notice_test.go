package app

import (
	"encoding/json"
	"testing"
	"time"

	"foci/internal/fap"
	"foci/internal/platform"
)

// #3248 (Dick 2026-11-21: "only the most-rceently-rceieved should ever be
// visible in chat"): each usage notice in a chat goes under the
// conversation's one fixed messageId, so the client replaces the earlier row,
// and the frame store keeps only the newest copy for replay and history.

func TestSendLatestOnlyNotification_OneIDPerConversation(t *testing.T) {
	h, c, b, conn := boundConn(t)
	b.store = tempFrameStore(t)
	b.seen = map[string]struct{}{}

	conn.SendLatestOnlyNotification(platform.NoticeKindUsage, "⚠️ Approaching Anthropic 7-day rate limit.\nUsage: 50%")
	plainID := conn.SendNotificationDirect("✅ Context compacted")
	conn.SendLatestOnlyNotification(platform.NoticeKindUsage, "⚠️ Approaching Anthropic 7-day rate limit.\nUsage: 60%")
	sendStored(t, b, fap.ServerMessage{ConversationID: "c1", MessageID: "m1", Role: "agent", Text: "hi"})

	want := latestNoticeID(platform.NoticeKindUsage, "c1")
	ds := drain(t, c)
	var usageIDs []any
	for _, d := range ds {
		if d.t == fap.TypeNotification && d.d["messageId"] != plainID {
			usageIDs = append(usageIDs, d.d["messageId"])
		}
	}
	if len(usageIDs) != 2 || usageIDs[0] != want || usageIDs[1] != want {
		t.Fatalf("live usage notice ids = %v, want two of %q (same id replaces the row)", usageIDs, want)
	}
	if h.bindingForNotification(want) != nil {
		t.Errorf("a latest-only notice was registered for an edit")
	}

	// Stored: the plain notice, the NEWEST usage notice, the message.
	var texts []string
	for _, r := range b.store.Range("c1", 0, 1000) {
		var env struct {
			T string           `json:"t"`
			D fap.Notification `json:"d"`
		}
		if err := json.Unmarshal([]byte(r.wire), &env); err != nil {
			t.Fatal(err)
		}
		if env.T == fap.TypeNotification {
			texts = append(texts, env.D.Text)
		}
	}
	if len(texts) != 2 || texts[0] != "✅ Context compacted" || texts[1] != "⚠️ Approaching Anthropic 7-day rate limit.\nUsage: 60%" {
		t.Errorf("stored notifications = %q, want the plain one and only the newest usage notice", texts)
	}
}

func TestSupersede_LatestOnlyNoticePerMessageID(t *testing.T) {
	fs := tempFrameStore(t)
	b := &convBinding{convID: "c1", store: fs, seen: map[string]struct{}{}}
	other := &convBinding{convID: "c2", store: fs, seen: map[string]struct{}{}}
	usage1 := latestNoticeID(platform.NoticeKindUsage, "c1")

	sendStored(t, b, fap.Notification{ConversationID: "c1", MessageID: usage1, Text: "50%"})                                            // 1
	sendStored(t, b, fap.Notification{ConversationID: "c1", MessageID: "n1", Text: "plain"})                                            // 2
	sendStored(t, b, fap.Notification{ConversationID: "c1", MessageID: latestNoticeID("other", "c1"), Text: "other"})                   // 3
	sendStored(t, b, fap.Notification{ConversationID: "c1", MessageID: "n1", Text: "plain edited"})                                     // 4: not latest-only, kept
	sendStored(t, b, fap.Notification{ConversationID: "c1", MessageID: usage1, Text: "60%"})                                            // 5
	sendStored(t, other, fap.Notification{ConversationID: "c2", MessageID: latestNoticeID(platform.NoticeKindUsage, "c2"), Text: "c2"}) // c2 seq 1

	got := storedTypesBySeq(t, fs, "c1")
	if len(got) != 4 || got[1] != "" || got[2] == "" || got[3] == "" || got[4] == "" || got[5] == "" {
		t.Errorf("stored c1 seqs = %v, want 2, 3, 4, 5 (the older usage copy deleted)", got)
	}
	if g := storedTypesBySeq(t, fs, "c2"); len(g) != 1 {
		t.Errorf("another conversation's usage notice was touched: %v", g)
	}
	if got := fs.MaxSeq("c1"); got != 5 {
		t.Errorf("MaxSeq = %d, want 5", got)
	}
}

// #3249: a usage notice carries its windows on the wire, for the app's pill,
// under the same latest-only id.
func TestSendUsageNotice_CarriesWindows(t *testing.T) {
	_, c, _, conn := boundConn(t)
	resets := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	conn.SendUsageNotice("⚠️ Approaching Anthropic 7-day rate limit.", &platform.UsageSnapshot{
		Windows: []platform.UsageSnapshotWindow{
			{Label: "5h", Length: 5 * time.Hour, UsedPct: 34},
			{Label: "7d", Length: 7 * 24 * time.Hour, UsedPct: 91, ResetsAt: resets},
		},
		Warned: "7d",
	})
	conn.SendUsageNotice("plain", nil)
	ds := drain(t, c)
	if len(ds) != 2 {
		t.Fatalf("frames = %v, want two notifications", types(ds))
	}
	want := latestNoticeID(platform.NoticeKindUsage, "c1")
	for _, d := range ds {
		if d.t != fap.TypeNotification || d.d["messageId"] != want {
			t.Errorf("frame %s id %v, want a notification with id %q", d.t, d.d["messageId"], want)
		}
	}
	u, _ := ds[0].d["usage"].(map[string]any)
	ws, _ := u["windows"].([]any)
	if u == nil || u["warned"] != "7d" || len(ws) != 2 {
		t.Fatalf("usage = %v, want two windows, warned 7d", ds[0].d["usage"])
	}
	w7 := ws[1].(map[string]any)
	if w7["label"] != "7d" || w7["usedPct"] != float64(91) || w7["resetsMs"] != float64(resets.UnixMilli()) {
		t.Errorf("7d window = %v", w7)
	}
	if w5 := ws[0].(map[string]any); w5["resetsMs"] != nil {
		t.Errorf("an unknown reset should be omitted: %v", w5)
	}
	if _, has := ds[1].d["usage"]; has {
		t.Errorf("a notice with no snapshot carried usage: %v", ds[1].d)
	}
}
