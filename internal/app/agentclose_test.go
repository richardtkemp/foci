package app

import (
	"context"
	"slices"
	"testing"
	"time"

	"foci/internal/fap"
	"foci/internal/turnevent"
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

// markReadHub installs one session binding the hub's turns render to
// (bySession) and a device attached to it, plus a second device elsewhere.
func markReadHub(t *testing.T) (h *Hub, b *convBinding, onChat, elsewhere *wsClient, getRead func() string) {
	t.Helper()
	h, idx := sweepHub(t, nil)
	b = sweepBinding(h, "mine", time.Minute, time.Now())
	h.bySession[b.sessionKey] = b
	onChat, elsewhere = fakeClient(), fakeClient()
	b.clients = map[*wsClient]struct{}{onChat: {}}
	h.clients[onChat] = struct{}{}
	h.clients[elsewhere] = struct{}{}
	getRead = func() string {
		v, _ := idx.GetChatMetadata("clutch", "app", b.chatID, "last_read")
		return v
	}
	return h, b, onChat, elsewhere, getRead
}

// TestMarkSessionRead_IdleMarksLatestNow (#2592): with no turn in flight the
// read applies at once, through the newest message, stored and mirrored to
// every device, including the one showing the chat (a server-originated read
// has no sender to skip).
func TestMarkSessionRead_IdleMarksLatestNow(t *testing.T) {
	h, b, onChat, elsewhere, getRead := markReadHub(t)
	b.send(fap.ServerMessage{ConversationID: b.convID, MessageID: "01J00000000000000000000001", Role: "agent", Text: "one"})
	b.send(fap.ServerMessage{ConversationID: b.convID, MessageID: "01J00000000000000000000002", Role: "agent", Text: "two"})
	b.send(fap.Notification{ConversationID: b.convID, MessageID: "01J00000000000000000000009", Text: "skill", Ambient: true})
	drain(t, onChat)

	if n := h.markSessionRead("clutch", b.sessionKey); n != 1 {
		t.Fatalf("marked = %d, want 1", n)
	}
	const want = "01J00000000000000000000002" // the ambient line is not a message row
	if got := getRead(); got != want {
		t.Errorf("last_read = %q, want %q", got, want)
	}
	for name, c := range map[string]*wsClient{"on chat": onChat, "elsewhere": elsewhere} {
		if rs := lastReadSync(t, c); rs["conversationId"] != b.convID || rs["messageId"] != want {
			t.Errorf("%s ReadSync = %v, want %s/%s", name, rs, b.convID, want)
		}
	}
}

// TestMarkSessionRead_InTurnWaitsForFinalReply (#2592): called mid-turn (as
// close_session is), the read waits for the turn's end and then covers the
// final reply the sink delivered after the call.
func TestMarkSessionRead_InTurnWaitsForFinalReply(t *testing.T) {
	h, b, onChat, elsewhere, getRead := markReadHub(t)
	s := newAppSink(b)
	s.afterTurn = func() { h.finishTurnRead(b) }
	ctx := context.Background()

	// The user's message, minted earlier: a same-millisecond ULID may sort
	// either side of the reply's, and production has seconds between them.
	b.send(fap.ServerMessage{ConversationID: b.convID, MessageID: "01J00000000000000000000001", Role: "user", Text: "ot"})
	s.Emit(ctx, turnevent.TurnStart{})
	if n := h.markSessionRead("clutch", b.sessionKey); n != 1 {
		t.Fatalf("marked = %d, want 1", n)
	}
	if got := getRead(); got != "" {
		t.Fatalf("last_read = %q mid-turn, want nothing until the turn ends", got)
	}
	s.Emit(ctx, turnevent.TurnComplete{FinalText: "Nothing outstanding."})
	s.cleanup()

	var replyID string
	for _, f := range drain(t, onChat) {
		if f.t == fap.TypeMessage && f.d["role"] == "agent" {
			replyID, _ = f.d["messageId"].(string)
		}
		if f.t == fap.TypeReadSync {
			if got, _ := f.d["messageId"].(string); got != replyID || replyID == "" {
				t.Errorf("ReadSync names %q, want the final reply %q", got, replyID)
			}
		}
	}
	if replyID == "" {
		t.Fatal("the turn delivered no agent message")
	}
	if got := getRead(); got != replyID {
		t.Errorf("last_read = %q, want the final reply %q", got, replyID)
	}
	if rs := lastReadSync(t, elsewhere); rs["messageId"] != replyID {
		t.Errorf("other device ReadSync = %v, want %s", rs, replyID)
	}

	// Consumed: the next turn's end marks nothing.
	s2 := newAppSink(b)
	s2.afterTurn = func() { h.finishTurnRead(b) }
	s2.Emit(ctx, turnevent.TurnStart{})
	s2.Emit(ctx, turnevent.TurnComplete{FinalText: "later"})
	s2.cleanup()
	if rs := lastReadSync(t, elsewhere); rs != nil {
		t.Errorf("a later turn re-marked read: %v", rs)
	}
}

// TestMarkSessionRead_WithoutItTheReplyStaysUnread: requirement 2 of #2592 —
// a turn whose agent never asked leaves the read state alone.
func TestMarkSessionRead_WithoutItTheReplyStaysUnread(t *testing.T) {
	h, b, _, elsewhere, getRead := markReadHub(t)
	s := newAppSink(b)
	s.afterTurn = func() { h.finishTurnRead(b) }
	s.Emit(context.Background(), turnevent.TurnStart{})
	s.Emit(context.Background(), turnevent.TurnComplete{FinalText: "hi"})
	s.cleanup()
	if got := getRead(); got != "" {
		t.Errorf("last_read = %q, want unset", got)
	}
	if rs := lastReadSync(t, elsewhere); rs != nil {
		t.Errorf("ReadSync sent without mark_read: %v", rs)
	}
}

// TestMarkSessionRead_NoMatch: another agent's session, an unbound session and
// an empty key mark nothing.
func TestMarkSessionRead_NoMatch(t *testing.T) {
	h, b, _, elsewhere, _ := markReadHub(t)
	b.send(fap.ServerMessage{ConversationID: b.convID, MessageID: fap.NewULID(), Role: "agent", Text: "x"})
	for _, tc := range []struct{ agent, key string }{{"cam", b.sessionKey}, {"clutch", "clutch/c999"}, {"clutch", ""}} {
		if n := h.markSessionRead(tc.agent, tc.key); n != 0 {
			t.Errorf("markSessionRead(%q, %q) = %d, want 0", tc.agent, tc.key, n)
		}
	}
	if rs := lastReadSync(t, elsewhere); rs != nil {
		t.Errorf("ReadSync sent for a non-matching session: %v", rs)
	}
}

// TestMarkSessionRead_CoversRepliesAfterTheCall (#2592, Dick's case): the
// agent replies, calls close_session with mark_read, then writes more in the
// same turn ("Closed."). Every one of those messages must end up read: the
// watermark names the last message the turn sent, not the last before the
// call.
func TestMarkSessionRead_CoversRepliesAfterTheCall(t *testing.T) {
	h, b, onChat, elsewhere, getRead := markReadHub(t)
	s := newAppSink(b)
	s.afterTurn = func() { h.finishTurnRead(b) }
	ctx := context.Background()

	s.Emit(ctx, turnevent.TurnStart{})
	s.Emit(ctx, turnevent.TextBlock{Text: "Nothing outstanding.", Phase: turnevent.PhaseIntermediate})
	s.Emit(ctx, turnevent.ToolCall{Name: "close_session", Args: []byte(`{"mark_read":true}`)})
	if n := h.markSessionRead("clutch", b.sessionKey); n != 1 {
		t.Fatalf("marked = %d, want 1", n)
	}
	s.Emit(ctx, turnevent.ToolResult{})
	s.Emit(ctx, turnevent.TextBlock{Text: "Closed.", Phase: turnevent.PhaseIntermediate})
	if rs := lastReadSync(t, elsewhere); rs != nil {
		t.Fatalf("read applied mid-turn: %v", rs)
	}
	s.Emit(ctx, turnevent.TurnComplete{FinalText: "Nothing outstanding.Closed."})
	s.cleanup()

	var ids []string
	var readID string
	for _, f := range drain(t, onChat) {
		switch f.t {
		case fap.TypeMessage, fap.TypeTextEnd:
			if id, _ := f.d["messageId"].(string); id != "" {
				ids = append(ids, id)
			}
		case fap.TypeReadSync:
			readID, _ = f.d["messageId"].(string)
		}
	}
	if len(ids) < 2 {
		t.Fatalf("turn sent %d message rows, want the reply before and after the call", len(ids))
	}
	last := ids[len(ids)-1]
	if readID != last {
		t.Errorf("ReadSync names %q, want the last message of the turn %q (all ids %v)", readID, last, ids)
	}
	if got := getRead(); got != last {
		t.Errorf("last_read = %q, want %q", got, last)
	}
	if rs := lastReadSync(t, elsewhere); rs["messageId"] != last {
		t.Errorf("other device ReadSync = %v, want %s", rs, last)
	}
}
