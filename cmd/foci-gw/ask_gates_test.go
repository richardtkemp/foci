package main

import (
	"strings"
	"testing"

	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/question"
)

// batchConn is an app-like connection that takes batched forms.
type batchConn struct {
	buttonStubConn
	forms int
}

func (c *batchConn) SendInteractiveBatch(string, []platform.BatchQuestion, func([]string)) (bool, error) {
	c.forms++
	return true, nil
}

// TestAskPresent_SpawnOwnerChatClosed (#2764, audit A8; the gate #2684 gave backend
// prompts): a spawn's question whose owner's chat is closed is refused with the
// unattended message on both presenters, and nothing is sent, not even to the
// rest of the chain. Any other session's ask is not gated, nor a spawn's whose
// owner's chat is open.
func TestAskPresent_SpawnOwnerChatClosed(t *testing.T) {
	const spawn, owner, root = "main/c1/b2", "main/c1/b1", "main/c1"
	chats := askChats(func(sk string) ([]string, bool) {
		if sk == spawn {
			return []string{owner, root}, true
		}
		return nil, false
	})
	for _, tc := range []struct {
		name      string
		session   string
		closedKey string
		refused   bool
	}{
		{"spawn, owner closed", spawn, owner, true},
		{"spawn, owner open", spawn, "main/other", false},
		{"not a spawn, own chat closed", root, root, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &batchConn{buttonStubConn: buttonStubConn{stubConn{sessionKey: tc.session}}}
			mgr := oneConnMgr{conn: conn}
			closed := func(sk string) bool { return sk == tc.closedKey }

			msgID := "ask-2764-" + strings.ReplaceAll(tc.name, " ", "-")
			t.Cleanup(func() { platform.CancelInteractiveMessage(msgID, "done") })
			_, err := newAskPresentFn("main", mgr, chats, closed)(tc.session, msgID, "Q?", "Q", askChoices(), func(string) {})
			batched := newAskPresentBatchFn("main", mgr, chats, closed)(tc.session, msgID+"-b", []question.Question{{Question: "Q?"}}, func([]string) {})

			if tc.refused {
				if err == nil || err.Error() != delegator.UnattendedQuestionMessage {
					t.Errorf("present err = %v, want the unattended message", err)
				}
				if batched || conn.forms != 0 {
					t.Errorf("batched = %v, forms = %d, want no form", batched, conn.forms)
				}
				return
			}
			if err != nil {
				t.Errorf("present err = %v, want it shown", err)
			}
			if !batched {
				t.Error("batch presenter refused an ask that is not gated")
			}
		})
	}
}

// TestAskPresent_NoChatCanShowIt (#2764, audit A8; the gate #2608 gave backend
// prompts): when every chat fails, the presenter reports it, naming each failure,
// so the ask layer does not keep an ask that is on no screen.
func TestAskPresent_NoChatCanShowIt(t *testing.T) {
	chats := askChats(func(string) ([]string, bool) { return []string{"main/c1/b1", "main/c1"}, true })
	hub := &fakeAppHub{bound: map[string]bool{}}
	id, err := newAskPresentFn("main", hub, chats, nil)("main/c1/b2", "ask-2764-none", "Q?", "Q", askChoices(), func(string) {})
	if err == nil || id != "" {
		t.Fatalf("id = %q, err = %v, want a failure", id, err)
	}
	for _, want := range []string{"main/c1/b1", "main/c1:", "no conversation for session", "nobody saw it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q lacks %q", err, want)
		}
	}
}

// TestAskPresent_ChatsAfterAFailureStillTried: a failed first chat is not yet a
// failure of the ask (#2630).
func TestAskPresent_ChatsAfterAFailureStillTried(t *testing.T) {
	chats := askChats(func(string) ([]string, bool) { return []string{"main/c1/b1", "main/c1"}, true })
	hub := &fakeAppHub{bound: map[string]bool{"main/c1": true}}
	t.Cleanup(func() { platform.CancelInteractiveMessage("ask-2764-walk", "done") })
	_, err := newAskPresentFn("main", hub, chats, nil)("main/c1/b2", "ask-2764-walk", "Q?", "Q", askChoices(), func(string) {})
	if err != nil || len(hub.shownIn) != 1 || hub.shownIn[0] != "main/c1" {
		t.Fatalf("err = %v, shown in %v, want main/c1", err, hub.shownIn)
	}
}
