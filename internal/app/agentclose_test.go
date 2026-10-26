package app

import (
	"slices"
	"testing"
	"time"
)

// TestCloseSessionConversations_RemovesOnlyThisSessionAndBroadcasts pins the
// agent close (#2573): the chat bound to the calling session leaves the stored
// open-set, every other chat (another session of the same agent, the same
// session key under another agent) stays in its original order, and every
// connected socket receives exactly one conversation.openSync of the survivors.
func TestCloseSessionConversations_RemovesOnlyThisSessionAndBroadcasts(t *testing.T) {
	h, _ := sweepHub(t, nil)
	now := time.Now()
	mine := sweepBinding(h, "mine", time.Minute, now)
	sweepBinding(h, "otherSession", time.Minute, now)
	foreign := sweepBinding(h, "foreignAgent", time.Minute, now)
	foreign.agentID = "cam"
	foreign.sessionKey = mine.sessionKey
	c1, c2 := fakeClient(), fakeClient()
	h.clients[c1] = struct{}{}
	h.clients[c2] = struct{}{}
	h.storeOpenChats([]string{"otherSession", "mine", "foreignAgent", "unbound"})

	if n := h.closeSessionConversations("clutch", mine.sessionKey); n != 1 {
		t.Fatalf("closed = %d, want 1", n)
	}

	want := []string{"otherSession", "foreignAgent", "unbound"}
	if got := h.loadOpenChats(); !slices.Equal(got, want) {
		t.Errorf("stored open-set = %v, want %v", got, want)
	}
	for _, c := range []*wsClient{c1, c2} {
		if ids, n := lastOpenSync(t, c); n != 1 || !slices.Equal(ids, want) {
			t.Errorf("client OpenSync = %v (frames=%d), want exactly one %v", ids, n, want)
		}
	}
}

// TestCloseSessionConversations_NotOpenIsANoop: a session whose chat is not in
// the open-set (already closed) closes nothing, stores nothing and sends
// nothing; an empty session key never matches an unbound binding.
func TestCloseSessionConversations_NotOpenIsANoop(t *testing.T) {
	h, _ := sweepHub(t, nil)
	now := time.Now()
	mine := sweepBinding(h, "mine", time.Minute, now)
	sweepBinding(h, "other", time.Minute, now)
	blank := sweepBinding(h, "blank", time.Minute, now)
	blank.sessionKey = ""
	c := fakeClient()
	h.clients[c] = struct{}{}
	h.storeOpenChats([]string{"other", "blank"})

	if n := h.closeSessionConversations("clutch", mine.sessionKey); n != 0 {
		t.Errorf("closed = %d, want 0 for a chat that is not open", n)
	}
	if n := h.closeSessionConversations("clutch", ""); n != 0 {
		t.Errorf("closed = %d, want 0 for an empty session key", n)
	}
	if got := h.loadOpenChats(); !slices.Equal(got, []string{"other", "blank"}) {
		t.Errorf("stored open-set = %v, want it untouched", got)
	}
	if _, n := lastOpenSync(t, c); n != 0 {
		t.Error("a close that removes nothing must not broadcast")
	}
}

// TestCloseSessionConversations_LastChatStoresEmptyList: closing the only open
// chat stores and sends [] (not null), so clients close the tab rather than
// read "nothing stored".
func TestCloseSessionConversations_LastChatStoresEmptyList(t *testing.T) {
	h, idx := sweepHub(t, nil)
	mine := sweepBinding(h, "mine", time.Minute, time.Now())
	c := fakeClient()
	h.clients[c] = struct{}{}
	h.storeOpenChats([]string{"mine"})

	if n := h.closeSessionConversations("clutch", mine.sessionKey); n != 1 {
		t.Fatalf("closed = %d, want 1", n)
	}
	if raw, _ := idx.GetSystemState(systemStateOpenChats); raw != "[]" {
		t.Errorf("stored open_chats = %q, want []", raw)
	}
	if ids, n := lastOpenSync(t, c); n != 1 || len(ids) != 0 {
		t.Errorf("client OpenSync = %v (frames=%d), want exactly one empty set", ids, n)
	}
}
