package app

import (
	"context"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/platform"
)

// rewritingAgent is a fakeAgent whose message transforms map whole texts, like
// a [[message_transforms]] rule ("st" → "/stop").
type rewritingAgent struct {
	fakeAgent
	rules map[string]string
}

func (r *rewritingAgent) TransformMessage(t string) string {
	if out, ok := r.rules[t]; ok {
		return out
	}
	return t
}

// #2830: a message the app sent as text ("st") that a transform turns into a
// command ("/stop") must be echoed under the inbound message id. The sending
// device keyed its optimistic bubble by that id, so the echo replaces the
// bubble's text with the rewritten command. A fresh id left the original "st"
// bubble on screen next to a second "/stop" bubble.
func TestRouteUserTurn_TransformedCommandEchoesUnderInboundID(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		rules      map[string]string
	}{
		{"transform", "st", map[string]string{"st": "/stop"}},
		// A command typed as message text (not sent as a Command frame) also
		// has an optimistic bubble under the inbound id.
		{"typed", "/stop", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHub()
			ran := make(chan struct{}, 1)
			reg := command.NewRegistry()
			reg.Register(&command.Command{
				Name: "stop",
				Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
					ran <- struct{}{}
					return command.Response{}, nil
				},
			})
			ra := &rewritingAgent{rules: tc.rules}
			h.agents["ag"] = &appConn{hub: h, agentID: "ag", agentRef: ra, commands: reg}
			h.agentOrder = append(h.agentOrder, "ag")
			c := fakeClient()
			c.hub = h

			h.routeUserTurn(c, "c1", "ag", tc.text, nil, nil, "env-1", 1, platform.SteerDefault, false, false)

			select {
			case <-ran:
			case <-time.After(10 * time.Second):
				t.Fatal("stop command never dispatched")
			}
			if ra.env != nil {
				t.Fatalf("a command must not be enqueued as a turn, got %q", ra.env.Text)
			}
			var users []decoded
			for _, d := range drain(t, c) {
				if d.d["role"] == "user" {
					users = append(users, d)
				}
			}
			if len(users) != 1 {
				t.Fatalf("want exactly one user echo, got %v", users)
			}
			if users[0].d["text"] != "/stop" {
				t.Errorf("echo text = %v, want the rewritten /stop", users[0].d["text"])
			}
			if users[0].d["messageId"] != "env-1" {
				t.Errorf("echo messageId = %v, want the inbound id env-1 so the client replaces its bubble", users[0].d["messageId"])
			}
		})
	}
}
