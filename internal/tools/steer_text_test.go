package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/session"
)

// #3114 review: a message within one agent that folds into a running turn
// must not tell the model to reply [[NO_RESPONSE]] (that would silence the
// running turn's answer to the human); the same message run as its own turn
// keeps the instruction.

const noResponse = "[[NO_RESPONSE]]"

// TestSpawnerMessage_SteerTextHasNoNoResponse: send_to_spawner's delivered
// text keeps the instruction; origin.SteerText (the fold) has none, and still
// carries the message and how to answer it.
func TestSpawnerMessage_SteerTextHasNoNoResponse(t *testing.T) {
	t.Parallel()
	info := RunningSpawn{ID: "spawn-abc"}
	standalone := formatSpawnerMessage(info, false, "halfway there")
	steer := formatSpawnerMessageSteer(info, false, "halfway there")
	if !strings.Contains(standalone, noResponse) {
		t.Errorf("standalone spawn message lost its %s instruction:\n%s", noResponse, standalone)
	}
	if strings.Contains(steer, noResponse) {
		t.Errorf("steered spawn message tells the model to reply %s:\n%s", noResponse, steer)
	}
	for _, want := range []string{"halfway there", "spawn_send spawn-abc", "NOT from your human"} {
		if !strings.Contains(steer, want) {
			t.Errorf("steered spawn message lacks %q:\n%s", want, steer)
		}
	}
}

// TestSendToSpawner_OriginCarriesSteerText: the delivery from the tool carries
// both texts — the message (standalone) with the instruction, the origin's
// SteerText without.
func TestSendToSpawner_OriginCarriesSteerText(t *testing.T) {
	t.Parallel()
	notifier, got := recordingNotifier()
	var s *Spawner
	agent := &messagingAgent{tool: func() *Tool { return s.SendToSpawnerTool() }, text: "found it"}
	cb := &characterBranches{}
	s = NewSpawner(SpawnDeps{AgentID: "steertxt", MaxInherit: 1, Notifier: notifier, Tracker: NewSpawnTracker(), CharacterBranch: cb.create},
		func() SpawnAgent { return agent })
	ctx := WithSessionKey(context.Background(), "steertxt/imain")
	if _, err := s.SpawnTool().Execute(ctx, json.RawMessage(`{"name":"steer1","prompt":"job","background":true}`)); err != nil {
		t.Fatal(err)
	}
	msg := recv(t, got, "the spawn's message")
	if !strings.Contains(msg.msg, noResponse) {
		t.Errorf("delivered message lost its %s instruction:\n%s", noResponse, msg.msg)
	}
	if msg.origin == nil || msg.origin.SteerText == "" {
		t.Fatalf("origin = %+v, want a SteerText", msg.origin)
	}
	if strings.Contains(msg.origin.SteerText, noResponse) || !strings.Contains(msg.origin.SteerText, "found it") {
		t.Errorf("SteerText = %q, want the message without %s", msg.origin.SteerText, noResponse)
	}
	recv(t, got, "the [SPAWN RESULT]")
}

// TestSendToSession_SteerText: reply_to=session (and the oneshot downgrade)
// carries a SteerText without the instruction, the delivered message keeps
// it; reply_to=caller has none (its target turn never folds).
func TestSendToSession_SteerText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, replyTo string
		caller        session.SessionType
		wantSteer     bool
	}{
		{"reply to session", "session", session.SessionTypeUnknown, true},
		{"oneshot caller", "caller", session.SessionTypeBackgroundTask, true},
		{"reply to caller", "caller", session.SessionTypeUnknown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var msg string
			var origin *AgentOrigin
			rec := func(m string, o *AgentOrigin) { msg, origin = m, o }
			n := NewAsyncNotifier(func(_, m, _, _ string, o *AgentOrigin) { rec(m, o) })
			tool := NewSendToSessionTool(nil, n, func(_, m string, o *AgentOrigin) { rec(m, o) }, nil, nil,
				func(string) session.SessionType { return tc.caller })
			params, _ := json.Marshal(map[string]string{"session_key": "clutch/c42", "message": "hello sibling", "reply_to": tc.replyTo})
			if _, err := tool.Execute(WithSessionKey(context.Background(), "clutch/c1"), params); err != nil {
				t.Fatal(err)
			}
			if origin == nil {
				t.Fatal("no delivery")
			}
			if !tc.wantSteer {
				if origin.SteerText != "" {
					t.Errorf("reply_to=caller carries a SteerText: %q", origin.SteerText)
				}
				return
			}
			if !strings.Contains(msg, noResponse) {
				t.Errorf("delivered message lost its %s instruction:\n%s", noResponse, msg)
			}
			if origin.SteerText == "" || strings.Contains(origin.SteerText, noResponse) || !strings.Contains(origin.SteerText, "hello sibling") {
				t.Errorf("SteerText = %q, want the message without %s", origin.SteerText, noResponse)
			}
		})
	}
}
