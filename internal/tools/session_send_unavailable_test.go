package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"foci/internal/session"
)

// sendWith runs send_to_session against a notifier with the given check, and
// returns the tool's text, its error and the origin the delivery carried.
func sendWith(t *testing.T, check TargetCheckFunc, callerType session.SessionType, replyTo string) (string, error, *AgentOrigin) {
	t.Helper()
	var got *AgentOrigin
	n := NewAsyncNotifier(func(_, _, _, _ string, origin *AgentOrigin) { got = origin }).WithTargetCheck(check)
	tool := NewSendToSessionTool(nil, n, func(_, _ string, origin *AgentOrigin) { got = origin }, nil, nil,
		func(string) session.SessionType { return callerType })
	params, _ := json.Marshal(map[string]string{"session_key": "scout/c42", "message": "hello", "reply_to": replyTo})
	res, err := tool.Execute(WithSessionKey(context.Background(), "clutch/c1"), params)
	return res.Text, err, got
}

// TestSendToSession_HeldTargetNamedInResult: #2892, the caller is told at
// once and the message is still sent, whichever way the reply goes.
func TestSendToSession_HeldTargetNamedInResult(t *testing.T) {
	held := func(string) (string, error) { return "usage hold (lifts Mon 2 Jan 15:04)", nil }
	for _, tc := range []struct {
		name, replyTo, where string
		caller               session.SessionType
		notify               bool
	}{
		{"reply to caller", "caller", "goes to you", session.SessionTypeUnknown, true},
		{"reply to session", "session", "goes to its own chat", session.SessionTypeUnknown, true},
		{"oneshot caller", "caller", "goes to its own chat", session.SessionTypeBackgroundTask, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, err, origin := sendWith(t, held, tc.caller, tc.replyTo)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"Message sent", "usage hold (lifts", "will be delivered when it is back", tc.where} {
				if !strings.Contains(text, want) {
					t.Errorf("result %q lacks %q", text, want)
				}
			}
			if origin == nil {
				t.Fatal("the message was not sent")
			}
			if !origin.CallerTold || origin.NotifyCaller != tc.notify {
				t.Errorf("origin CallerTold=%v NotifyCaller=%v, want true, %v", origin.CallerTold, origin.NotifyCaller, tc.notify)
			}
		})
	}
}

// TestSendToSession_AvailableTargetUnchanged: no note, nothing marked told.
func TestSendToSession_AvailableTargetUnchanged(t *testing.T) {
	text, err, origin := sendWith(t, func(string) (string, error) { return "", nil }, session.SessionTypeUnknown, "caller")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "⚠️") || origin == nil || origin.CallerTold || !origin.NotifyCaller {
		t.Errorf("result %q, origin %+v: want a plain send to a caller that takes notices", text, origin)
	}
}

// TestSendToSession_MissingTargetRefused: a target with no agent is an error
// and nothing is sent. The tool used to report "Message sent".
func TestSendToSession_MissingTargetRefused(t *testing.T) {
	_, err, origin := sendWith(t, func(string) (string, error) { return "", errors.New("no agent") }, session.SessionTypeUnknown, "caller")
	if err == nil || origin != nil {
		t.Errorf("err=%v origin=%+v, want a refusal and no send", err, origin)
	}
}
