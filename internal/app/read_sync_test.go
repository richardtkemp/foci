package app

import (
	"testing"

	"foci/internal/fap"
	"foci/internal/platform"
)

func lastReadSync(t *testing.T, c *wsClient) map[string]string {
	t.Helper()
	var out map[string]string
	for _, f := range drain(t, c) {
		if f.t != fap.TypeReadSync {
			continue
		}
		out = map[string]string{}
		out["conversationId"], _ = f.d["conversationId"].(string)
		out["messageId"], _ = f.d["messageId"].(string)
	}
	return out
}

// TestHandleRead_PersistsAndFansOutToOtherClients proves a read persists the
// watermark and mirrors a ReadSync to the OTHER clients — never echoing to the
// device that sent it.
func TestHandleRead_PersistsAndFansOutToOtherClients(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42, sessionKey: "clutch/c42"}

	sender := fakeClient()
	other := fakeClient()
	h.clients[sender] = struct{}{}
	h.clients[other] = struct{}{}

	h.handleRead(sender, fap.Read{ConversationID: "c1", MessageID: "m9"})

	if v, _ := idx.GetChatMetadata("clutch", "app", 42, "last_read"); v != "m9" {
		t.Errorf("last_read = %q, want m9", v)
	}
	rs := lastReadSync(t, other)
	if rs["conversationId"] != "c1" || rs["messageId"] != "m9" {
		t.Errorf("other client ReadSync = %v, want c1/m9", rs)
	}
	if len(drain(t, sender)) != 0 {
		t.Error("sender must not receive its own read echo")
	}
}

// TestPushReads_ReplaysStoredWatermark proves a just-connected client is replayed
// each conversation's stored read watermark.
func TestPushReads_ReplaysStoredWatermark(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42}
	_ = idx.SetChatMetadata("clutch", "app", 42, "last_read", "m5")

	c := fakeClient()
	h.pushReads(c)
	if rs := lastReadSync(t, c); rs["messageId"] != "m5" {
		t.Errorf("pushReads ReadSync = %v, want m5", rs)
	}
}

// TestHandleRead_IgnoresUnknownConversation proves a read for a conversation with
// no live binding is a no-op (no watermark written).
func TestHandleRead_IgnoresUnknownConversation(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.handleRead(fakeClient(), fap.Read{ConversationID: "ghost", MessageID: "m1"})
	if v, _ := idx.GetChatMetadata("clutch", "app", 42, "last_read"); v != "" {
		t.Error("read for unknown conv must not persist")
	}
}

// Two ULIDs minted a second apart: older sorts before newer.
const (
	ulidOlder = "01K6B0000000000000000000AA"
	ulidNewer = "01K6B0001000000000000000AA"
)

// TestHandleRead_StaleReadDoesNotRegressWatermark proves the stored watermark
// only moves forward (#1204): a stale device's Read of an OLDER message must not
// overwrite a newer stored watermark — that value is what pushReads replays to
// every reconnecting device — nor be mirrored to the other devices.
func TestHandleRead_StaleReadDoesNotRegressWatermark(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42, sessionKey: "clutch/c42"}
	stale := fakeClient()
	other := fakeClient()
	h.clients[stale] = struct{}{}
	h.clients[other] = struct{}{}
	_ = idx.SetChatMetadata("clutch", "app", 42, "last_read", ulidNewer)

	h.handleRead(stale, fap.Read{ConversationID: "c1", MessageID: ulidOlder})

	if v, _ := idx.GetChatMetadata("clutch", "app", 42, "last_read"); v != ulidNewer {
		t.Errorf("last_read = %q after a stale read, want it kept at %q", v, ulidNewer)
	}
	if rs := lastReadSync(t, other); rs != nil {
		t.Errorf("stale read must not be mirrored, other got %v", rs)
	}

	// A genuinely newer read still advances and fans out.
	newest := "01K6B0002000000000000000AA"
	h.handleRead(stale, fap.Read{ConversationID: "c1", MessageID: newest})
	if v, _ := idx.GetChatMetadata("clutch", "app", 42, "last_read"); v != newest {
		t.Errorf("last_read = %q, want %q (a newer read must advance)", v, newest)
	}
	if rs := lastReadSync(t, other); rs["messageId"] != newest {
		t.Errorf("other client ReadSync = %v, want %s", rs, newest)
	}
}

func TestReadWatermarkAdvances(t *testing.T) {
	cases := []struct {
		name, cur, next string
		want            bool
	}{
		{"nothing stored", "", ulidOlder, true},
		{"newer advances", ulidOlder, ulidNewer, true},
		{"older rejected", ulidNewer, ulidOlder, false},
		{"same id is not an advance", ulidNewer, ulidNewer, false},
		{"unorderable stored value is replaced", "m9", ulidOlder, true},
		{"unorderable incoming id is accepted", ulidNewer, "m1", true},
	}
	for _, tc := range cases {
		if got := readWatermarkAdvances(tc.cur, tc.next); got != tc.want {
			t.Errorf("%s: readWatermarkAdvances(%q, %q) = %v, want %v", tc.name, tc.cur, tc.next, got, tc.want)
		}
	}
}
