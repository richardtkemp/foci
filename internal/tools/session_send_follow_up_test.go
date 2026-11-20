package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/session"
)

// TestSendToSessionChatFollowUp is #3159: a reflection-pass branch may send
// its parent chat ONE follow-up (an unkept promise, a dropped point), and the
// chat's reply goes to its own user (reply_to=session). Every other send from
// a barred caller is still refused (#1409): to another session, a second
// follow-up, a branch the follow-up fn does not name (consolidation,
// keepalive), and any barred caller when the option is not wired.
func TestSendToSessionChatFollowUp(t *testing.T) {
	t.Parallel()
	const (
		chat        = "clutch/c1"
		reflection  = "clutch/c1/b1"
		consolidate = "clutch/c1/b2"
		keepalive   = "clutch/c1/b3"
	)
	types := map[string]session.SessionType{
		chat:        session.SessionTypeChat,
		reflection:  session.SessionTypeReflection,
		consolidate: session.SessionTypeReflection,
		keepalive:   session.SessionTypeKeepalive,
	}
	callerType := func(sk string) session.SessionType { return types[sk] }
	followUpParent := func(caller string) (string, bool) {
		if caller == reflection {
			return chat, true
		}
		return "", false
	}

	type send struct{ target, msg string }
	newTool := func(opts ...SendToSessionOption) (*Tool, chan send, chan string) {
		sessionSends := make(chan send, 4)
		callerRelays := make(chan string, 4)
		notifier := NewAsyncNotifier(func(sk, _, _, _ string, _ *AgentOrigin) { callerRelays <- sk })
		sessionNotify := func(sk, msg string, _ *AgentOrigin) { sessionSends <- send{sk, msg} }
		return NewSendToSessionTool(&mockSessionAppender{}, notifier, sessionNotify, nil, nil, callerType, opts...), sessionSends, callerRelays
	}
	call := func(tool *Tool, caller string, params map[string]string) error {
		b, _ := json.Marshal(params)
		_, err := tool.Execute(WithSessionKey(context.Background(), caller), b)
		return err
	}
	const bar = "send_to_session is disabled in reflection/keepalive branches"

	t.Run("one_follow_up_to_parent", func(t *testing.T) {
		tool, sessionSends, callerRelays := newTool(WithChatFollowUp(followUpParent))
		// reply_to left at its default (caller): the follow-up still goes
		// reply_to=session, so the chat answers its own user.
		if err := call(tool, reflection, map[string]string{"session_key": chat, "message": "You said you would run the tests."}); err != nil {
			t.Fatalf("follow-up refused: %v", err)
		}
		select {
		case got := <-sessionSends:
			if got.target != chat || !strings.Contains(got.msg, "You said you would run the tests.") {
				t.Errorf("follow-up delivered as %+v, want to %s with the message", got, chat)
			}
		default:
			t.Fatal("follow-up was not delivered reply_to=session")
		}
		if len(callerRelays) != 0 {
			t.Errorf("follow-up also went reply_to=caller to %q", <-callerRelays)
		}

		// A second message from the same pass is refused.
		err := call(tool, reflection, map[string]string{"session_key": chat, "message": "and another thing"})
		if err == nil || !strings.Contains(err.Error(), "already sent its one chat follow-up") {
			t.Errorf("second follow-up error = %v, want the once-only refusal", err)
		}
		if len(sessionSends) != 0 {
			t.Error("second follow-up was delivered")
		}
	})

	t.Run("other_target_refused", func(t *testing.T) {
		tool, sessionSends, callerRelays := newTool(WithChatFollowUp(followUpParent))
		err := call(tool, reflection, map[string]string{"session_key": "scout/c0", "message": "status update", "reply_to": "session"})
		if err == nil || !strings.Contains(err.Error(), bar) {
			t.Errorf("send to another session: error = %v, want the bar", err)
		}
		if len(sessionSends)+len(callerRelays) != 0 {
			t.Error("send to another session was delivered")
		}
		// The refused attempt did not use up the follow-up.
		if err := call(tool, reflection, map[string]string{"session_key": chat, "message": "promise"}); err != nil {
			t.Errorf("follow-up after a refused send: %v", err)
		}
	})

	for _, tc := range []struct {
		name   string
		caller string
		opts   []SendToSessionOption
	}{
		{"consolidation_branch", consolidate, []SendToSessionOption{WithChatFollowUp(followUpParent)}},
		{"keepalive_branch", keepalive, []SendToSessionOption{WithChatFollowUp(followUpParent)}},
		{"option_not_wired", reflection, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, sessionSends, callerRelays := newTool(tc.opts...)
			err := call(tool, tc.caller, map[string]string{"session_key": chat, "message": "promise", "reply_to": "session"})
			if err == nil || !strings.Contains(err.Error(), bar) {
				t.Errorf("error = %v, want the bar", err)
			}
			if len(sessionSends)+len(callerRelays) != 0 {
				t.Error("message was delivered")
			}
		})
	}
}
