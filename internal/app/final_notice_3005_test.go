package app

import (
	"context"
	"testing"

	"foci/internal/agent"
	"foci/internal/fap"
	"foci/internal/turnevent"
)

// TestAppSink_FinalNoticeIsSystemMessage (#3005): a turn whose final text is
// foci's notice ("Max tool call depth reached.", TurnComplete.FinalNotice)
// shows in the app as a system message, never as the agent's reply.
func TestAppSink_FinalNoticeIsSystemMessage(t *testing.T) {
	h := newTestHub()
	b := h.ensureBinding(nil, "arnix", "conv-1")
	client := fakeClient()
	b.attach(client)

	conn := &appConn{hub: h, agentID: "arnix"}
	sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: b.key()})
	if sink == nil {
		t.Fatal("NewTurnSink returned a nil sink for a live binding")
	}
	defer cleanup()

	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "Max tool call depth reached.", FinalNotice: true})

	var msgs []decoded
	for _, f := range drain(t, client) {
		if f.t == fap.TypeMessage || f.t == fap.TypeTextEnd {
			msgs = append(msgs, f)
		}
	}
	if len(msgs) != 1 || msgs[0].t != fap.TypeMessage {
		t.Fatalf("text frames = %v, want exactly one message", types(msgs))
	}
	if msgs[0].d["role"] != "system" || msgs[0].d["text"] != "Max tool call depth reached." {
		t.Errorf("message = %v, want the notice with role system", msgs[0].d)
	}
}

// TestAppSink_FinalNoticeAfterStreamedReply (#3005): a turn that already
// delivered text and then hits the notice still shows the notice, as a system
// message after the reply; the reply keeps the agent role.
func TestAppSink_FinalNoticeAfterStreamedReply(t *testing.T) {
	h := newTestHub()
	b := h.ensureBinding(nil, "arnix", "conv-1")
	client := fakeClient()
	b.attach(client)

	conn := &appConn{hub: h, agentID: "arnix"}
	sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: b.key()})
	defer cleanup()

	ctx := context.Background()
	sink.Emit(ctx, turnevent.TextBlock{Text: "working on it", Phase: turnevent.PhaseIntermediate})
	sink.Emit(ctx, turnevent.TurnComplete{FinalText: "Max tool call depth reached.", FinalNotice: true})

	var roles, texts []string
	for _, f := range drain(t, client) {
		if f.t != fap.TypeMessage {
			continue
		}
		r, _ := f.d["role"].(string)
		x, _ := f.d["text"].(string)
		roles, texts = append(roles, r), append(texts, x)
	}
	if len(texts) != 2 || texts[0] != "working on it" || roles[0] != "agent" ||
		texts[1] != "Max tool call depth reached." || roles[1] != "system" {
		t.Errorf("messages = %q roles %q, want the agent reply then the system notice", texts, roles)
	}
}

// Characterisation: an ordinary final text (FinalNotice false) is still the
// agent's reply.
func TestAppSink_FinalTextWithoutNoticeIsAgent(t *testing.T) {
	h := newTestHub()
	b := h.ensureBinding(nil, "arnix", "conv-1")
	client := fakeClient()
	b.attach(client)

	conn := &appConn{hub: h, agentID: "arnix"}
	sink, cleanup := conn.NewTurnSink(agent.Envelope{SessionKey: b.key()})
	defer cleanup()

	sink.Emit(context.Background(), turnevent.TurnComplete{FinalText: "a reply"})

	for _, f := range drain(t, client) {
		if f.t == fap.TypeMessage && f.d["role"] != "agent" {
			t.Errorf("message = %v, want role agent", f.d)
		}
	}
}
