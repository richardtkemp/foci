package app

import (
	"testing"
	"time"

	"foci/internal/fap"
	"foci/internal/platform"
)

// A burst of reads on one chat sends ONE read wake per offline device, after
// readWait, carrying the newest watermark (#2182). Typed "read", normal
// priority, collapsed per chat, and without the message wake's preview fields.
func TestPusher_ReadWakeBatchesToNewestWatermark(t *testing.T) {
	tk := newPushTokens()
	tk.set("phone", "tok-phone")
	tk.setReadWake("phone", true)
	p, rec := newRecordingPusher(t, tk)
	p.readWait = 50 * time.Millisecond

	for _, m := range []string{"m1", "m2", "m3"} {
		p.notifyRead("c1", m, nil)
	}
	rec.waitFor(t, "tok-phone", 1)
	time.Sleep(150 * time.Millisecond)

	got := rec.pushes()
	if len(got) != 1 {
		t.Fatalf("pushes = %d, want 1 (a burst of reads is one wake)", len(got))
	}
	d := got[0].Data
	if d["type"] != pushTypeRead || d["conversationId"] != "c1" || d["messageId"] != "m3" {
		t.Errorf("read wake data = %v, want type=read c1/m3", d)
	}
	if _, ok := d["preview"]; ok {
		t.Errorf("read wake must carry no preview: %v", d)
	}
	if got[0].Android["priority"] != "normal" || got[0].Android["collapse_key"] != "read:c1" {
		t.Errorf("read wake android = %v, want normal priority, collapse_key read:c1", got[0].Android)
	}

	// The batch closed with the send: a later read wakes again.
	p.notifyRead("c1", "m4", nil)
	rec.waitFor(t, "tok-phone", 2)
}

// The connected set is taken when the wake fires, not when the read arrives: a
// device that connected in the meantime heard the ReadSync over its socket.
func TestPusher_ReadWakeSkipsDevicesConnectedAtSend(t *testing.T) {
	tk := newPushTokens()
	tk.set("phone", "tok-phone")
	tk.set("tablet", "tok-tablet")
	tk.setReadWake("phone", true)
	tk.setReadWake("tablet", true)
	p, rec := newRecordingPusher(t, tk)
	p.readWait = 50 * time.Millisecond

	phoneUp := make(chan struct{})
	p.notifyRead("c1", "m1", func() map[string]bool {
		<-phoneUp
		return map[string]bool{"phone": true}
	})
	close(phoneUp) // the phone connected after the read, before the wake fired

	rec.waitFor(t, "tok-tablet", 1)
	time.Sleep(50 * time.Millisecond)
	if n := rec.count("tok-phone"); n != 0 {
		t.Errorf("phone pushes = %d, want 0 (connected when the wake fired)", n)
	}
}

// A message wake is typed "message", so a client can tell it from a read wake.
func TestPusher_MessageWakeIsTyped(t *testing.T) {
	tk := newPushTokens()
	tk.set("phone", "tok-phone")
	p, rec := newRecordingPusher(t, tk)
	p.notify(pushPayload{ConvID: "c1", Preview: "hi"}, nil)
	rec.waitFor(t, "tok-phone", 1)
	got := rec.pushes()[0]
	if got.Data["type"] != pushTypeMessage || got.Data["preview"] != "hi" {
		t.Errorf("message wake data = %v, want type=message preview=hi", got.Data)
	}
	if got.Android["priority"] != "high" {
		t.Errorf("message wake priority = %v, want high", got.Android["priority"])
	}
}

// handleRead queues a read wake when the watermark advances, and none for a
// stale read that does not.
func TestHandleRead_SendsReadWakeOnlyOnAdvance(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42, sessionKey: "clutch/c42"}
	h.tokens.set("phone", "tok-phone")
	h.tokens.setReadWake("phone", true)
	p, rec := newRecordingPusher(t, h.tokens)
	p.readWait = 20 * time.Millisecond
	h.pusher = p
	_ = idx.SetChatMetadata("clutch", "app", 42, "last_read", ulidNewer)

	mac := fakeClient()
	h.clients[mac] = struct{}{}

	h.handleRead(mac, fap.Read{ConversationID: "c1", MessageID: ulidOlder})
	time.Sleep(100 * time.Millisecond)
	if n := rec.count("tok-phone"); n != 0 {
		t.Fatalf("stale read sent %d read wakes, want 0", n)
	}

	newest := "01K6B0002000000000000000AA"
	h.handleRead(mac, fap.Read{ConversationID: "c1", MessageID: newest})
	rec.waitFor(t, "tok-phone", 1)
	if d := rec.pushes()[0].Data; d["type"] != pushTypeRead || d["messageId"] != newest {
		t.Errorf("read wake data = %v, want type=read messageId=%s", d, newest)
	}
}

// A nil pusher (push unconfigured) makes notifyRead a no-op: handleRead calls it
// unconditionally.
func TestPusher_NilNotifyReadIsNoop(t *testing.T) {
	var p *fcmPusher
	p.notifyRead("c1", "m1", nil)
}

// Read wakes go only to devices whose latest hello advertised featureReadWake:
// an older client would show one as "New message". A later hello without the
// feature (a downgraded build) withdraws it.
func TestReadWake_OnlyToDevicesAdvertisingTheFeature(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag")
	p, rec := newRecordingPusher(t, h.tokens)
	p.readWait = 20 * time.Millisecond
	h.pusher = p

	h.dispatchInbound(fakeClientFor(h), []byte(`{"t":"hello","id":"x","d":{"client":{"deviceId":"new"},"pushToken":"tok-new","features":["readWake"]}}`))
	h.dispatchInbound(fakeClientFor(h), []byte(`{"t":"hello","id":"y","d":{"client":{"deviceId":"old"},"pushToken":"tok-old"}}`))

	p.notifyRead("c1", "m1", nil)
	rec.waitFor(t, "tok-new", 1)
	time.Sleep(50 * time.Millisecond)
	if n := rec.count("tok-old"); n != 0 {
		t.Errorf("device without readWake got %d read wakes, want 0", n)
	}

	h.dispatchInbound(fakeClientFor(h), []byte(`{"t":"hello","id":"z","d":{"client":{"deviceId":"new"},"pushToken":"tok-new"}}`))
	p.notifyRead("c1", "m2", nil)
	time.Sleep(100 * time.Millisecond)
	if n := rec.count("tok-new"); n != 1 {
		t.Errorf("after a hello without readWake, device got %d read wakes total, want 1", n)
	}
}

// A re-asserted watermark (#1515) is mirrored over the socket but sends no read
// wake: the wake went out when that watermark first advanced.
func TestHandleRead_ReassertedWatermarkSendsNoWake(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.convs["c1"] = &convBinding{convID: "c1", agentID: "clutch", chatID: 42, sessionKey: "clutch/c42"}
	h.tokens.set("phone", "tok-phone")
	h.tokens.setReadWake("phone", true)
	p, rec := newRecordingPusher(t, h.tokens)
	p.readWait = 20 * time.Millisecond
	h.pusher = p
	_ = idx.SetChatMetadata("clutch", "app", 42, "last_read", ulidNewer)

	h.handleRead(fakeClient(), fap.Read{ConversationID: "c1", MessageID: ulidNewer})
	time.Sleep(100 * time.Millisecond)
	if n := rec.count("tok-phone"); n != 0 {
		t.Fatalf("re-asserted read sent %d read wakes, want 0", n)
	}
}
