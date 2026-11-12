package app

import (
	"testing"

	"foci/internal/fap"
)

// sawTextEnd reports whether c received a TextEnd for convID.
func sawTextEnd(t *testing.T, c *wsClient, convID string) bool {
	t.Helper()
	for _, d := range drain(t, c) {
		if d.t == fap.TypeTextEnd && d.d["conversationId"] == convID {
			return true
		}
	}
	return false
}

// TestEnsureBindingNil_ExistingKeepsAttachedDevices (#3052): a server-side
// lookup with no calling socket (ensureBinding(nil, …)) on a conversation that
// already exists must not detach the devices attached to it. Before the fix it
// called attach(nil), which empties the live set, so the next reply reached no
// connected device until it re-sent or reconnected.
func TestEnsureBindingNil_ExistingKeepsAttachedDevices(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag")
	phone, tablet := fakeClientFor(h), fakeClientFor(h)
	h.clients[phone] = struct{}{}
	h.clients[tablet] = struct{}{}

	b := h.ensureBinding(phone, "ag", "conv-a")
	h.ensureBinding(tablet, "ag", "conv-a")
	drain(t, phone)
	drain(t, tablet)

	if got := h.ensureBinding(nil, "ag", "conv-a"); got != b {
		t.Fatalf("ensureBinding(nil) on an existing conversation returned %p, want the existing binding %p", got, b)
	}

	final := "reply"
	b.send(fap.TextEnd{ConversationID: "conv-a", TurnID: "t1", MessageID: "m1", FinalText: &final})
	if !sawTextEnd(t, phone, "conv-a") {
		t.Error("phone was detached by ensureBinding(nil): it did not receive the reply live")
	}
	if !sawTextEnd(t, tablet, "conv-a") {
		t.Error("tablet was detached by ensureBinding(nil): it did not receive the reply live")
	}
}

// TestBindingForPrompt_DurableFallbackKeepsAttachedDevices (#3052): the
// runtime route. After a restart the prompt registry is empty, so resolving an
// ask asked before the restart goes through bindingForPrompt's durable
// fallback, which calls ensureBinding(nil, …) on the (restored, now attached)
// conversation. The device must stay attached.
func TestBindingForPrompt_DurableFallbackKeepsAttachedDevices(t *testing.T) {
	h := newTestHub()
	h.frames = tempFrameStore(t)
	registerBareAgent(h, "ag")
	c := fakeClientFor(h)
	h.clients[c] = struct{}{}

	b := h.ensureBinding(c, "ag", "conv-b")
	h.registerPrompt("p1", b)
	// A restart: the in-memory registry is gone, the durable prompt row stays.
	h.mu.Lock()
	delete(h.prompts, "p1")
	h.mu.Unlock()
	drain(t, c)

	if got := h.bindingForPrompt("p1"); got != b {
		t.Fatalf("bindingForPrompt durable fallback = %p, want %p", got, b)
	}

	final := "after the ask"
	b.send(fap.TextEnd{ConversationID: "conv-b", TurnID: "t2", MessageID: "m2", FinalText: &final})
	if !sawTextEnd(t, c, "conv-b") {
		t.Fatal("resolving a pre-restart ask detached the device: the next reply did not reach it live")
	}
}
