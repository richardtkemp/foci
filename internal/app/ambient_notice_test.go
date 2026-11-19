package app

import (
	"testing"

	"foci/internal/fap"
	"foci/internal/platform"
)

// ambientBinding is an offline binding that records each wake push's preview
// and reads the "Notify for system messages" setting from *on.
func ambientBinding(pushes *[]string, on *bool) *convBinding {
	return &convBinding{
		convID:        "c1",
		seen:          make(map[string]struct{}),
		notifyOffline: func(p pushPayload) { *pushes = append(*pushes, p.Preview) },
		notifySystem:  func() bool { return *on },
	}
}

// TestSend_SystemMessagesAreAmbientByDefault (#3180 req 1-3): with the setting
// off, a system message (a Notification, or a system-role ServerMessage) is not
// a visible frame: no push, no roster preview, no activity time. A read through
// the latest message still covers it. An Urgent one is news whatever the
// setting.
func TestSend_SystemMessagesAreAmbientByDefault(t *testing.T) {
	var pushes []string
	on := false
	b := ambientBinding(&pushes, &on)

	b.send(fap.ServerMessage{ConversationID: "c1", MessageID: "m1", Role: "agent", Text: "agent reply"})
	actMs := b.lastActMs
	b.send(fap.Notification{ConversationID: "c1", MessageID: "n1", Text: "gateway restarted"})
	b.send(fap.ServerMessage{ConversationID: "c1", MessageID: "s1", Role: "system", Text: "deferred message dropped"})

	if len(pushes) != 1 || pushes[0] != "agent reply" {
		t.Fatalf("pushes = %q, want only the agent reply's", pushes)
	}
	if b.lastPreview != "agent reply" || b.lastActMs != actMs {
		t.Errorf("roster row moved for an ambient notice: preview %q, act %d -> %d", b.lastPreview, actMs, b.lastActMs)
	}
	if b.lastMsgID != "s1" {
		t.Errorf("lastMsgID = %q, want s1 (a read through latest must cover the notice's row)", b.lastMsgID)
	}

	b.send(fap.Notification{ConversationID: "c1", MessageID: "n2", Text: "sign-in ends soon", Urgent: true})
	b.send(fap.ServerMessage{ConversationID: "c1", MessageID: "s2", Role: "system", Text: "sign-in link", Urgent: true})
	if len(pushes) != 3 || pushes[1] != "sign-in ends soon" || pushes[2] != "sign-in link" {
		t.Fatalf("pushes = %q, want the two urgent notices pushed", pushes)
	}
	if b.lastPreview != "sign-in link" {
		t.Errorf("lastPreview = %q, want the urgent notice's", b.lastPreview)
	}
}

// TestSend_SystemMessagesNotifyWhenSettingOn (#3180 req 4): with "Notify for
// system messages" on, an ambient notice pushes and moves the chat like any
// message. A trace (a shell chit, a "Loaded skill" line) stays quiet.
func TestSend_SystemMessagesNotifyWhenSettingOn(t *testing.T) {
	var pushes []string
	on := true
	b := ambientBinding(&pushes, &on)

	b.send(fap.Notification{ConversationID: "c1", MessageID: "n1", Text: "gateway restarted"})
	b.send(fap.ServerMessage{ConversationID: "c1", MessageID: "s1", Role: "system", Text: "deferred message dropped"})
	b.send(fap.Notification{ConversationID: "c1", MessageID: "k1", Text: "Loaded skill x", Ambient: true})

	if len(pushes) != 2 || pushes[0] != "gateway restarted" || pushes[1] != "deferred message dropped" {
		t.Fatalf("pushes = %q, want both notices and not the trace", pushes)
	}
	if b.lastPreview != "deferred message dropped" {
		t.Errorf("lastPreview = %q, want the last notice's", b.lastPreview)
	}

	on = false
	b.send(fap.Notification{ConversationID: "c1", MessageID: "n2", Text: "compacted"})
	if len(pushes) != 2 {
		t.Errorf("pushes = %q: the setting turned off must stop the next notice's push", pushes)
	}
}

// TestNotifySystemMessages_FollowsTheSyncedSetting: the hub reads the toggle
// from the synced settings bag (off when unset) and a setting.put changes it
// at once.
func TestNotifySystemMessages_FollowsTheSyncedSetting(t *testing.T) {
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: newTestIndex(t)}
	if h.notifySystemMessages() {
		t.Fatal("unset setting must read as off")
	}
	h.handleSettingPut(fap.SettingPut{Key: settingNotifySystemMessages, Value: "true"})
	if !h.notifySystemMessages() {
		t.Fatal("setting.put true must turn it on")
	}
	h.handleSettingPut(fap.SettingPut{Key: "theme", Value: "dark"})
	if !h.notifySystemMessages() {
		t.Fatal("another key's put must keep it on")
	}
	h.handleSettingPut(fap.SettingPut{Key: settingNotifySystemMessages, Value: "false"})
	if h.notifySystemMessages() {
		t.Fatal("setting.put false must turn it off")
	}

	// A fresh hub over the same bag (a restart) reads the stored value.
	h.handleSettingPut(fap.SettingPut{Key: settingNotifySystemMessages, Value: "true"})
	h2 := newTestHub()
	h2.deps = h.deps
	if !h2.notifySystemMessages() {
		t.Fatal("a restarted hub must read the stored setting")
	}
}

// TestUrgentSends_MarkTheWireFrame: the two urgent senders (the sign-in link
// and the sign-in expiry notice) put urgent=true on the wire; the plain ones
// leave it off.
func TestUrgentSends_MarkTheWireFrame(t *testing.T) {
	h := newTestHub()
	c := fakeClient()
	b := withIdent(&convBinding{convID: "c1", clients: map[*wsClient]struct{}{c: {}}}, "ag", "ag/c7")
	h.bySession[b.key()] = b
	conn := &appConn{hub: h, agentID: "ag", bound: b.key()}

	if _, err := conn.SendUrgentEditableInjected(b.key(), "link"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.SendEditableInjected(b.key(), "plain link"); err != nil {
		t.Fatal(err)
	}
	conn.SendUrgentNotification("expiry")
	conn.SendNotification("plain notice")

	got := drain(t, c)
	if len(got) != 4 {
		t.Fatalf("frames = %v, want 4", types(got))
	}
	for i, want := range []bool{true, false, true, false} {
		urgent, present := got[i].d["urgent"]
		if want && urgent != true {
			t.Errorf("frame %d (%s %v) urgent = %v, want true", i, got[i].t, got[i].d["text"], urgent)
		}
		if !want && present {
			t.Errorf("frame %d (%s %v) carries urgent = %v, want it omitted", i, got[i].t, got[i].d["text"], urgent)
		}
	}
}
